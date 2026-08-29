package agent

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/llmclients"
	"github.com/bornholm/automata/internal/model"
)

// ClientResolver choisit le client de modèle d'un rôle pour une
// organisation : la surcharge de l'organisation si elle existe, le défaut
// de l'instance sinon (voir llmclients.Resolver).
//
// Sans résolveur câblé, chaque agent utilise le client construit au
// démarrage depuis la configuration — c'est le comportement historique, et
// c'est aussi le repli en cas d'erreur.
type ClientResolver interface {
	ResolveClient(ctx context.Context, role string, orgID model.OrgID) (llmclients.Resolved, error)
}

// ImageClientResolver fait de même pour la génération d'images, dont le
// modèle est distinct de celui qui converse. Un ClientResolver l'implémente
// aussi lorsqu'un pool d'images lui est branché.
type ImageClientResolver interface {
	ResolveImageClient(ctx context.Context, role string, orgID model.OrgID) (llm.ImageGenerationClient, error)
}

// imageBinding relie un agent à son rôle de génération d'images.
type imageBinding struct {
	role     string
	resolver ImageClientResolver
	logger   *slog.Logger
	// fallback est le générateur construit au démarrage ; nil quand l'agent
	// ne déclare pas de génération d'images.
	fallback llm.ImageGenerationClient
}

// resolve retourne le générateur à utiliser pour cette organisation, ou nil
// si l'agent n'en a aucun — auquel cas l'outil n'est pas monté.
func (b *imageBinding) resolve(ctx context.Context, orgID model.OrgID) llm.ImageGenerationClient {
	if b.resolver == nil || b.role == "" {
		return b.fallback
	}

	generator, err := b.resolver.ResolveImageClient(ctx, b.role, orgID)
	if err != nil {
		if b.logger != nil && b.fallback != nil {
			b.logger.WarnContext(ctx, "agent: générateur d'images non résolu, repli sur la configuration de démarrage",
				"role", b.role, "org", string(orgID), "error", err)
		}
		return b.fallback
	}

	return generator
}

// clientBinding relie un agent à son rôle dans le catalogue de modèles.
// Embarqué par chaque type d'agent, il tient toute la logique de repli en
// un seul endroit.
type clientBinding struct {
	role     string
	resolver ClientResolver
	logger   *slog.Logger
	// last est le dernier client résolu avec succès : le repli des pannes
	// TRANSITOIRES (base momentanément illisible), jamais celui d'un rôle
	// non configuré — qui doit se voir, pas se contourner.
	mu   sync.Mutex
	last llmclients.Resolved
}

// bind déclare le rôle de cet agent et le résolveur à interroger.
func (b *clientBinding) bind(resolver ClientResolver, role string, logger *slog.Logger) {
	b.resolver = resolver
	b.role = role
	b.logger = logger
}

// errNoResolver distingue « aucun résolveur câblé » (tests, agents
// construits à la main) d'un échec de résolution.
var errNoResolver = errors.New("agent: no client resolver wired")

// resolve retourne le client à utiliser pour cette organisation.
//
// Le YAML ne fournit plus de client de démarrage : le catalogue est LA
// source. Un rôle sans défaut d'instance rend l'erreur telle quelle — elle
// nomme le rôle et pointe vers l'administration. Une panne transitoire de
// lecture retombe sur le dernier client résolu, avec une trace : une base
// momentanément indisponible ne doit pas rendre l'assistant muet.
func (b *clientBinding) resolve(ctx context.Context, orgID model.OrgID) (llmclients.Resolved, error) {
	if b.resolver == nil || b.role == "" {
		return llmclients.Resolved{}, errNoResolver
	}

	resolved, err := b.resolver.ResolveClient(ctx, b.role, orgID)
	if err != nil {
		if errors.Is(err, llmclients.ErrNoDefault) {
			// Rôle non configuré : pas de repli, l'erreur doit remonter
			// jusqu'à quelqu'un qui peut la corriger.
			return llmclients.Resolved{}, err
		}

		b.mu.Lock()
		last := b.last
		b.mu.Unlock()

		if last.Client != nil {
			if b.logger != nil {
				b.logger.WarnContext(ctx, "agent: résolution du modèle en échec, repli sur le dernier client résolu",
					"role", b.role, "org", string(orgID), "error", err)
			}
			return last, nil
		}

		return llmclients.Resolved{}, err
	}

	b.mu.Lock()
	b.last = resolved
	b.mu.Unlock()

	return resolved, nil
}
