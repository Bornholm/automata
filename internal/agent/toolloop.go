package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/media"
)

// proposalCollector accumule les delegation.ProposedAction produites par
// les outils exécutés durant un tour de runToolLoop (PLAN.md §10, Phase
// 15). C'est le canal choisi pour faire remonter une proposition depuis
// l'exécution d'un outil LLM jusqu'à Agent.Result, sans changer la
// signature de llm.Tool.Execute (context.Context, map[string]any) ni celle
// de runToolLoop : un outil qui produit une proposition (voir
// MemoryTools.newForgetMemoryTool) capture le collector correspondant à
// l'exécution courante dans sa closure et y ajoute l'action, en plus de
// retourner un texte lisible au modèle. Protégé par un mutex bien que
// runToolLoop exécute les outils strictement séquentiellement (jamais deux
// exécutions concurrentes pour un même tour) : un canal explicitement
// thread-safe est plus robuste qu'une hypothèse implicite sur l'ordre
// d'exécution qui pourrait changer sans que ce fichier soit relu.
type proposalCollector struct {
	mu        sync.Mutex
	proposals []delegation.ProposedAction
}

func newProposalCollector() *proposalCollector {
	return &proposalCollector{}
}

// add ajoute p aux propositions accumulées. Sûr à appeler avec un récepteur
// nil (aucun collector actif) : ne fait rien.
func (c *proposalCollector) add(p delegation.ProposedAction) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.proposals = append(c.proposals, p)
}

// take retourne les propositions accumulées. Sûr à appeler avec un
// récepteur nil (retourne nil).
func (c *proposalCollector) take() []delegation.ProposedAction {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.proposals
}

// mediaCollector accumule les médias produits par les spécialistes délégués
// durant un tour, pour les joindre à la réponse envoyée à l'utilisateur.
//
// Il double proposalCollector plutôt que de l'étendre : une action proposée
// et un média produit n'ont ni le même cycle de vie (l'une attend une
// confirmation, l'autre part immédiatement) ni le même destinataire.
// Thread-safe pour la même raison que proposalCollector.
type mediaCollector struct {
	mu     sync.Mutex
	medias []media.Media
}

func newMediaCollector() *mediaCollector {
	return &mediaCollector{}
}

// add ajoute medias aux médias accumulés. Sûr sur un récepteur nil.
func (c *mediaCollector) add(medias ...media.Media) {
	if c == nil || len(medias) == 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.medias = append(c.medias, medias...)
}

// mediaCollectorContextKey est un type dédié pour éviter toute collision de
// clé de contexte avec d'autres packages.
type mediaCollectorContextKey struct{}

// withMediaCollector attache collector à ctx.
//
// Le collecteur voyage par le contexte, et non par la fermeture de l'outil,
// parce que les outils d'un agent sont construits UNE fois au démarrage et
// partagés par toutes les requêtes : un collecteur capturé à la
// construction mélangerait les médias de conversations concurrentes.
func withMediaCollector(ctx context.Context, collector *mediaCollector) context.Context {
	return context.WithValue(ctx, mediaCollectorContextKey{}, collector)
}

// mediaCollectorFromContext extrait le collecteur de médias de ctx.
func mediaCollectorFromContext(ctx context.Context) (*mediaCollector, bool) {
	collector, ok := ctx.Value(mediaCollectorContextKey{}).(*mediaCollector)
	return collector, ok && collector != nil
}

