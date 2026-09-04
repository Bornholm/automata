package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/media"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/observability"
)

// ErrMaxDelegationsReached est retournée par OrchestratorAgent.Execute
// lorsque le plafond d'itérations de la boucle d'appel d'outils
// (agentCfg.Limits.MaxSequentialToolCalls) est atteint sans que le modèle
// n'ait produit de réponse finale sans tool-call.
//
// Décision de conception (plan de conception, Phase 8, "détecter les boucles et
// plafonner les délégations") : on n'invente jamais une réponse à partir
// d'un tour de boucle interrompu — le même principe que ErrEmptyReply
// (agent.go) s'applique ici. Une erreur explicite laisse l'appelant
// (internal/conversation.Handler) traiter l'échec comme n'importe quelle
// autre erreur d'agent, plutôt que de risquer d'envoyer à l'utilisateur une
// réponse tronquée qui semblerait délibérée.
var ErrMaxDelegationsReached = errors.New("agent: plafond de délégations atteint sans réponse finale du modèle")

// OrchestratorAgent est un agent généraliste capable de déléguer à des
// sous-agents spécialistes, exposés comme des outils LLM (plan de conception, §6.1,
// §6.4, Phase 8). Contrairement à GenAIAgent (streaming, sans outil), il
// utilise une complétion non-streaming : l'API GenAI expose les tool-calls
// demandés par le modèle directement sur ChatCompletionResponse.ToolCalls()
// (llm/chat_completion.go), ce qui rend la boucle "appeler -> exécuter les
// outils -> renvoyer les résultats -> rappeler" nettement plus simple qu'en
// reconstruisant des deltas de tool-calls depuis un flux de chunks
// (llm.ToolCallDelta existe côté streaming, mais reconstruire des appels
// d'outils complets à partir de deltas incrémentaux n'apporterait ici aucun
// bénéfice utilisateur, puisque les outils de délégation ne sont pas
// affichés en direct). Voir docs/integration-inventory.md §2 "Tool
// calling".
type OrchestratorAgent struct {
	client                 llm.ChatCompletionClient
	systemPrompt           string
	orgPrompts             map[string]string
	textOnly               bool
	agentName              string
	specialists            map[string]delegation.Specialist
	specialistDescriptions map[string]string
	// pluginProvider fournit les sous-agents des plugins actifs pour
	// l'organisation du tour ; nil quand le système de plugins est
	// désactivé.
	pluginProvider PluginSpecialistProvider
	// skills fournit le catalogue des compétences et l'outil load_skill ;
	// nil quand aucune bibliothèque n'est câblée.
	skills                 SkillsProvider
	maxSequentialToolCalls int
	maxActionsPerTurn      int
	maxToolContextBytes    int64
	memoryTools            MemoryTools
	reminderTools          ReminderTools
	profileTools           ProfileTools
	// customization, s'il est renseigné, adapte le tour à l'organisation
	// (prompt additionnel, spécialistes retirés, plafond d'outils).
	customization OrgCustomizer
	metrics       *observability.Metrics
	logger        *slog.Logger
	// binding permet de servir un modèle différent selon l'organisation,
	// résolu à chaque exécution.
	binding clientBinding
	// judge relit les réponses produites sans aucun appel d'outil ; nil
	// quand aucun modèle n'est affecté au rôle « judge » (voir judge.go).
	judge Judge
	// judgeAttempts plafonne les appels au juge pour UN tour. <= 0 retombe
	// sur defaultJudgeAttempts.
	judgeAttempts int
}

// WithJudge câble le juge des réponses sans appel d'outil. Nil désactive la
// vérification : c'est la valeur par défaut, et une instance sans modèle
// affecté au rôle fonctionne exactement comme avant.
func (a *OrchestratorAgent) WithJudge(judge Judge) *OrchestratorAgent {
	a.judge = judge
	return a
}

// WithJudgeAttempts fixe le nombre d'appels au juge avant de renoncer au
// tour (agents.<nom>.limits.judge_attempts). <= 0 laisse le défaut.
func (a *OrchestratorAgent) WithJudgeAttempts(attempts int) *OrchestratorAgent {
	a.judgeAttempts = attempts
	return a
}

