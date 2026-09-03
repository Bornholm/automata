package registry

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/config"
)

// probeClient rejoue une réponse scriptée selon le nombre d'outils reçus :
// c'est le comportement qu'on cherche à mesurer chez un vrai modèle.
type probeClient struct {
	// callsWhenAtMost : le modèle appelle l'outil tant qu'on ne lui en
	// offre pas plus que ce nombre. 0 : il n'appelle jamais.
	callsWhenAtMost int
	err             error
	// seenTools retient les tailles de jeu d'outils reçues.
	seenTools []int
	// toolChoice retient le dernier choix d'outil demandé.
	toolChoice llm.ToolChoice
}

func (c *probeClient) ChatCompletion(_ context.Context, funcs ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	opts := llm.NewChatCompletionOptions(funcs...)
	c.seenTools = append(c.seenTools, len(opts.Tools))
	c.toolChoice = opts.ToolChoice

	if c.err != nil {
		return nil, c.err
	}

	if len(opts.Tools) <= c.callsWhenAtMost {
		return &probeResponse{toolCalls: []llm.ToolCall{llm.NewToolCall("c1", "get_maintenance_code", `{"unit":47}`)}}, nil
	}
	return &probeResponse{text: "Je ne peux pas accéder à cette information pour l'instant."}, nil
}

type probeResponse struct {
	text      string
	toolCalls []llm.ToolCall
}

func (r *probeResponse) Message() llm.Message {
	if r.text == "" {
		return nil
	}
	return llm.NewMessage(llm.RoleAssistant, r.text)
}
func (r *probeResponse) ToolCalls() []llm.ToolCall      { return r.toolCalls }
func (r *probeResponse) Usage() llm.ChatCompletionUsage { return nil }

// La sonde doit interroger le modèle EXACTEMENT comme un tour réel : un
// tool_choice différent mesurerait autre chose que ce qui se passe en
// conversation, et le diagnostic ne vaudrait rien.
func TestProbeOnce_AsksLikeARealTurn(t *testing.T) {
	client := &probeClient{callsWhenAtMost: 1}

	result := probeOnce(context.Background(), client, probeStage{tools: 1, prompt: probeSystemPrompt})
	if !result.called {
		t.Fatalf("l'appel d'outil n'a pas été détecté: %+v", result)
	}
	if client.toolChoice != llm.ToolChoiceAuto {
		t.Errorf("tool_choice = %q, attendu %q", client.toolChoice, llm.ToolChoiceAuto)
	}
	if len(client.seenTools) != 1 || client.seenTools[0] != 1 {
		t.Errorf("jeux d'outils reçus: %v", client.seenTools)
	}
}

// Un modèle qui renonce laisse sa réponse dans le rapport : c'est elle qui
// montre l'excuse inventée, et donc que rien n'est en panne côté serveur.
func TestProbeOnce_ReportsTheRefusal(t *testing.T) {
	client := &probeClient{callsWhenAtMost: 0}

	result := probeOnce(context.Background(), client, probeStage{tools: 25, prompt: probeSystemPrompt})
	if result.called {
		t.Fatal("un appel d'outil a été détecté à tort")
	}
	if !strings.Contains(result.reply, "Je ne peux pas") {
		t.Errorf("la réponse du modèle est perdue: %+v", result)
	}
	if !strings.Contains(result.line("25 outils"), "PAS APPELÉ") {
		t.Errorf("ligne de rapport inattendue: %q", result.line("25 outils"))
	}
}

func TestProbeOnce_ReportsAFailure(t *testing.T) {
	client := &probeClient{err: errors.New("401 Unauthorized")}

	result := probeOnce(context.Background(), client, probeStage{tools: 1, prompt: probeSystemPrompt})
	if result.called {
		t.Fatal("un appel d'outil a été détecté malgré l'échec")
	}
	if !strings.Contains(result.line("1 outil"), "401") {
		t.Errorf("la cause de l'échec est perdue: %q", result.line("1 outil"))
	}
}

