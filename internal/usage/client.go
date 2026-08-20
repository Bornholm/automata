package usage

import (
	"context"
	"time"

	"github.com/bornholm/genai/llm"
)

// recordingClient décore un llm.Client pour enregistrer la consommation de
// chaque appel réussi. provider et model proviennent de la configuration du
// client (config.LLMClient) : la réponse genai ne les porte pas.
//
// Le décorateur est appliqué inconditionnellement par les constructeurs de
// clients (agent.BuildLLMClient, agent.BuildTranscriptionClient) : c'est la
// présence d'un Recorder dans le contexte, au moment de l'appel, qui décide
// si une trace est écrite. Sans Recorder (tests, commandes CLI), il est
// transparent.
//
// Seuls les appels réussis sont enregistrés : un appel en erreur ne rapporte
// aucun volume exploitable, et les middlewares de retry (wrapResilience)
// sont enveloppés PAR ce décorateur — une complétion retentée trois fois ne
// produit qu'une trace, celle de la réponse effectivement obtenue.
type recordingClient struct {
	llm.Client
	provider string
	model    string
}

// WrapClient décore client pour enregistrer la consommation de ses
// complétions (unitaires et en streaming) et de ses transcriptions. Les
// embeddings ne sont PAS enregistrés : leurs appels partent de la scrutation
// asynchrone d'amoxtli, dont le contexte ne porte ni Recorder ni
// Attribution (voir docs/configuration.md, « Comptabilité d'usage »).
func WrapClient(client llm.Client, provider, model string) llm.Client {
	return &recordingClient{Client: client, provider: provider, model: model}
}

// ChatCompletion implémente [llm.ChatCompletionClient].
func (c *recordingClient) ChatCompletion(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	resp, err := c.Client.ChatCompletion(ctx, funcs...)
	if err != nil {
		return resp, err
	}

	c.record(ctx, KindChat, resp.Usage())

	return resp, nil
}