// NewOrchestratorAgent construit un OrchestratorAgent. specialists associe
// chaque nom de délégué (tel que déclaré dans agentCfg.Delegates) au
// delegation.Specialist correspondant ; un outil "delegate_to_<nom>" est
// exposé au modèle pour chaque entrée. maxSequentialToolCalls plafonne le
// nombre d'itérations de la boucle d'appel d'outils (§6.4) ; une valeur <= 0
// retombe sur 1 (au moins un appel de complétion, jamais de boucle
// illimitée).
func NewOrchestratorAgent(client llm.ChatCompletionClient, systemPrompt, agentName string, specialists map[string]delegation.Specialist, maxSequentialToolCalls int) *OrchestratorAgent {
	return &OrchestratorAgent{
		client:                 client,
		systemPrompt:           systemPrompt,
		agentName:              agentName,
		specialists:            specialists,
		maxSequentialToolCalls: maxSequentialToolCalls,
	}
}

// Execute implémente Agent. La boucle d'appel d'outils est strictement
// séquentielle (plan de conception, §6.4, "l'exécution initiale sera séquentielle") :
// lorsqu'un tour du modèle demande plusieurs tool-calls, ils sont exécutés
// un par un, dans l'ordre reçu, avant le tour suivant. La mécanique de
// boucle elle-même est factorisée dans runToolLoop (toolloop.go), partagée
// avec MCPToolAgent (Phase 12) : voir son commentaire de package.
func (a *OrchestratorAgent) Execute(ctx context.Context, req Request) (Result, error) {
	ctx = withUsageAttribution(ctx, req.Identity, a.agentName)

	// Modèle du tour : résolu par le catalogue (défaut d'instance,
	// surchargé par organisation). Le client du constructeur ne sert plus
	// qu'aux tests, qui n'ont pas de résolveur.
	client, textOnly := a.client, a.textOnly
	var modelName string
	if resolved, resolveErr := a.binding.resolve(ctx, req.Identity.OrgID); resolveErr == nil {
		client, textOnly, modelName = resolved.Client, !resolved.SupportsVision, resolved.Model
	} else if !errors.Is(resolveErr, errNoResolver) {
		return Result{}, fmt.Errorf("agent %q: %w", a.agentName, resolveErr)
	}
	if client == nil {
		return Result{}, fmt.Errorf("agent %q: no model is configured for this agent — set an instance default in the administration (Modèles)", a.agentName)
	}

	collector := newProposalCollector()
	mediaCollector := newMediaCollector()

	// Personnalisation de l'organisation : elle retire des spécialistes,
	// ajoute une consigne et resserre le plafond d'outils. Elle ne peut
	// jamais accorder ce que la configuration n'a pas prévu — seulement
	// restreindre ou préciser.
	custom := a.orgCustomization(ctx, req.Identity)

	// Fichiers déjà reçus dans la conversation : ils ne partent qu'aux
	// délégués capables de les manipuler, et jamais au modèle.
	recentFiles := recentToolFiles(req.History, req.Attachments)

	// Un spécialiste capable de fichiers peut venir de la configuration ou
	// d'un plugin actif pour l'organisation : les deux sources comptent.
	fileCapable := hasFileCapableSpecialist(a.specialists)

	// Un spécialiste qui ne travaille que sur pièce jointe (la vision) peut
	// se replier sur les images des messages précédents : voir
	// newDelegationTool.
	attachmentDependent := hasAttachmentDependentSpecialist(a.specialists)

	tools := a.buildDelegationTools(req.Identity, req.Attachments, recentFiles, collector, mediaCollector)
	if len(custom.DisabledAgents) > 0 {
		tools = filterDelegationTools(tools, custom.DisabledAgents)
	}
	// Sous-agents des plugins actifs pour l'organisation du tour :
	// l'activation est relue à CHAQUE tour (comme la personnalisation),
	// une désactivation s'applique donc au message suivant, sans
	// redémarrage. La personnalisation ne filtre pas ces outils : c'est
	// l'activation elle-même qui accorde ou retire.
	if a.pluginProvider != nil {
		pluginSpecs, pluginDescs := a.pluginProvider.SpecialistsFor(ctx, req.Identity)
		for name, specialist := range pluginSpecs {
			tools = append(tools, newDelegationTool(name, pluginDescs[name], specialist, req.Identity, req.Attachments, recentFiles, collector, mediaCollector, a.metrics, a.logger))
		}
		fileCapable = fileCapable || hasFileCapableSpecialist(pluginSpecs)
		attachmentDependent = attachmentDependent || hasAttachmentDependentSpecialist(pluginSpecs)
	}
	tools = append(tools, a.memoryTools.buildMemoryTools(req.Identity, collector)...)
	tools = append(tools, a.reminderTools.buildReminderTools(req.Identity)...)
	tools = append(tools, a.profileTools.buildProfileTools(req.Identity)...)

	// Rappel automatique de souvenirs (memory.recall) : recherche mémoire
	// sur le message entrant, injectée dans le message système. Jamais
	// bloquant : une mémoire indisponible donne un tour sans rappel, pas un
	// tour en échec.
	recallNote := a.memoryTools.recallNote(ctx, req.Identity, req.Input)

	systemPrompt := resolveSystemPrompt(a.systemPrompt, a.orgPrompts, req.Identity.OrgID)
	if custom.PromptExtra != "" {
		systemPrompt += "\n\n---\n\n" + custom.PromptExtra
	}

	// Catalogue des compétences et outil load_skill : relus à CHAQUE tour,
	// comme les activations de plugins — une compétence désactivée dans
	// l'administration s'applique au message suivant, sans redémarrage.
	systemPrompt, tools = appendSkills(ctx, a.skills, a.agentName, systemPrompt, tools)

	// L'introspection capture les outils du tour APRÈS leur assemblage :
	// son instantané reflète exactement ce que ce tour offre, permissions
	// comprises.
	tools = append(tools, newDescribeCapabilitiesTool(tools, a.specialists, a.specialistDescriptions, req.Identity))
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name() < tools[j].Name() })

	// L'orchestrateur ne voit que le message courant : sans cette note, une
	// demande qui renvoie à un fichier envoyé plus tôt lui paraît sans
	// objet, et il répond « je n'ai rien reçu » au lieu de déléguer.
	// N'annoncer que ce qu'un spécialiste peut réellement traiter : tous
	// les fichiers s'il en existe un capable de fichiers, sinon les seules
	// images visibles qu'un spécialiste dépendant saura regarder.
	if announced := delegableFiles(recentFiles, fileCapable, attachmentDependent); len(announced) > 0 {
		req.Input += media.DelegableFilesNotice(announced)
	}

	messages := buildChatMessages(systemPrompt, a.agentName, textOnly, recallNote, req)

	maxIterations := a.maxSequentialToolCalls
	if maxIterations <= 0 {
		maxIterations = 1
	}
	if custom.MaxToolCalls > 0 && custom.MaxToolCalls < maxIterations {
		maxIterations = custom.MaxToolCalls
	}

	loopResult, err := runToolLoop(ctx, client, messages, tools, maxIterations, a.maxToolContextBytes, ErrMaxDelegationsReached, a.logger, a.agentName, modelName)
	if err != nil {
		return Result{}, err
	}

	// Deux relectures, dans cet ordre. La première décide sur une PREUVE :
	// le tour n'a appelé aucun outil, et le juge dit si la réponse affirme
	// pourtant quelque chose (voir judge.go). La seconde décide sur un
	// FAIT : une adresse que rien n'a fournie au modèle a été composée par
	// lui (voir unsourced_url.go). Chacune rend le tour au modèle une fois,
	// ses outils toujours offerts ; aucune ne réécrit sa réponse.
	loopResult, err = a.reviewGrounding(ctx, client, req.Identity, messages, tools, maxIterations, req.Input, loopResult)
	if err != nil {
		return Result{}, err
	}
	loopResult = a.verifyURLSources(ctx, client, messages, tools, maxIterations, loopResult)

	proposals := collector.take()

	// Plafond d'actions par tour (plan de conception, §9.4, agents.<nom>.limits.
	// max_actions_per_turn). Le dépassement rejette le lot ENTIER plutôt que
	// d'en conserver les N premières : ces actions sont sensibles et
	// destinées à une confirmation groupée, or n'en garder qu'un préfixe
	// arbitraire ferait confirmer à l'utilisateur autre chose que ce que
	// l'agent a annoncé. Le texte de réponse, lui, est conservé : il porte le
	// raisonnement qui aide à reformuler.
	if a.maxActionsPerTurn > 0 && len(proposals) > a.maxActionsPerTurn {
		notice := fmt.Sprintf(
			"\n\n⚠️ Ce tour a produit %d actions à confirmer, au-delà de la limite de %d configurée pour l'agent %q. Aucune action n'a été enregistrée : reformulez votre demande en la découpant en plusieurs étapes.",
			len(proposals), a.maxActionsPerTurn, a.agentName,
		)

		return Result{
			Reply:                loopResult.Text + notice,
			Attachments:          a.collectedMedia(loopResult, mediaCollector),
			AnsweredWithoutTools: answeredWithoutTools(tools, loopResult),
		}, nil
	}

	return Result{
		Reply:                loopResult.Text,
		ProposedActions:      proposals,
		Attachments:          a.collectedMedia(loopResult, mediaCollector),
		AnsweredWithoutTools: answeredWithoutTools(tools, loopResult),
	}, nil
}

