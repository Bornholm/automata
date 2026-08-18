package agent

import (
	"context"
	"fmt"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/config"
)

// reasoningEfforts énumère les niveaux acceptés en configuration. La liste
// suit celle de genai ; tous ne sont pas honorés par tous les fournisseurs,
// et un modèle sans mode réflexion ignore simplement l'option.
var reasoningEfforts = map[string]llm.ReasoningEffort{
	"none":    llm.ReasoningEffortNone,
	"minimal": llm.ReasoningEffortMinimal,
	"low":     llm.ReasoningEffortLow,
	"medium":  llm.ReasoningEffortMedium,
	"high":    llm.ReasoningEffortHigh,
	"xhigh":   llm.ReasoningEffortXHigh,
}

// ReasoningEfforts retourne les niveaux acceptés, triés, pour les messages
// d'erreur de la validation de configuration.
func ReasoningEfforts() []string {
	return []string{"none", "minimal", "low", "medium", "high", "xhigh"}
}

// reasoningOptions traduit la configuration en options genai. Retourne nil
// si aucun réglage n'est demandé : le défaut du modèle s'applique alors.
func reasoningOptions(cfg *config.LLMReasoning) (*llm.ReasoningOptions, error) {
	if cfg == nil || cfg.Effort == "" {
		return nil, nil
	}

	effort, ok := reasoningEfforts[cfg.Effort]
	if !ok {
		return nil, fmt.Errorf("niveau de réflexion %q inconnu", cfg.Effort)
	}

	return llm.NewReasoningOptions(effort), nil
}

// reasoningClient applique un réglage de réflexion à CHAQUE complétion du
// client qu'il enveloppe.
//
// C'est une option d'appel côté genai, pas de construction : sans ce
// décorateur, il faudrait la répéter sur tous les sites d'appel (boucle
// d'outils, agent en streaming, compaction, consolidation) et un oubli
// passerait inaperçu — le modèle continuerait simplement à réfléchir à son
// rythme par défaut.
type reasoningClient struct {
	llm.Client
	options *llm.ReasoningOptions
}

// ChatCompletion implémente [llm.ChatCompletionClient].
func (c *reasoningClient) ChatCompletion(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	// Le réglage est placé en tête : un appelant qui passerait explicitement
	// WithReasoning garde le dernier mot.
	return c.Client.ChatCompletion(ctx, append([]llm.ChatCompletionOptionFunc{llm.WithReasoning(c.options)}, funcs...)...)
}

// ChatCompletionStream implémente [llm.ChatCompletionStreamingClient].
func (c *reasoningClient) ChatCompletionStream(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (<-chan llm.StreamChunk, error) {
	return c.Client.ChatCompletionStream(ctx, append([]llm.ChatCompletionOptionFunc{llm.WithReasoning(c.options)}, funcs...)...)
}

// withReasoning enveloppe client si un réglage est demandé, et le retourne
// tel quel sinon.
func withReasoning(client llm.Client, options *llm.ReasoningOptions) llm.Client {
	if options == nil {
		return client
	}
	return &reasoningClient{Client: client, options: options}
}

var _ llm.Client = &reasoningClient{}
