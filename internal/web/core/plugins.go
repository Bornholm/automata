package core

import (
	"context"
	"io"

	"github.com/bornholm/automata/internal/plugin"
)

// Vues d'une interface de plugin : la même page sert l'opérateur et le
// membre, le jeton dit laquelle.
const (
	PluginViewAdmin  = "admin"
	PluginViewMember = "member"
	// PluginViewPublic ne dessert que le retour OAuth : aucune identité,
	// aucun jeton, un seul chemin atteignable.
	PluginViewPublic = "public"

	// PluginUIPrefix préfixe toutes les interfaces de plugins, quelle que
	// soit la vue : un seul chemin à reconnaître pour l'exemption CSRF, un
	// seul endroit où l'authentification par jeton s'applique.
	PluginUIPrefix = "/plugin-ui/"
)

// PluginManager est la vue du gestionnaire de plugins dont l'écran
// d'administration a besoin. Interface déclarée côté consommateur, comme
// PlatformManager.
type PluginManager interface {
	Statuses() []plugin.Status
	Restart(ctx context.Context, name string) bool
	// OpenFile sert les liens de téléchargement : les octets viennent du
	// plugin EN FLUX, ils ne passent jamais par la mémoire du serveur.
	OpenFile(ctx context.Context, pluginName string, callCtx plugin.CallContext, path string) (plugin.FileMeta, io.ReadCloser, error)
}