// answeredWithoutTools dit si le tour avait des outils et n'en a appelé
// aucun. Les deux conditions comptent : sans outils offerts, ne rien
// appeler est le fonctionnement normal et n'apprend rien.
func answeredWithoutTools(tools []llm.Tool, loopResult toolLoopResult) bool {
	return len(tools) > 0 && loopResult.ToolCalls == 0
}

// collectedMedia agrège les médias produits durant le tour : ceux des outils
// appelés directement par l'orchestrateur (mémoire) et ceux remontés par les
// spécialistes délégués.
func (a *OrchestratorAgent) collectedMedia(loopResult toolLoopResult, mediaCollector *mediaCollector) []media.Media {
	return append(append([]media.Media(nil), loopResult.Attachments...), mediaCollector.take()...)
}

// WithVision déclare si le modèle du client accepte les images en entrée.
// À false, aucune pièce jointe ne part vers le modèle — les délégations,
// elles, continuent de les transporter. Retourne a pour permettre le
// chaînage.
func (a *OrchestratorAgent) WithVision(enabled bool) *OrchestratorAgent {
	a.textOnly = !enabled
	return a
}

// WithClientResolver permet de servir à cet agent un modèle différent selon
// l'organisation. role est le nom sous lequel le catalogue le connaît
// (typiquement le nom de l'agent). Retourne a pour permettre le chaînage.
func (a *OrchestratorAgent) WithClientResolver(resolver ClientResolver, role string, logger *slog.Logger) *OrchestratorAgent {
	a.binding.bind(resolver, role, logger)
	return a
}