// ChatCompletionStream implémente [llm.ChatCompletionStreamingClient]. La
// consommation d'un flux n'est connue qu'à sa clôture : les chunks sont
// relayés tels quels et le dernier usage non nil rencontré est enregistré
// une fois le flux source épuisé.
func (c *recordingClient) ChatCompletionStream(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (<-chan llm.StreamChunk, error) {
	source, err := c.Client.ChatCompletionStream(ctx, funcs...)
	if err != nil {
		return source, err
	}

	if _, ok := RecorderFromContext(ctx); !ok {
		return source, nil
	}

	relay := make(chan llm.StreamChunk)
	go func() {
		defer close(relay)

		var lastUsage llm.ChatCompletionUsage
		for chunk := range source {
			if usage := chunk.Usage(); usage != nil {
				lastUsage = usage
			}
			relay <- chunk
		}

		if lastUsage != nil {
			c.record(ctx, KindChat, lastUsage)
		}
	}()

	return relay, nil
}

// Transcription implémente [llm.TranscriptionClient].
func (c *recordingClient) Transcription(ctx context.Context, audio []byte, funcs ...llm.TranscriptionOptionFunc) (llm.TranscriptionResponse, error) {
	resp, err := c.Client.Transcription(ctx, audio, funcs...)
	if err != nil {
		return resp, err
	}

	recorder, ok := RecorderFromContext(ctx)
	if !ok {
		return resp, nil
	}

	rec := c.newRecord(ctx, KindTranscription)
	// L'usage d'une transcription peut être nil (provider sans métriques) :
	// l'appel est enregistré quand même, un appel sans volume vaut mieux
	// qu'un appel invisible.
	if usage := resp.Usage(); usage != nil {
		rec.PromptTokens = usage.InputTokens()
		rec.CompletionTokens = usage.OutputTokens()
		rec.TotalTokens = usage.TotalTokens()
		applyReportedCost(&rec, usage)
	}
	recorder.RecordUsage(ctx, rec)

	return resp, nil
}

// applyReportedCost inscrit le coût rapporté par le fournisseur, s'il en
// rapporte un. Toutes les natures d'usage passent par ici : une
// transcription ou une image facturée au réel n'a aucune raison d'être
// estimée, et les images le sont d'autant plus mal qu'elles se facturent à
// l'unité, pas au jeton.
func applyReportedCost(rec *Record, usage any) {
	reporting, ok := usage.(llm.CostReportingUsage)
	if !ok {
		return
	}

	amount, currency, ok := reporting.Cost()
	if !ok {
		return
	}

	rec.CostAmount = amount
	rec.CostCurrency = currency
	rec.CostReported = true
}

// record enregistre usage (possiblement nil) sous la nature kind, si un
// Recorder est attaché au contexte.
func (c *recordingClient) record(ctx context.Context, kind string, usage llm.ChatCompletionUsage) {
	recorder, ok := RecorderFromContext(ctx)
	if !ok {
		return
	}

	rec := c.newRecord(ctx, kind)
	if usage != nil {
		rec.PromptTokens = usage.PromptTokens()
		rec.CompletionTokens = usage.CompletionTokens()
		rec.TotalTokens = usage.TotalTokens()
		// CachedTokens et Cost sont des capacités optionnelles du provider,
		// découvertes par assertion de type (même convention que genai).
		if cached, ok := usage.(interface{ CachedTokens() int64 }); ok {
			rec.CachedTokens = cached.CachedTokens()
		}
		applyReportedCost(&rec, usage)
	}

	recorder.RecordUsage(ctx, rec)
}

// newRecord amorce un Record avec l'horodatage, la cible (provider, modèle)
// et l'attribution portée par le contexte — valeur zéro si absente : la
// trace est alors orpheline, pas abandonnée.
func (c *recordingClient) newRecord(ctx context.Context, kind string) Record {
	attr, _ := AttributionFromContext(ctx)

	return Record{
		CreatedAt:      time.Now().UTC(),
		OrgID:          attr.OrgID,
		PrincipalID:    attr.PrincipalID,
		ConversationID: attr.ConversationID,
		Component:      attr.Component,
		Agent:          attr.Agent,
		Kind:           kind,
		Provider:       c.provider,
		Model:          c.model,
	}
}

// Unwrap expose le client enveloppé, pour que les tests puissent vérifier
// la composition de la pile de middlewares (voir
// TestBuildLLMClient_WrapsResilienceMiddlewares).
func (c *recordingClient) Unwrap() llm.Client {
	return c.Client
}

var _ llm.Client = &recordingClient{}

// recordingImageClient décore un llm.ImageGenerationClient sur le même
// principe que recordingClient : l'interface de génération d'images est
// distincte de llm.Client dans genai, d'où un décorateur dédié.
type recordingImageClient struct {
	llm.ImageGenerationClient
	provider string
	model    string
}

// WrapImageClient décore client pour enregistrer la consommation de ses
// générations d'images. Les providers d'images ne rapportent pas de coût
// facturé : seuls les tokens (quand ils existent) et l'appel lui-même sont
// enregistrés.
func WrapImageClient(client llm.ImageGenerationClient, provider, model string) llm.ImageGenerationClient {
	return &recordingImageClient{ImageGenerationClient: client, provider: provider, model: model}
}

// ImageGeneration implémente [llm.ImageGenerationClient].
func (c *recordingImageClient) ImageGeneration(ctx context.Context, prompt string, funcs ...llm.ImageGenerationOptionFunc) (llm.ImageGenerationResponse, error) {
	resp, err := c.ImageGenerationClient.ImageGeneration(ctx, prompt, funcs...)
	if err != nil {
		return resp, err
	}

	recorder, ok := RecorderFromContext(ctx)
	if !ok {
		return resp, nil
	}

	attr, _ := AttributionFromContext(ctx)
	rec := Record{
		CreatedAt:      time.Now().UTC(),
		OrgID:          attr.OrgID,
		PrincipalID:    attr.PrincipalID,
		ConversationID: attr.ConversationID,
		Component:      attr.Component,
		Agent:          attr.Agent,
		Kind:           KindImage,
		Provider:       c.provider,
		Model:          c.model,
	}
	if usage := resp.Usage(); usage != nil {
		rec.PromptTokens = usage.InputTokens()
		rec.CompletionTokens = usage.OutputTokens()
		rec.TotalTokens = usage.TotalTokens()
		applyReportedCost(&rec, usage)
	}
	recorder.RecordUsage(ctx, rec)

	return resp, nil
}

var _ llm.ImageGenerationClient = &recordingImageClient{}
