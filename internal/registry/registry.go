// Package registry assemble les services applicatifs d'Automata
// (configuration, ingress, scheduler, agents, mémoire, persistance) et
// pilote leur cycle de vie.
//
// À ce stade, aucun service n'est encore câblé : Run se contente de
// démarrer proprement et de s'arrêter à l'annulation du contexte. Les
// phases suivantes y brancheront progressivement les composants réels.
package registry

import (
	"context"
	"log/slog"
)

// Run démarre les services applicatifs et bloque jusqu'à l'annulation du
// contexte fourni, puis s'arrête proprement.
func Run(ctx context.Context, logger *slog.Logger) error {
	logger.InfoContext(ctx, "automata starting")

	<-ctx.Done()

	logger.InfoContext(ctx, "automata stopping")

	return nil
}