// WithOrgSystemPrompts remplace le prompt système par organisation : la clé
// est un organizations[].id, la valeur un prompt complet déjà composé (voir
// BuildOrgSystemPrompts). Le prompt du constructeur reste le défaut pour
// toute organisation absente de la map. Retourne a pour permettre le
// chaînage.
func (a *OrchestratorAgent) WithOrgSystemPrompts(prompts map[string]string) *OrchestratorAgent {
	a.orgPrompts = prompts
	return a
}

// WithMaxActionsPerTurn plafonne le nombre d'actions que ce tour peut
// proposer à la confirmation (plan de conception, §9.4). Une valeur <= 0 (défaut) laisse
// le nombre d'actions non borné. Retourne a pour permettre le chaînage.
func (a *OrchestratorAgent) WithMaxActionsPerTurn(max int) *OrchestratorAgent {
	a.maxActionsPerTurn = max
	return a
}

// WithMaxToolContextBytes borne le cumul des résultats d'outils réinjectés
// dans la conversation durant un tour (plan de conception, §9.4). Une valeur <= 0
// (défaut) laisse ce budget illimité. Retourne a pour permettre le chaînage.
func (a *OrchestratorAgent) WithMaxToolContextBytes(max int64) *OrchestratorAgent {
	a.maxToolContextBytes = max
	return a
}

