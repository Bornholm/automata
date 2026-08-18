package config

import "testing"

// La configuration produite par le wizard omettait base_url sur le client de
// transcription : la panne n'apparaissait qu'au premier message vocal reçu,
// des semaines après le déploiement, sous la forme d'un « unsupported
// protocol scheme "" ». Un client incomplet doit être refusé au chargement.
func TestValidateLLMClients_RequiresEveryField(t *testing.T) {
	cfg := &Config{
		LLMClients: map[string]LLMClient{
			"transcription": {Provider: "openai", Model: "whisper-1", APIKey: "sk-test"},
		},
	}

	errs := validateLLMClients(cfg)
	assertHasError(t, errs, "llm_clients.transcription.base_url: requis")

	cfg.LLMClients["main"] = LLMClient{}
	errs = validateLLMClients(cfg)
	for _, field := range []string{"provider", "model", "api_key", "base_url"} {
		assertHasError(t, errs, "llm_clients.main."+field+": requis")
	}

	cfg = &Config{
		LLMClients: map[string]LLMClient{
			"main": {Provider: "openai", Model: "gpt-test", APIKey: "sk-test", BaseURL: "https://api.example.test/v1"},
		},
	}
	if errs := validateLLMClients(cfg); len(errs) != 0 {
		t.Fatalf("client complet refusé: %v", errs)
	}
}
