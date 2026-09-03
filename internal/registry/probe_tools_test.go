package registry

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bornholm/genai/llm"
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

	result := probeOnce(context.Background(), client, 1)
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

	result := probeOnce(context.Background(), client, 25)
	if result.called {
		t.Fatal("un appel d'outil a été détecté à tort")
	}
	if !strings.Contains(result.reply, "Je ne peux pas") {
		t.Errorf("la réponse du modèle est perdue: %+v", result)
	}
	if !strings.Contains(result.line(25), "PAS APPELÉ") {
		t.Errorf("ligne de rapport inattendue: %q", result.line(25))
	}
}

func TestProbeOnce_ReportsAFailure(t *testing.T) {
	client := &probeClient{err: errors.New("401 Unauthorized")}

	result := probeOnce(context.Background(), client, 1)
	if result.called {
		t.Fatal("un appel d'outil a été détecté malgré l'échec")
	}
	if !strings.Contains(result.line(1), "401") {
		t.Errorf("la cause de l'échec est perdue: %q", result.line(1))
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

// Le verdict est la seule ligne que beaucoup liront : les trois cas
// mènent à des remèdes différents, et aucun ne doit conseiller de
// retoucher les prompts.
func TestProbeVerdict(t *testing.T) {
	cases := map[string]struct {
		called, total int
		needle        string
	}{
		"jamais":   {0, 3, "inapte au rôle"},
		"parfois":  {1, 3, "moins"},
		"toujours": {3, 3, "ailleurs"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			verdict := probeVerdict(tc.called, tc.total)
			if !strings.Contains(verdict, tc.needle) {
				t.Errorf("verdict sans %q: %s", tc.needle, verdict)
			}
			if strings.Contains(strings.ToLower(verdict), "prompt") && tc.called != 0 {
				t.Errorf("le verdict renvoie vers les prompts: %s", verdict)
			}
		})
	}
}