// Le jeu d'outils contient l'outil qui répond, puis des leurres : c'est le
// NOMBRE qu'on mesure, la question restant la même.
func TestProbeTools_FirstAnswersTheQuestion(t *testing.T) {
	tools := probeTools(10)
	if len(tools) != 10 {
		t.Fatalf("%d outil(s), attendu 10", len(tools))
	}
	if tools[0].Name() != "get_maintenance_code" {
		t.Errorf("premier outil = %q", tools[0].Name())
	}

	seen := map[string]struct{}{}
	for _, tool := range tools {
		if _, dup := seen[tool.Name()]; dup {
			t.Errorf("nom d'outil dupliqué: %q", tool.Name())
		}
		seen[tool.Name()] = struct{}{}
	}

	if len(probeTools(1)) != 1 {
		t.Error("un seul outil demandé doit en donner un seul")
	}
}

// Le verdict nomme l'étage qui a cassé : c'est lui la cause, et chacune
// mène à un remède différent. Aucun ne doit conseiller de retoucher les
// prompts quand ce n'est pas le prompt qui casse.
func TestProbeVerdict_NamesTheStageThatBroke(t *testing.T) {
	stages := probeStages(fullProbeConfig(), "main")
	if len(stages) != 5 {
		t.Fatalf("%d étage(s), attendu 5 (1, 10, 25 outils, prompt, historique)", len(stages))
	}

	called := probeResult{called: true}
	failed := probeResult{reply: "Je ne peux pas."}

	cases := map[string]struct {
		results []probeResult
		needle  string
	}{
		"tout passe":             {[]probeResult{called, called, called, called, called}, "n'est ni le modèle"},
		"jamais":                 {[]probeResult{failed, failed, failed, failed, failed}, "inapte au rôle"},
		"décroche sur le nombre": {[]probeResult{called, called, failed, failed, failed}, "entre 10 et 25 outils"},
		"le prompt inhibe":       {[]probeResult{called, called, called, failed, failed}, "prompt qui l'inhibe"},
		"imite son refus":        {[]probeResult{called, called, called, called, failed}, "imite"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			verdict := probeVerdict(stages, tc.results)
			if !strings.Contains(verdict, tc.needle) {
				t.Errorf("verdict sans %q:\n%s", tc.needle, verdict)
			}
		})
	}
}

// Un échec de transport n'est pas un refus : il ne doit pas être pris pour
// la cause, sans quoi un jeton expiré ferait accuser le modèle.
func TestProbeVerdict_IgnoresTransportFailures(t *testing.T) {
	stages := probeStages(fullProbeConfig(), "main")
	results := []probeResult{
		{called: true}, {called: true}, {err: errors.New("502 Bad Gateway")},
		{called: true}, {called: true},
	}

	if verdict := probeVerdict(stages, results); !strings.Contains(verdict, "n'est ni le modèle") {
		t.Errorf("un échec de transport a été pris pour un refus:\n%s", verdict)
	}
}

// Les étages de contexte n'existent que pour un rôle qui EST un agent
// configuré : « plugins » ou « compaction » n'ont pas de prompt à eux.
func TestProbeStages_ContextStagesNeedAConfiguredAgent(t *testing.T) {
	stages := probeStages(&config.Config{}, "main")
	if len(stages) != len(probeToolCounts) {
		t.Errorf("%d étage(s) sans agent configuré, attendu %d", len(stages), len(probeToolCounts))
	}

	full := probeStages(fullProbeConfig(), "main")
	// Le prompt réel porte les règles invariantes : sans elles, l'étage ne
	// mesurerait pas ce que voit un vrai tour.
	if !strings.Contains(full[3].prompt, "Invariant security rules") {
		t.Error("l'étage du prompt n'utilise pas le prompt assemblé de l'agent")
	}
	if len(full[4].history) == 0 {
		t.Error("le dernier étage doit porter un refus dans l'historique")
	}
}

func fullProbeConfig() *config.Config {
	return &config.Config{
		Agents: map[string]config.Agent{
			"main": {
				Type:         "orchestrator",
				SystemPrompt: config.SystemPrompt{Content: "You are the household's general assistant."},
			},
		},
	}
}
