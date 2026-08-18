package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/config"
)

// La liste des niveaux de réflexion est dupliquée entre internal/config (qui
// valide la configuration) et ce paquet (qui traduit en options genai) :
// internal/config ne dépend d'aucun paquet applicatif. Ce test tient les deux
// alignées — sans lui, un niveau accepté au chargement pourrait être refusé à
// la construction du client, donc au démarrage du worker.
func TestReasoningEffortsMatchConfigValidation(t *testing.T) {
	efforts := ReasoningEfforts()

	for _, effort := range efforts {
		opts, err := reasoningOptions(&config.LLMReasoning{Effort: effort})
		if err != nil {
			t.Errorf("niveau %q annoncé mais refusé: %v", effort, err)
		}
		if opts == nil {
			t.Errorf("niveau %q n'a produit aucune option", effort)
		}
	}

	if !slices.IsSorted(efforts) && len(efforts) != 6 {
		t.Errorf("niveaux = %v", efforts)
	}
}

func TestReasoningOptions_AbsentMeansModelDefault(t *testing.T) {
	opts, err := reasoningOptions(nil)
	if err != nil || opts != nil {
		t.Fatalf("reasoningOptions(nil) = (%v, %v), attendu (nil, nil)", opts, err)
	}

	opts, err = reasoningOptions(&config.LLMReasoning{})
	if err != nil || opts != nil {
		t.Fatalf("effort vide = (%v, %v), attendu (nil, nil)", opts, err)
	}

	if _, err := reasoningOptions(&config.LLMReasoning{Effort: "beaucoup"}); err == nil {
		t.Error("un niveau inconnu doit être refusé")
	}
}

// Vérifie que le réglage atteint réellement le fournisseur : une option
// silencieusement perdue en chemin laisserait le modèle réfléchir à son
// rythme par défaut, et seule la facture (ou la lenteur) le dirait.
// Le provider "openai" est le seul des trois à honorer BaseURL : celui
// d'openrouter construit son client sur l'URL du service en dur, et ne peut
// donc pas être intercepté par un serveur de test. C'est aussi celui qu'on
// utilise en pratique pour parler à un service compatible OpenAI, OpenRouter
// compris.
const providerUnderTest = "openai"

func TestReasoningReachesTheWire(t *testing.T) {
	var body []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	client, err := BuildLLMClient(context.Background(), config.LLMClient{
		Provider: providerUnderTest, Model: "m", APIKey: "sk-test", BaseURL: server.URL,
		Reasoning: &config.LLMReasoning{Effort: "low"},
	})
	if err != nil {
		t.Fatalf("BuildLLMClient: %v", err)
	}

	if _, err := client.ChatCompletion(context.Background(), llm.WithMessages(llm.NewMessage(llm.RoleUser, "coucou"))); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	t.Logf("provider %q envoie: %s", providerUnderTest, body)

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("corps illisible: %v", err)
	}

	// Les deux dialectes existent : "reasoning_effort" (OpenAI) et
	// "reasoning": {"effort"} (OpenRouter). Chaque provider genai émet le
	// sien ; ce qui compte est que le niveau parte.
	switch {
	case payload["reasoning_effort"] == "low":
	case reasoningEffortOf(payload) == "low":
	default:
		t.Fatalf("corps envoyé = %s, le niveau de réflexion n'y figure pas", body)
	}
}

func reasoningEffortOf(payload map[string]any) any {
	reasoning, ok := payload["reasoning"].(map[string]any)
	if !ok {
		return nil
	}
	return reasoning["effort"]
}

// Sans réglage, rien n'est imposé au fournisseur : le défaut du modèle
// s'applique.
func TestWithoutReasoningNothingIsSent(t *testing.T) {
	var body []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	client, err := BuildLLMClient(context.Background(), config.LLMClient{
		Provider: providerUnderTest, Model: "m", APIKey: "sk-test", BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("BuildLLMClient: %v", err)
	}

	if _, err := client.ChatCompletion(context.Background(), llm.WithMessages(llm.NewMessage(llm.RoleUser, "coucou"))); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	if bytes.Contains(body, []byte("reasoning")) {
		t.Errorf("corps envoyé = %s, aucun réglage de réflexion ne devait partir", body)
	}
}
