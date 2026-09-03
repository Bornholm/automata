package registry

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/llmclients"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/secretbox"
)

// Sonde d'appel d'outils.
//
// Un assistant qui n'appelle jamais ses outils répond de mémoire et invente
// des excuses plausibles — « ce spécialiste n'est pas joignable », « la
// fonction est indisponible » — sans qu'aucune erreur n'apparaisse nulle
// part. Ni les rappels, ni la mémoire, ni les délégations ne se produisent,
// et le tour se termine normalement.
//
// La cause est presque toujours le modèle, mais « presque toujours » ne se
// déploie pas : il faut la voir. Cette sonde reproduit le tour le plus
// petit possible — un outil trivial, une question qui l'exige — puis
// recommence avec un nombre croissant d'outils factices. Elle sépare ainsi
// trois causes que les journaux d'une instance en marche confondent :
//
//   - le modèle n'appelle jamais d'outil, même seul : il est inapte au rôle ;
//   - il appelle avec un outil et renonce avec vingt : c'est le NOMBRE
//     d'outils qui le noie, et l'instance peut en retirer ;
//   - il appelle dans tous les cas : la panne est ailleurs, et ce n'est
//     plus le modèle qu'il faut changer.
//
// C'est la leçon du 2026-08-18, où cinq fausses pistes (configuration,
// prompt, registre, historique, modèle) ont précédé la vraie cause, un
// tool_choice à "none" : reproduire en isolé AVANT de toucher aux prompts.

// probeToolCounts sont les tailles de jeu d'outils essayées. La première
// mesure l'aptitude, les suivantes le seuil de décrochage — un
// orchestrateur complet en expose une vingtaine.
var probeToolCounts = []int{1, 10, 25}

// probeFullTools est la taille utilisée par les étages qui ajoutent le
// contexte d'un vrai tour : celui d'un orchestrateur réel.
const probeFullTools = 25

// probeTimeout borne chaque essai.
const probeTimeout = 60 * time.Second

// probeQuestion part au modèle : anglais, comme toute consigne. Elle ne
// laisse aucune issue honnête sans appeler l'outil — le modèle ne peut pas
// connaître ce nombre.
const probeQuestion = "What is the maintenance code of unit 47? Use your tools; do not guess."

// probeSystemPrompt part au modèle : anglais. Volontairement minimal —
// c'est l'étalon auquel les étages suivants comparent le prompt réel.
const probeSystemPrompt = "You are a diagnostic assistant. When a tool can answer the question, call it."

// probeRefusal est le refus injecté dans l'historique du dernier étage. Il
// reproduit la forme observée en production : l'assistant décline, puis
// décline à nouveau au tour suivant, sans jamais essayer.
const probeRefusal = "Je suis désolé, ce service est temporairement indisponible. Je ne peux pas récupérer cette information pour l'instant."

// probeStage est un essai : un nombre d'outils, un prompt système, et un
// historique.
type probeStage struct {
	label   string
	tools   int
	prompt  string
	history []llm.Message
}

// probeStages compose la bissection. Chaque étage ajoute UN élément du tour
// réel à celui d'avant : le premier qui casse désigne la cause, ce qu'aucun
// journal d'instance en marche ne peut faire.
func probeStages(cfg *config.Config, role string) []probeStage {
	stages := make([]probeStage, 0, len(probeToolCounts)+2)
	for _, count := range probeToolCounts {
		stages = append(stages, probeStage{
			label:  fmt.Sprintf("%2d outil(s)", count),
			tools:  count,
			prompt: probeSystemPrompt,
		})
	}

	agentCfg, ok := cfg.Agents[role]
	if !ok {
		return stages
	}

	// Le prompt réel, assemblé comme au tour : règles invariantes,
	// personnalité, capacités et règles d'honnêteté comprises.
	full := agent.BuildSystemPrompt(role, agentCfg)

	stages = append(stages,
		probeStage{
			label:  "+ le prompt de l'agent",
			tools:  probeFullTools,
			prompt: full,
		},
		probeStage{
			label:  "+ un refus dans l'historique",
			tools:  probeFullTools,
			prompt: full,
			history: []llm.Message{
				llm.NewMessage(llm.RoleUser, probeQuestion),
				llm.NewMessage(llm.RoleAssistant, probeRefusal),
			},
		},
	)

	return stages
}