// buildDelegationTools construit un llm.Tool "delegate_to_<agentID>" par
// spécialiste connu. Reconstruit à chaque exécution (plutôt que mémorisé à
// la construction de l'OrchestratorAgent) car chaque outil capture
// l'identité d'exécution propre à la requête courante : l'identité n'est
// jamais décidée par le modèle (InvariantRules, règle 1), seulement
// transmise par l'application.
func (a *OrchestratorAgent) buildDelegationTools(identity model.ExecutionIdentity, attachments, recentFiles []media.Media, collector *proposalCollector, mediaCollector *mediaCollector) []llm.Tool {
	tools := make([]llm.Tool, 0, len(a.specialists))

	for agentID, specialist := range a.specialists {
		tools = append(tools, newDelegationTool(agentID, a.specialistDescriptions[agentID], specialist, identity, attachments, recentFiles, collector, mediaCollector, a.metrics, a.logger))
	}

	// Ordre déterministe : la map d'origine n'a pas d'ordre garanti, et un
	// ordre stable des outils envoyés au modèle facilite le débogage et les
	// tests.
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name() < tools[j].Name() })

	return tools
}

// recentToolFiles rassemble les fichiers déjà reçus dans la conversation,
// de la plus récent au plus ancien.
//
// TOUTES les pièces jointes sont retenues, pas seulement celles réservées
// aux outils : une photo ordinaire est un fichier comme un autre pour qui
// sait l'éditer, et la retenir sur le seul critère « le modèle pourrait la
// voir » condamnait « voici une photo » puis « enlève le logo » à travailler
// sur le mauvais fichier. Rien ne fuit pour autant vers le modèle : ces
// fichiers ne vont qu'aux délégués FileCapable, qui ne reçoivent aucune
// pièce jointe dans leur contexte (voir PluginSubAgent.Execute).
//
// Les pièces du tour courant sont exclues : elles voyagent déjà dans
// Request.Attachments, et un doublon ferait hésiter le délégué entre deux
// entrées de même nom. Le dédoublonnage par nom garde la plus récente,
// pour la même raison — c'est celle que l'utilisateur a en tête.
//
// L'historique est déjà borné en amont (attachments.max_history) : rien
// n'est relu en base ici.
func recentToolFiles(history []Message, current []media.Media) []media.Media {
	seen := make(map[string]struct{}, len(current))
	for _, m := range current {
		seen[m.Filename] = struct{}{}
	}

	var out []media.Media

	for i := len(history) - 1; i >= 0; i-- {
		for _, m := range history[i].Attachments {
			if _, dup := seen[m.Filename]; dup {
				continue
			}
			seen[m.Filename] = struct{}{}
			out = append(out, m)
		}
	}

	return out
}

// delegableFiles retient, parmi les fichiers déjà reçus, ceux qu'au moins
// un spécialiste du tour saura traiter : tous si l'un d'eux manipule des
// fichiers, les seuls visibles du modèle si l'un d'eux dépend d'une pièce
// jointe, aucun sinon.
func delegableFiles(recentFiles []media.Media, fileCapable, attachmentDependent bool) []media.Media {
	switch {
	case fileCapable:
		return recentFiles
	case attachmentDependent:
		return visibleAttachments(recentFiles)
	default:
		return nil
	}
}

// hasAttachmentDependentSpecialist indique si au moins un spécialiste ne
// travaille que sur pièce jointe. C'est à lui que les images des messages
// précédents peuvent encore être soumises.
func hasAttachmentDependentSpecialist(specialists map[string]delegation.Specialist) bool {
	for _, specialist := range specialists {
		if dependent, ok := specialist.(delegation.AttachmentDependent); ok && dependent.RequiresAttachments() {
			return true
		}
	}

	return false
}