// take retourne les médias accumulés. Sûr sur un récepteur nil.
func (c *mediaCollector) take() []media.Media {
	if c == nil {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	return c.medias
}

// toolLoopResult est le résultat d'un appel à runToolLoop : le texte de la
// réponse finale du modèle (sans tool-call), et le contenu textuel de
// chaque résultat d'outil exécuté durant le tour, dans l'ordre d'exécution
// (utilisé par MCPToolAgent pour en extraire des références, voir
// mcp_tool_agent.go).
type toolLoopResult struct {
	Text        string
	ToolResults []string
	// Attachments porte les médias produits par les outils durant le tour
	// (un serveur MCP peut joindre une image ou un document à son résultat),
	// dans l'ordre d'exécution, afin d'être renvoyés à l'utilisateur.
	Attachments []media.Media
}

// runToolLoop factorise la mécanique "appeler le LLM avec des tools ->
// exécuter les tool-calls -> renvoyer les résultats -> rappeler ->
// plafonner", partagée par OrchestratorAgent (délégation, Phase 8) et
// MCPToolAgent (outils MCP réels, Phase 12) : les deux ont besoin
// exactement de la même boucle de tool-calling non-streaming, seuls les
// outils exposés au modèle diffèrent.
//
// messages doit déjà contenir le system prompt (avec bloc de contexte) et
// l'historique de la requête courante (voir buildChatMessages) : runToolLoop
// n'en construit aucun lui-même. maxIterations <= 0 retombe sur 1 (au moins
// un appel de complétion, jamais de boucle illimitée). maxReachedErr est
// retournée telle quelle si maxIterations tours se sont écoulés sans réponse
// finale du modèle : chaque appelant fournit son propre sentinel (voir
// ErrMaxDelegationsReached, ErrMaxToolCallsReached) pour que
// errors.Is continue de distinguer les deux origines côté appelant.
//
// forceAnswerInstruction est injecté dans l'historique lorsque la boucle
// d'outils se clôt sans réponse finale (plafond d'appels atteint, ou modèle
// muet). Plutôt que d'interdire les outils au niveau du protocole
// (ToolChoiceNone) — ce que certains modèles traduisent en balisage d'appel
// d'outil au format texte qui fuite dans la réponse — on demande d'abord
// explicitement au modèle de conclure avec ce qu'il a. Mécanisme et
// formulation repris d'edecan (internal/infra/llm/agent.go) : la
// formulation est délibérément restrictive, car « réponds avec ce que tu
// as » est, sur une demande précise, une incitation directe à combler le
// manque en fabriquant une réponse plausible — d'autant plus que le plafond
// est atteint précisément quand les outils n'ont pas trouvé. L'aveu d'échec
// est donc présenté comme la conclusion attendue, pas comme un repli.
const forceAnswerInstruction = `You have reached the limit of available tool calls. Do not call any further tool. Conclude now from the tool results and information already available in this conversation. State no fact that does not appear in what you have: do not fill gaps, do not reconstruct, and do not infer what the tools did not return. If what you were asked for is not there, answer that you did not find it and state what you searched for — that is a valid answer, and preferable to a plausible but unverifiable one. An unsuccessful search does not prove that what is sought does not exist: never conclude that something does not exist. Write your reply now, addressed directly to the user: this is your final turn, and what you write is shown to them as-is. Never return an empty reply — even "I did not find X, here is what I found instead" is a useful answer.`

// Étiquettes des tentatives de conclusion, journalisées pour distinguer en
// production quel repli a fini par produire une réponse.
const (
	attemptTextualInstruction = "consigne textuelle"
	attemptToolsForbidden     = "outils interdits"
	attemptReasoningDisabled  = "raisonnement désactivé"
)

// conclusionAttempt associe une variante d'options de complétion à son
// étiquette de journal.
type conclusionAttempt struct {
	label string
	opts  []llm.ChatCompletionOptionFunc
}

// conclusionAttempts construit les variantes de la complétion de conclusion,
// dans l'ordre du cas nominal (repris d'edecan) :
//
//  1. consigne textuelle seule : le cas le plus fréquent — interdire les
//     outils au niveau du protocole dès ce stade ferait fuiter du balisage
//     d'appel d'outil dans le texte chez certains modèles ;
//  2. outils interdits : le modèle a ignoré la consigne et redemandé un
//     outil — il n'est pas muet, il n'a pas essayé de répondre ;
//  3. raisonnement désactivé : le modèle a rédigé sa réponse dans son canal
//     de raisonnement et laissé le contenu vide. Lui retirer ce canal est
//     alors le seul moyen d'obtenir du texte.
//
// Chaque variante coûte une complétion : orderConclusionFallbacks replace en
// tête des variantes restantes celle que le symptôme désigne.
func conclusionAttempts(messages []llm.Message, tools []llm.Tool) []conclusionAttempt {
	base := []llm.ChatCompletionOptionFunc{
		llm.WithMessages(messages...),
		llm.WithTools(tools...),
	}
	with := func(extra ...llm.ChatCompletionOptionFunc) []llm.ChatCompletionOptionFunc {
		opts := make([]llm.ChatCompletionOptionFunc, 0, len(base)+len(extra))
		opts = append(opts, base...)
		return append(opts, extra...)
	}

	disabled := false
	return []conclusionAttempt{
		{attemptTextualInstruction, with(llm.WithToolChoice(llm.ToolChoiceAuto))},
		{attemptToolsForbidden, with(llm.WithToolChoice(llm.ToolChoiceNone))},
		// WithReasoning par appel garde le dernier mot sur le réglage du
		// client (voir reasoningClient.ChatCompletion).
		{attemptReasoningDisabled, with(llm.WithToolChoice(llm.ToolChoiceNone), llm.WithReasoning(&llm.ReasoningOptions{Enabled: &disabled}))},
	}
}

// orderConclusionFallbacks remonte en tête des variantes restantes celle que
// le symptôme désigne : un appel d'outil trahit un refus de conclure, une
// réponse vide sans appel d'outil trahit un canal de raisonnement qui a
// absorbé la réponse. Les autres variantes restent tentées ensuite.
func orderConclusionFallbacks(rest []conclusionAttempt, lastHadToolCalls bool) {
	want := attemptReasoningDisabled
	if lastHadToolCalls {
		want = attemptToolsForbidden
	}
	for i := range rest {
		if rest[i].label == want {
			rest[0], rest[i] = rest[i], rest[0]
			return
		}
	}
}

// concludeToolLoop tente d'obtenir une réponse finale après la clôture de la
// boucle sans réponse : la matière déjà rapportée par les outils est le coût
// déjà payé du tour, la jeter serait le pire résultat possible. Une consigne
// de conclusion est injectée dans l'historique, puis chaque variante de
// complétion est tentée jusqu'à obtenir un texte non vide. fallbackErr est
// retournée telle quelle si aucune variante n'aboutit, préservant le contrat
// historique des appelants (ErrMaxToolCallsReached, ErrMaxDelegationsReached,
// ErrEmptyReply).
func concludeToolLoop(ctx context.Context, client llm.ChatCompletionClient, messages []llm.Message, tools []llm.Tool, closeReason string, fallbackErr error, iterations, totalCalls int, toolResults []string, attachments []media.Media, logger *slog.Logger, agentName string, lastHadToolCalls bool) (toolLoopResult, error) {
	if logger != nil {
		logger.WarnContext(ctx, "agent: boucle d'outils close sans réponse, consigne de conclusion injectée",
			"agent", agentName, "reason", closeReason, "iterations", iterations, "tool_calls", totalCalls)
	}

	messages = append(messages, llm.NewMessage(llm.RoleUser, forceAnswerInstruction))

	attempts := conclusionAttempts(messages, tools)
	orderConclusionFallbacks(attempts[1:], lastHadToolCalls)

	// Conservée pour être rapportée si AUCUNE variante n'aboutit : sans
	// elle, l'appelant ne verrait qu'un fallbackErr générique, sans la
	// cause réelle.
	var lastErr error

	for len(attempts) > 0 {
		attempt := attempts[0]
		attempts = attempts[1:]

		resp, err := client.ChatCompletion(ctx, attempt.opts...)
		if err != nil {
			// Une variante en échec n'est pas la fin : c'est précisément
			// ce que les suivantes existent pour rattraper. Un modèle à
			// raisonnement répond « http 200 » sans contenu exploitable
			// quand sa réponse est partie dans le canal de réflexion, et
			// la variante qui désactive ce canal aboutit là où la première
			// a échoué. Abandonner au premier échec jetait tout le travail
			// du tour.
			lastErr = fmt.Errorf("agent: conclusion après clôture de la boucle (%s, %s): %w", closeReason, attempt.label, err)
			if logger != nil {
				logger.WarnContext(ctx, "agent: variante de conclusion en échec",
					"agent", agentName, "attempt", attempt.label, "reason", closeReason, "error", err)
			}

			// Une réponse vide trahit le canal de réflexion : la variante
			// qui le désactive passe devant.
			orderConclusionFallbacks(attempts, false)

			continue
		}

		lastHadToolCalls = len(resp.ToolCalls()) > 0
		if !lastHadToolCalls {
			raw := ""
			if msg := resp.Message(); msg != nil {
				raw = msg.Content()
			}
			if text := cleanReply(raw); text != "" {
				if logger != nil {
					logger.InfoContext(ctx, "agent: tour conclu après clôture de la boucle",
						"agent", agentName, "attempt", attempt.label, "reason", closeReason, "reply_bytes", len(text))
				}
				return toolLoopResult{Text: text, ToolResults: toolResults, Attachments: attachments}, nil
			}
		}

		orderConclusionFallbacks(attempts, lastHadToolCalls)
	}

	if lastErr != nil {
		return toolLoopResult{}, lastErr
	}

	return toolLoopResult{}, fallbackErr
}

// maxContextBytes borne le cumul des résultats d'outils réinjectés dans la
// conversation au fil du tour (PLAN.md §9.4, "budget total des résultats",
// agents.<nom>.limits.max_tool_context_bytes) ; <= 0 le laisse illimité. Ce
// budget est distinct du plafond par résultat appliqué côté MCP
// (mcp.Limits.MaxToolResultBytes) : sans lui, maxIterations appels tenant
// chacun sous le plafond unitaire dépassent tout de même largement le
// contexte annoncé.
// logger, s'il n'est pas nil, journalise l'introspection du tour : outils
// exposés au modèle, chaque appel d'outil (nom, itération, durée, erreur),
// fin de tour. JAMAIS les arguments ni les résultats d'outils : ils portent
// du contenu privé (texte d'un rappel, contenu mémorisé, requête de
// recherche — AGENTS.md, "ne pas journaliser les contenus privés"). nil
// désactive toute journalisation (tests, agents construits hors registre).
// deliverWarning est le temps restant à partir duquel l'agent est prévenu
// qu'il doit livrer. Il faut qu'il reste de quoi faire au moins un appel
// d'outil et un tour de modèle : c'est plus que conclusionReserve, à
// dessein.
const deliverWarning = 40 * time.Second

// deliverNowInstruction part vers le modèle : anglais uniquement.
// Volontairement générique — elle sert tous les agents, pas seulement ceux
// qui produisent des fichiers.
const deliverNowInstruction = "You are running out of time for this task. " +
	"If you already have a usable result, deliver it NOW with the appropriate tool " +
	"(for example attach_file for a file you produced), then answer. " +
	"Do not start anything new, do not check your work again: an imperfect result delivered " +
	"is worth more than a perfect one you never hand over."

// conclusionReserve est la marge en deçà de laquelle la boucle renonce à
// une itération de plus. Elle est indicative : une itération déjà lancée
// peut dépasser l'échéance, c'est conclusionBudget qui garantit la
// conclusion.
const conclusionReserve = 25 * time.Second

// conclusionBudget est le temps GARANTI à la conclusion, indépendant de
// l'échéance de la boucle. Sans lui, la conclusion héritait d'un contexte
// déjà expiré et ses variantes de repli échouaient toutes en
// « context deadline exceeded » — le travail du tour était perdu alors même
// que le mécanisme de secours se déclenchait correctement.
const conclusionBudget = 45 * time.Second

// withConclusionBudget détache le contexte de l'échéance de la boucle pour
// lui en donner une propre. L'annulation du parent n'est délibérément pas
// propagée : c'est justement parce que le temps de la boucle est écoulé que
// l'on conclut. La borne reste courte, et le pipeline garde sa propre
// échéance au-dessus.
func withConclusionBudget(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), conclusionBudget)
}