// ProbeTools exécute la sonde pour un rôle et écrit son rapport.
func ProbeTools(ctx context.Context, cfg *config.Config, role, orgID string, out io.Writer) error {
	if role == "" {
		// L'orchestrateur : c'est lui qui porte le plus d'outils, et le
		// seul dont le mutisme se voie en conversation.
		role = "main"
	}

	db, err := persistence.OpenWithEncryption(ctx, cfg.Storage.Application, cfg.Storage.EncryptionKey)
	if err != nil {
		return fmt.Errorf("registry: ouverture de la persistance: %w", err)
	}
	defer func() { _ = db.Close() }()

	box, err := secretbox.NewLLMClients(cfg.Web.SessionSecret)
	if err != nil {
		return fmt.Errorf("registry: dérivation de la clé du catalogue de modèles: %w", err)
	}

	store := llmclients.NewStore(db, box)
	// Journal muet : le rapport est ce que l'exploitant lit, une ligne de
	// résolution par essai le noierait.
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	resolver := llmclients.NewResolver(llmclients.NewPool(store, agent.BuildLLMClient, quiet), store, quiet)

	resolved, err := resolver.ResolveClient(ctx, role, model.OrgID(orgID))
	if err != nil {
		return fmt.Errorf("registry: aucun modèle pour le rôle %q: %w", role, err)
	}

	scope := "défaut de l'instance"
	if orgID != "" {
		scope = "organisation " + orgID
	}
	fmt.Fprintf(out, "rôle %s (%s)\n", role, scope)
	fmt.Fprintf(out, "  client  %s\n", resolved.Name)
	fmt.Fprintf(out, "  modèle  %s\n\n", resolved.Model)

	stages := probeStages(cfg, role)

	var results []probeResult
	for _, stage := range stages {
		result := probeOnce(ctx, resolved.Client, stage)
		results = append(results, result)
		fmt.Fprintln(out, result.line(stage.label))
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, probeVerdict(stages, results))

	return nil
}

// probeResult est le résultat d'un essai.
type probeResult struct {
	called bool
	err    error
	reply  string
}

func (r probeResult) line(stageLabel string) string {
	label := fmt.Sprintf("  %-30s : ", stageLabel)

	switch {
	case r.err != nil:
		return label + "ÉCHEC        " + firstLine(r.err.Error())
	case r.called:
		return label + "outil appelé"
	default:
		return label + "PAS APPELÉ   réponse : " + firstLine(r.reply)
	}
}

// probeOnce envoie une question à laquelle seul l'outil peut répondre, dans
// les conditions de l'étage : nombre d'outils, prompt système, historique.
func probeOnce(ctx context.Context, client llm.ChatCompletionClient, stage probeStage) probeResult {
	callCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	messages := []llm.Message{llm.NewMessage(llm.RoleSystem, stage.prompt)}
	messages = append(messages, stage.history...)
	messages = append(messages, llm.NewMessage(llm.RoleUser, probeQuestion))

	resp, err := client.ChatCompletion(callCtx,
		llm.WithMessages(messages...),
		llm.WithTools(probeTools(stage.tools)...),
		// Exactement ce que fait un tour réel (voir runToolLoop) : la sonde
		// ne vaudrait rien si elle interrogeait le modèle autrement.
		llm.WithToolChoice(llm.ToolChoiceAuto),
	)
	if err != nil {
		return probeResult{err: err}
	}

	if len(resp.ToolCalls()) > 0 {
		return probeResult{called: true}
	}

	reply := ""
	if msg := resp.Message(); msg != nil {
		reply = strings.TrimSpace(msg.Content())
	}
	if reply == "" {
		reply = "(réponse vide)"
	}

	return probeResult{reply: reply}
}