// hasFileCapableSpecialist indique si au moins un spécialiste sait
// manipuler des fichiers. Annoncer des fichiers qu'aucun délégué ne peut
// ouvrir ne ferait que promettre à l'utilisateur ce que le tour ne sait pas
// tenir.
func hasFileCapableSpecialist(specialists map[string]delegation.Specialist) bool {
	for _, specialist := range specialists {
		if capable, ok := specialist.(delegation.FileCapable); ok && capable.SupportsFiles() {
			return true
		}
	}

	return false
}

// maxSameAgentDelegations borne le nombre d'appels au MÊME spécialiste
// pendant un tour. Solliciter deux spécialistes différents reste libre :
// c'est la répétition stérile qui est bornée, pas la délégation.
const maxSameAgentDelegations = 2

// newDelegationTool construit l'outil LLM "delegate_to_<agentID>" qui
// adapte les arguments produits par le modèle en delegation.Request, puis
// exécute specialist.Execute. Un échec du spécialiste n'est jamais remonté
// comme erreur Go (ce qui ferait échouer tout le tour) : il est transmis au
// modèle comme contenu de résultat d'outil, en clair, pour qu'il puisse
// s'adapter (plan de conception, Phase 8, test "spécialiste en erreur").
func newDelegationTool(agentID, description string, specialist delegation.Specialist, identity model.ExecutionIdentity, attachments, recentFiles []media.Media, collector *proposalCollector, mediaCollector *mediaCollector, metrics *observability.Metrics, logger *slog.Logger) llm.Tool {
	// Un spécialiste qui ne sait travailler que sur des pièces jointes est
	// refusé quand le tour n'en porte aucune. La question ne lui est même
	// pas posée : sollicité à vide, un modèle multimodal invente ce qu'il
	// aurait vu (voir delegation.AttachmentDependent).
	//
	// Exception : les images des messages précédents, encore rejouées dans
	// l'historique (attachments.max_history). « Voici une carte » puis
	// « c'est Malakoff ? » deux minutes plus tard porte bien une image à
	// examiner ; la refuser condamnait toute question de suivi à un « renvoie
	// la capture ». Vu en production le 2026-09-04. Seules les pièces
	// visibles du modèle se prêtent à ce repli : une vidéo réservée aux
	// outils ne peut pas être regardée par un tel spécialiste.
	dependent := false
	var earlierVisible []media.Media
	if capable, ok := specialist.(delegation.AttachmentDependent); ok && capable.RequiresAttachments() {
		dependent = true
		earlierVisible = visibleAttachments(recentFiles)
	}

	// Les fichiers déjà reçus ne sont proposés qu'aux spécialistes qui
	// déclarent savoir les manipuler : l'orchestrateur interroge une
	// capacité, il ne connaît aucun spécialiste par son nom.
	if capable, ok := specialist.(delegation.FileCapable); !ok || !capable.SupportsFiles() {
		recentFiles = nil
	}

	// Un spécialiste ne garde aucun état d'une délégation à l'autre : le
	// re-solliciter dans le même tour le fait repartir de zéro, pour le
	// même coût. Vu en production, trois délégations d'affilée ont épuisé
	// le délai du pipeline sans rien produire. Passé la limite, l'outil
	// répond sans rien exécuter.
	var delegations atomic.Int32

	schema := llm.NewJSONSchema().
		RequiredProperty("goal", "Precise objective for the specialist to reach.", "string").
		Property("relevant_input", "Context strictly needed for the task, spelled out. Never pass the whole conversation history.", "string").
		Property("constraints", "Additional constraints the specialist must respect.", "array", map[string]any{"type": "string"}, "items")

	// La description du spécialiste (agents.<nom>.description) est ce sur
	// quoi le modèle décide de déléguer ou non : sans elle il ne connaît que
	// le nom du délégué, et un petit modèle préfère répondre qu'il ne sait
	// pas faire plutôt que d'appeler un outil dont il ignore la portée.
	toolDescription := fmt.Sprintf("Delegate a task to the %q specialist and get a summary of its result.", agentID)
	if strings.TrimSpace(description) != "" {
		toolDescription = fmt.Sprintf("Delegate a task to the %q specialist, which %s. Returns a summary of its result.", agentID, strings.TrimSpace(description))
	}

	return llm.NewFuncTool(
		"delegate_to_"+agentID,
		toolDescription,
		schema,
		func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
			goal, _ := params["goal"].(string)
			if strings.TrimSpace(goal) == "" {
				return llm.NewToolResult(fmt.Sprintf("erreur: le paramètre 'goal' est requis et ne peut pas être vide pour déléguer à %q.", agentID)), nil
			}

			relevantInput, _ := params["relevant_input"].(string)

			var constraints []string
			if raw, ok := params["constraints"].([]any); ok {
				for _, c := range raw {
					if s, ok := c.(string); ok {
						constraints = append(constraints, s)
					}
				}
			}

			turnAttachments := attachments
			if dependent && len(turnAttachments) == 0 {
				turnAttachments = earlierVisible
			}

			if dependent && len(turnAttachments) == 0 && len(recentFiles) == 0 {
				if logger != nil {
					logger.InfoContext(ctx, "agent: délégation refusée faute de pièce jointe", "agent", agentID)
				}

				return llm.NewToolResult(fmt.Sprintf(
					"erreur: le spécialiste %q ne peut travailler que sur une image ou un document, et ce tour n'en porte aucun. "+
						"Ne décris rien que tu n'aies pas vu : demande à la personne d'envoyer le fichier, ou réponds à sa question sans lui.",
					agentID)), nil
			}

			if delegations.Add(1) > maxSameAgentDelegations {
				return llm.NewToolResult(fmt.Sprintf(
					"erreur: le spécialiste %q a déjà été sollicité %d fois pendant ce tour et repartirait de zéro. "+
						"Conclus avec ce qu'il t'a déjà rapporté, ou explique à l'utilisateur ce qui n'a pas abouti.",
					agentID, maxSameAgentDelegations)), nil
			}

			metrics.IncDelegation(agentID)

			result, err := specialist.Execute(ctx, delegation.Request{
				AgentID:       agentID,
				Goal:          goal,
				RelevantInput: relevantInput,
				Constraints:   constraints,
				Identity:      identity,
				// Les pièces jointes du tour accompagnent toujours la
				// délégation : le modèle ne peut pas les recopier dans
				// relevant_input (voir delegation.Request.Attachments). Un
				// spécialiste dépendant reçoit à leur place, faute de
				// mieux, les images des messages précédents.
				Attachments: turnAttachments,
				// Fichiers des messages précédents : invisibles du modèle,
				// atteignables par les seuls outils fichiers du délégué.
				RecentAttachments: recentFiles,
			})
			if err != nil {
				// L'échec part au modèle sous forme de texte : sans cette
				// trace, il ne subsisterait AUCUNE trace technique de la
				// panne, le tour se terminant par un « tour terminé » normal
				// après que le modèle a reformulé l'erreur à sa façon.
				if logger != nil {
					logger.WarnContext(ctx, "agent: spécialiste en échec", "agent", agentID, "error", err)
				}

				return llm.NewToolResult(fmt.Sprintf("le spécialiste %q a échoué: %v", agentID, err)), nil
			}

			for _, pa := range result.ProposedActions {
				collector.add(pa)
			}

			// Les médias produits par le spécialiste remontent jusqu'à la
			// réponse envoyée à l'utilisateur, sans passer par le texte du
			// résumé (que le modèle réécrit librement).
			mediaCollector.add(result.Attachments...)

			return llm.NewToolResult(result.Summary), nil
		},
	)
}