func runToolLoop(ctx context.Context, client llm.ChatCompletionClient, messages []llm.Message, tools []llm.Tool, maxIterations int, maxContextBytes int64, maxReachedErr error, logger *slog.Logger, agentName string) (toolLoopResult, error) {
	if maxIterations <= 0 {
		maxIterations = 1
	}

	if logger != nil {
		toolNames := make([]string, len(tools))
		for i, tool := range tools {
			toolNames[i] = tool.Name()
		}
		logger.InfoContext(ctx, "agent: tour démarré", "agent", agentName, "tools", toolNames, "max_iterations", maxIterations)
	}

	var (
		toolResults []string
		attachments []media.Media
		usedBytes   int64
		totalCalls  int
		warned      bool
	)

	for iteration := range maxIterations {
		// Avertissement de fin de budget, une seule fois. Un agent ne sait
		// pas qu'il est chronométré : il peaufine, vérifie une fois de
		// plus, et le temps s'épuise AVANT qu'il ait livré. Vu en
		// production : un fichier correctement produit, jamais joint à la
		// réponse parce que la boucle s'est arrêtée entre-temps.
		//
		// La consigne arrive pendant la boucle, pas à la clôture : c'est ce
		// qui permet encore d'exécuter l'appel d'outil qui livre.
		if deadline, ok := ctx.Deadline(); ok && !warned && time.Until(deadline) <= deliverWarning {
			warned = true
			messages = append(messages, llm.NewMessage(llm.RoleUser, deliverNowInstruction))

			if logger != nil {
				logger.InfoContext(ctx, "agent: consigne de livraison injectée",
					"agent", agentName, "iterations", iteration+1, "tool_calls", totalCalls)
			}
		}

		// ToolChoiceAuto explicite : ne jamais dépendre du défaut de la
		// bibliothèque — un défaut à "none" a historiquement interdit tout
		// appel d'outil en silence, les modèles promettant alors des actions
		// qu'ils n'avaient pas le droit d'exécuter.
		resp, err := client.ChatCompletion(ctx, llm.WithMessages(messages...), llm.WithTools(tools...), llm.WithToolChoice(llm.ToolChoiceAuto))
		if err != nil {
			// Sans matière, il n'y a rien à sauver : l'erreur remonte.
			if len(toolResults) == 0 {
				return toolLoopResult{}, fmt.Errorf("agent: appel du client llm: %w", err)
			}

			// Avec de la matière, la jeter serait le pire résultat : les
			// outils ont déjà tourné, le fichier est peut-être déjà
			// produit. On tente de conclure — les variantes de conclusion
			// sont plus légères que l'appel qui vient d'échouer (outils
			// interdits, raisonnement désactivé), et l'une d'elles aboutit
			// souvent là où celui-ci a échoué.
			//
			// Vu en production : une passerelle qui répond « 200 » avec un
			// corps vide après 48 s emportait un tour de six appels
			// d'outils.
			if logger != nil {
				logger.WarnContext(ctx, "agent: appel du modèle en échec, conclusion tentée",
					"agent", agentName, "iterations", iteration+1, "tool_calls", totalCalls, "error", err)
			}

			conclusionCtx, cancel := withConclusionBudget(ctx)
			defer cancel()

			return concludeToolLoop(conclusionCtx, client, messages, tools, "appel du modèle en échec", fmt.Errorf("agent: appel du client llm: %w", err),
				iteration+1, totalCalls, toolResults, attachments, logger, agentName, true)
		}

		toolCalls := resp.ToolCalls()
		if len(toolCalls) == 0 {
			raw := ""
			if msg := resp.Message(); msg != nil {
				raw = msg.Content()
			}

			text := cleanReply(raw)

			if text == "" {
				// Les deux causes d'une réponse vide demandent des correctifs
				// opposés : ou le modèle n'a rien produit (réglage du client,
				// budget de réflexion), ou sa réponse n'était qu'un bloc de
				// réflexion que cleanReply a retiré. Seules les tailles sont
				// journalisées : le contenu reste privé (AGENTS.md).
				if logger != nil {
					logger.WarnContext(ctx, "agent: réponse vide du modèle", "agent", agentName,
						"iterations", iteration+1, "tool_calls", totalCalls,
						"raw_bytes", len(raw), "cleaned_bytes", len(text))
				}

				return concludeToolLoop(ctx, client, messages, tools, "réponse vide du modèle", ErrEmptyReply,
					iteration+1, totalCalls, toolResults, attachments, logger, agentName, false)
			}

			if logger != nil {
				logger.InfoContext(ctx, "agent: tour terminé", "agent", agentName, "iterations", iteration+1, "tool_calls", totalCalls, "reply_bytes", len(text))
			}

			return toolLoopResult{Text: text, ToolResults: toolResults, Attachments: attachments}, nil
		}

		messages = append(messages, llm.NewToolCallsMessage(toolCalls...))

		for _, tc := range toolCalls {
			totalCalls++
			callStart := time.Now()
			toolMessage, err := llm.ExecuteToolCall(ctx, tc, tools...)
			if logger != nil {
				logCtx := []any{"agent", agentName, "tool", tc.Name(), "iteration", iteration + 1, "duration", time.Since(callStart).String()}
				if err != nil {
					logCtx = append(logCtx, "error", err)
				}
				logger.InfoContext(ctx, "agent: appel d'outil", logCtx...)
			}
			if err != nil {
				// Un échec dur (paramètres illisibles, panique interne au
				// tool) ne doit pas faire échouer tout le tour : on le
				// transmet au modèle comme résultat d'outil, exactement
				// comme un échec applicatif du spécialiste (voir
				// newDelegationTool), pour qu'il puisse s'adapter.
				toolMessage = llm.NewToolMessage(tc.ID(), llm.NewToolResult(fmt.Sprintf("erreur d'exécution de l'outil %q: %v", tc.Name(), err)))
			}

			// Le budget ne s'applique qu'au texte réinjecté dans la
			// conversation : l'outil a déjà été exécuté, ses effets (actions
			// proposées, appels MCP en lecture) sont donc préservés. Seul ce
			// que le modèle relit est borné, et toute réduction lui est
			// signalée explicitement (PLAN.md §9.4).
			content, used := applyContextBudget(toolMessage.Content(), maxContextBytes, usedBytes)
			if content != toolMessage.Content() {
				toolMessage = llm.NewToolMessage(tc.ID(), llm.NewToolResult(content))
			}
			usedBytes = used

			// Les médias joints au résultat d'un outil sont conservés pour
			// être renvoyés à l'utilisateur, en plus d'être transmis au
			// modèle par le message d'outil lui-même.
			for _, attachment := range toolMessage.Attachments() {
				if m, ok := media.FromLLM(attachment, ""); ok {
					attachments = append(attachments, m)
				}
			}

			messages = append(messages, toolMessage)
			toolResults = append(toolResults, toolMessage.Content())
		}

		if err := ctx.Err(); err != nil {
			return toolLoopResult{}, err
		}

		// Budget de temps : le plafond d'appels ne borne pas la durée d'un
		// tour, et un agent qui enchaîne des outils lents (regarder une
		// image, ré-encoder une vidéo) épuise le délai du pipeline avant
		// d'avoir rien produit — l'utilisateur attend alors plusieurs
		// minutes pour un échec. On sort AVANT l'échéance, en gardant de
		// quoi conclure : la matière déjà rassemblée est le coût déjà payé.
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= conclusionReserve {
			conclusionCtx, cancel := withConclusionBudget(ctx)
			defer cancel()

			return concludeToolLoop(conclusionCtx, client, messages, tools, "budget de temps épuisé", maxReachedErr,
				iteration+1, totalCalls, toolResults, attachments, logger, agentName, true)
		}
	}

	conclusionCtx, cancel := withConclusionBudget(ctx)
	defer cancel()

	return concludeToolLoop(conclusionCtx, client, messages, tools, "plafond d'appels d'outils atteint", maxReachedErr,
		maxIterations, totalCalls, toolResults, attachments, logger, agentName, true)
}

