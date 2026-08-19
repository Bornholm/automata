package agent

import (
	"context"

	"github.com/bornholm/genai/llm"
)

// extraFieldsClient ajoute des champs bruts au corps de CHAQUE requête du
// client qu'il enveloppe.
//
// Les passerelles compatibles OpenAI acceptent des paramètres que l'API
// d'origine ne connaît pas. Le cas qui motive ce décorateur est comptable :
// OpenRouter ne rapporte le coût réel d'un appel que si la requête porte
// « usage: {include: true} ». Sans lui, Automata facture sur une
// estimation par tokens là où le fournisseur connaît le chiffre exact.
//
// Comme le réglage de réflexion, c'est une option d'appel côté genai : la
// répéter sur chaque site d'appel (boucle d'outils, streaming, compaction,
// consolidation) inviterait l'oubli, et l'oubli serait silencieux.
type extraFieldsClient struct {
	llm.Client
	fields map[string]any
}

// ChatCompletion implémente [llm.ChatCompletionClient].
func (c *extraFieldsClient) ChatCompletion(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	return c.Client.ChatCompletion(ctx, append([]llm.ChatCompletionOptionFunc{llm.WithExtraFields(c.fields)}, funcs...)...)
}

// ChatCompletionStream implémente [llm.ChatCompletionStreamingClient].
func (c *extraFieldsClient) ChatCompletionStream(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (<-chan llm.StreamChunk, error) {
	return c.Client.ChatCompletionStream(ctx, append([]llm.ChatCompletionOptionFunc{llm.WithExtraFields(c.fields)}, funcs...)...)
}

// withExtraFields enveloppe client si des champs sont déclarés, et le
// retourne tel quel sinon.
func withExtraFields(client llm.Client, fields map[string]any) llm.Client {
	if len(fields) == 0 {
		return client
	}
	return &extraFieldsClient{Client: client, fields: fields}
}

var _ llm.Client = &extraFieldsClient{}