// WithMemoryTools attache tools à a : les outils search_memory/remember/
// forget_memory correspondants (selon tools.Search/Remember/Forget) sont
// exposés au modèle en plus des délégations, dès le prochain Execute
// (plan de conception, §6.1, §8, Phase 10). Retourne a pour permettre le chaînage à la
// construction (voir internal/agent.NewRegistryWithMemory). Un appel avec la
// valeur zéro de MemoryTools désactive tous les outils mémoire, ce qui est
// le comportement par défaut d'un OrchestratorAgent tout juste construit.
func (a *OrchestratorAgent) WithMemoryTools(tools MemoryTools) *OrchestratorAgent {
	a.memoryTools = tools
	return a
}

// WithReminderTools attache tools à a : les outils create_reminder/
// list_reminders/cancel_reminder sont exposés au modèle en plus des
// délégations, dès le prochain Execute. Un appel avec la valeur zéro de
// ReminderTools désactive tous les outils de rappels (comportement par
// défaut d'un OrchestratorAgent tout juste construit). Retourne a pour
// permettre le chaînage à la construction, comme WithMemoryTools.
func (a *OrchestratorAgent) WithReminderTools(tools ReminderTools) *OrchestratorAgent {
	a.reminderTools = tools
	return a
}

// WithOrgCustomizer attache la personnalisation par organisation. Sans
// elle, tous les tours suivent la configuration de l'instance.
func (a *OrchestratorAgent) WithOrgCustomizer(customizer OrgCustomizer) *OrchestratorAgent {
	a.customization = customizer
	return a
}

