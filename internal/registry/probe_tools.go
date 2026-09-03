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

// probeToolCounts sont les tailles de jeu d'outils essayées par défaut. La
// première mesure l'aptitude, les suivantes le seuil de décrochage — un
// orchestrateur complet en expose une vingtaine.
var probeToolCounts = []int{1, 10, 25}

// probeTimeout borne chaque essai.
const probeTimeout = 60 * time.Second

// probeQuestion part au modèle : anglais, comme toute consigne. Elle ne
// laisse aucune issue honnête sans appeler l'outil — le modèle ne peut pas
// connaître ce nombre.
const probeQuestion = "What is the maintenance code of unit 47? Use your tools; do not guess."

// probeSystemPrompt part au modèle : anglais.
const probeSystemPrompt = "You are a diagnostic assistant. When a tool can answer the question, call it."

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

	called := 0
	for _, count := range probeToolCounts {
		result := probeOnce(ctx, resolved.Client, count)
		if result.called {
			called++
		}
		fmt.Fprintln(out, result.line(count))
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, probeVerdict(called, len(probeToolCounts)))

	return nil
}

// probeResult est le résultat d'un essai.
type probeResult struct {
	called bool
	err    error
	reply  string
}

func (r probeResult) line(count int) string {
	label := fmt.Sprintf("  %2d outil(s) : ", count)

	switch {
	case r.err != nil:
		return label + "ÉCHEC        " + firstLine(r.err.Error())
	case r.called:
		return label + "outil appelé"
	default:
		return label + "PAS APPELÉ   réponse : " + firstLine(r.reply)
	}
}

// probeOnce envoie une question à laquelle seul l'outil peut répondre, avec
// count outils au total : le vrai, et des leurres plausibles pour mesurer
// l'effet du nombre.
func probeOnce(ctx context.Context, client llm.ChatCompletionClient, count int) probeResult {
	callCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	resp, err := client.ChatCompletion(callCtx,
		llm.WithMessages(
			llm.NewMessage(llm.RoleSystem, probeSystemPrompt),
			llm.NewMessage(llm.RoleUser, probeQuestion),
		),
		llm.WithTools(probeTools(count)...),
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

// probeVerdict traduit le compte en une conclusion actionnable. C'est la
// seule ligne que beaucoup liront.
func probeVerdict(called, total int) string {
	switch {
	case called == total:
		return "Ce modèle appelle ses outils, y compris avec un jeu fourni.\n" +
			"Si l'assistant reste muet en conversation, la cause est ailleurs :\n" +
			"comparez avec les lignes « agent: tour terminé » (champs model et tool_calls)."
	case called == 0:
		return "Ce modèle n'appelle AUCUN outil, même seul face à une question qui l'exige.\n" +
			"Il est inapte au rôle : changez-en depuis l'administration (écran Modèles).\n" +
			"Aucun réglage de prompt ne rattrape cela."
	default:
		return "Ce modèle appelle ses outils quand ils sont peu nombreux, et renonce\n" +
			"au-delà. Deux issues, cumulables : lui en offrir moins (retirez des\n" +
			"délégués, les rappels ou les tâches planifiées de l'agent concerné dans\n" +
			"la configuration), ou lui préférer un modèle qui tient la charge."
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