// probeTools construit le jeu d'outils : le premier répond à la question,
// les suivants sont des leurres crédibles. Ils ne sont jamais exécutés —
// seul compte le fait que le modèle en demande un.
func probeTools(count int) []llm.Tool {
	answer := llm.NewFuncTool(
		"get_maintenance_code",
		"Return the maintenance code of a unit, by its number.",
		llm.NewJSONSchema().RequiredProperty("unit", "The unit number.", "integer"),
		func(context.Context, map[string]any) (llm.ToolResult, error) {
			return llm.NewToolResult("K-4417"), nil
		},
	)

	tools := []llm.Tool{answer}
	for i := 1; i < count; i++ {
		name := fmt.Sprintf("lookup_record_%02d", i)
		tools = append(tools, llm.NewFuncTool(
			name,
			fmt.Sprintf("Look up record set %d. Unrelated to maintenance codes.", i),
			llm.NewJSONSchema().RequiredProperty("query", "What to look up.", "string"),
			func(context.Context, map[string]any) (llm.ToolResult, error) {
				return llm.NewToolResult("nothing"), nil
			},
		))
	}

	return tools
}

// probeVerdict traduit les résultats en une conclusion actionnable. C'est
// la seule ligne que beaucoup liront, et elle doit nommer l'étage qui a
// cassé — c'est lui la cause.
func probeVerdict(stages []probeStage, results []probeResult) string {
	firstFailure := -1
	for i, result := range results {
		if !result.called && result.err == nil {
			firstFailure = i
			break
		}
	}

	switch {
	case firstFailure < 0:
		return "Ce modèle appelle ses outils dans toutes les conditions essayées,\n" +
			"prompt de l'agent et historique de refus compris.\n" +
			"Si l'assistant reste muet en conversation, la cause n'est ni le modèle,\n" +
			"ni le nombre d'outils, ni le prompt : comparez avec les lignes\n" +
			"« agent: tour démarré » d'un vrai tour (champs model et tools)."

	case firstFailure == 0:
		return "Ce modèle n'appelle AUCUN outil, même seul face à une question qui\n" +
			"l'exige. Il est inapte au rôle : changez-en depuis l'administration\n" +
			"(écran Modèles). Aucun réglage de prompt ne rattrape cela."

	case stages[firstFailure].tools > stages[firstFailure-1].tools:
		return fmt.Sprintf("Ce modèle décroche entre %d et %d outils : c'est leur NOMBRE qui le noie.\n",
			stages[firstFailure-1].tools, stages[firstFailure].tools) +
			"Deux issues, cumulables : lui en offrir moins (retirez des délégués, les\n" +
			"rappels ou les tâches planifiées de l'agent concerné dans la\n" +
			"configuration), ou lui préférer un modèle qui tient la charge."

	case len(stages[firstFailure].history) > 0:
		return "Ce modèle appelle ses outils, SAUF quand un refus figure dans\n" +
			"l'historique : il imite alors sa propre réponse précédente au lieu\n" +
			"d'essayer. C'est ce qui rend la panne persistante — un premier refus\n" +
			"en engendre une suite, et repartir d'une conversation neuve suffit à\n" +
			"tout débloquer. Le prompt le lui interdit déjà (règles d'honnêteté,\n" +
			"« TRY IT AGAIN this turn ») : ce modèle ne suit pas la consigne."

	default:
		return "Ce modèle appelle ses outils, SAUF avec le prompt de l'agent : c'est\n" +
			"le prompt qui l'inhibe, pas le nombre d'outils ni le modèle lui-même.\n" +
			"Regardez la personnalité de l'agent (agents.<nom>.system_prompt) avant\n" +
			"les règles invariantes, qui, elles, ne se configurent pas."
	}
}

func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx]
	}
	const max = 120
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