// orgCustomization lit la personnalisation applicable au tour. Jamais
// bloquant : une lecture en échec donne un tour standard, pas un tour
// raté.
func (a *OrchestratorAgent) orgCustomization(ctx context.Context, identity model.ExecutionIdentity) OrgCustomization {
	if a.customization == nil {
		return OrgCustomization{}
	}

	custom, err := a.customization.CustomizationFor(ctx, string(identity.OrgID))
	if err != nil {
		if a.logger != nil {
			a.logger.WarnContext(ctx, "agent: personnalisation d'organisation illisible, réglages par défaut",
				"agent", a.agentName, "org_id", identity.OrgID, "error", err)
		}
		return OrgCustomization{}
	}

	return custom
}

// WithProfileTools attache tools à a : l'outil open_profile_link est
// exposé au modèle dès le prochain Execute. Même contrat que
// WithMemoryTools et WithReminderTools — la valeur zéro le désactive.
func (a *OrchestratorAgent) WithProfileTools(tools ProfileTools) *OrchestratorAgent {
	a.profileTools = tools
	return a
}

// WithPluginSpecialists branche les sous-agents fournis par les plugins.
// Nil-safe : sans provider, aucun outil de plugin n'est jamais exposé.
func (a *OrchestratorAgent) WithPluginSpecialists(provider PluginSpecialistProvider) *OrchestratorAgent {
	a.pluginProvider = provider
	return a
}

// WithSkills branche la bibliothèque de compétences. Nil-safe : sans
// provider, ni catalogue ni load_skill ne sont exposés — et un catalogue
// vide ne coûte rien non plus.
func (a *OrchestratorAgent) WithSkills(provider SkillsProvider) *OrchestratorAgent {
	a.skills = provider
	return a
}

// WithSpecialistDescriptions renseigne, par nom de délégué, la phrase
// décrivant ce qu'il sait faire (agents.<nom>.description). Elle est reprise
// dans la description de l'outil delegate_to_<nom>. Un nom absent de la map
// garde la description générique.
func (a *OrchestratorAgent) WithSpecialistDescriptions(descriptions map[string]string) *OrchestratorAgent {
	a.specialistDescriptions = descriptions
	return a
}

// WithLogger attache logger à a : chaque tour journalise alors son
// introspection (outils exposés, appels d'outils avec durée, fin de tour) —
// jamais les arguments ni les résultats, qui portent du contenu privé. nil
// (défaut) désactive cette journalisation.
func (a *OrchestratorAgent) WithLogger(logger *slog.Logger) *OrchestratorAgent {
	a.logger = logger
	return a
}

// WithMetrics attache metrics à a : les délégations vers chaque spécialiste
// (outil "delegate_to_<agentID>") sont comptabilisées dès le prochain
// Execute (plan de conception, §14.3, Phase 20). metrics nil désactive l'observation
// (comportement par défaut d'un OrchestratorAgent tout juste construit).
// Retourne a pour permettre le chaînage à la construction, comme
// WithMemoryTools.
func (a *OrchestratorAgent) WithMetrics(metrics *observability.Metrics) *OrchestratorAgent {
	a.metrics = metrics
	return a
}

var _ Agent = &OrchestratorAgent{}
