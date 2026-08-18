package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"time"

	"github.com/bornholm/genai/llm"
	"github.com/bornholm/genai/llm/provider"
	"github.com/bornholm/genai/llm/provider/minimax"
	"github.com/bornholm/genai/llm/provider/openai"
	"github.com/bornholm/genai/llm/provider/openrouter"

	"github.com/bornholm/automata/internal/config"
)

// imageGenerationTimeout borne un appel de génération : les modèles d'image
// sont plus lents qu'une complétion, mais une minute sans réponse est une
// panne, pas une attente.
const imageGenerationTimeout = time.Minute

// imageAspectRatios est la liste proposée au modèle. Elle suit les ratios
// MiniMax, que les providers traduisent ou ignorent selon leur dialecte.
var imageAspectRatios = []string{"1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "21:9"}

// BuildImageGenerationClient construit le client de génération d'images
// d'un image_clients.<nom>. Même structure que BuildLLMClient, registre de
// providers genai compris.
func BuildImageGenerationClient(ctx context.Context, cfg config.ImageClient) (llm.ImageGenerationClient, error) {
	common := provider.CommonOptions{
		Model:   cfg.Model,
		BaseURL: cfg.BaseURL,
		APIKey:  cfg.APIKey,
	}

	// Contrairement aux llm_clients, une base_url vide garde le défaut du
	// provider : openai a besoin de la sienne, les autres embarquent leur
	// service.
	if cfg.BaseURL == "" && cfg.Provider == "openai" {
		common.BaseURL = "https://api.openai.com/v1"
	}

	var optFunc provider.OptionFunc

	switch cfg.Provider {
	case "openai":
		optFunc = provider.WithImageGeneration(openai.Name, openai.Options{CommonOptions: common})
	case "openrouter":
		optFunc = provider.WithImageGeneration(openrouter.Name, openrouter.Options{CommonOptions: common})
	case "minimax":
		optFunc = provider.WithImageGeneration(minimax.Name, minimax.Options{CommonOptions: common})
	default:
		return nil, fmt.Errorf("provider d'images %q non supporté", cfg.Provider)
	}

	client, err := provider.Create(ctx, optFunc)
	if err != nil {
		return nil, fmt.Errorf("création du client d'images (provider %q): %w", cfg.Provider, err)
	}

	generator, ok := client.(llm.ImageGenerationClient)
	if !ok {
		return nil, fmt.Errorf("le provider %q ne fournit pas de génération d'images", cfg.Provider)
	}

	return generator, nil
}

// newGenerateImageTool expose generate_image à un spécialiste. L'image
// produite est JOINTE au résultat d'outil : la boucle de tool-calling la
// remonte alors dans Result.Attachments, jusqu'au canal — le modèle n'a
// rien à faire d'autre que décrire ce qu'il a demandé.
func newGenerateImageTool(generator llm.ImageGenerationClient) llm.Tool {
	schema := llm.NewJSONSchema().
		RequiredProperty("prompt", "Detailed description of the image to generate, in English (image models understand English best): subject, style, lighting, composition.", "string").
		Property("aspect_ratio", fmt.Sprintf("Aspect ratio of the image, one of: %v. Defaults to the model's own.", imageAspectRatios), "string")

	return llm.NewFuncTool(
		"generate_image",
		"Generate an image from a text description. The image is automatically attached to your reply: do NOT describe it back, just confirm briefly what was generated.",
		schema,
		func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
			prompt, _ := params["prompt"].(string)
			if prompt == "" {
				return llm.NewToolResult("error: 'prompt' is required and cannot be empty."), nil
			}

			var opts []llm.ImageGenerationOptionFunc
			if ratio, _ := params["aspect_ratio"].(string); ratio != "" {
				if !slices.Contains(imageAspectRatios, ratio) {
					return llm.NewToolResult(fmt.Sprintf("error: unknown aspect_ratio %q, use one of %v.", ratio, imageAspectRatios)), nil
				}
				opts = append(opts, llm.WithImageAspectRatio(ratio))
			}

			genCtx, cancel := context.WithTimeout(ctx, imageGenerationTimeout)
			defer cancel()

			resp, err := generator.ImageGeneration(genCtx, prompt, opts...)
			if err != nil {
				// Jamais d'erreur Go : le modèle doit pouvoir expliquer
				// l'échec à l'utilisateur au lieu de faire échouer le tour.
				return llm.NewToolResult(fmt.Sprintf("image generation failed: %v", err)), nil
			}

			images := resp.Images()
			if len(images) == 0 {
				return llm.NewToolResult("image generation returned no image."), nil
			}

			attachments := make([]llm.Attachment, 0, len(images))
			for _, img := range images {
				attachment, err := llm.NewImageAttachment(img.MediaType(), base64.StdEncoding.EncodeToString(img.Data()), false)
				if err != nil {
					return llm.NewToolResult(fmt.Sprintf("generated image cannot be attached: %v", err)), nil
				}
				attachments = append(attachments, attachment)
			}

			return llm.NewToolResult(
				fmt.Sprintf("%d image(s) generated and attached to the reply. Confirm briefly; do not describe the image in detail.", len(images)),
				attachments...,
			), nil
		},
	)
}