// applyContextBudget réduit content pour tenir dans ce qu'il reste de
// maxBytes après usedBytes déjà consommés, et retourne le texte à réinjecter
// ainsi que le nouveau cumul. maxBytes <= 0 désactive le budget.
//
// Une fois le budget épuisé, les résultats suivants ne sont pas simplement
// omis en silence : le modèle reçoit une note explicite, sans quoi il
// interpréterait un résultat vide comme "l'outil n'a rien trouvé" et
// conclurait à tort.
func applyContextBudget(content string, maxBytes, usedBytes int64) (string, int64) {
	if maxBytes <= 0 {
		return content, usedBytes
	}

	remaining := maxBytes - usedBytes
	if remaining <= 0 {
		return "[budget de contexte d'outils épuisé : résultat non transmis]", usedBytes
	}

	if int64(len(content)) <= remaining {
		return content, usedBytes + int64(len(content))
	}

	truncated := truncateUTF8(content, remaining)

	return truncated + fmt.Sprintf("\n[résultat tronqué : budget de contexte d'outils de %d octets atteint]", maxBytes), maxBytes
}

// truncateUTF8 coupe s à max octets au plus, sans jamais laisser une
// séquence UTF-8 incomplète en fin de chaîne.
func truncateUTF8(s string, max int64) string {
	if max <= 0 {
		return ""
	}
	if int64(len(s)) <= max {
		return s
	}

	b := s[:max]
	for len(b) > 0 && !utf8.ValidString(b) {
		b = b[:len(b)-1]
	}

	return b
}
