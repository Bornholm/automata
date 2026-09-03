// The subagents plugin gives each member a catalogue of specialised
// sub-agents to turn on from their profile. Every entry — its prompt, its
// description and the MCP servers it draws its tools from — comes from a
// YAML catalogue owned by the operator; the member only picks from it and
// supplies their own credentials, which are what keeps one member's tools
// out of another's reach.
package main

import (
	"log/slog"
	"os"

	"github.com/bornholm/automata/pkg/pluginsdk"
)

func main() {
	// Un catalogue illisible arrête le plugin. C'est délibéré : chargé à
	// moitié, il laisserait un membre activer un agent qui ne se montera
	// jamais, sans que rien ne le dise.
	cat, err := loadCatalog()
	if err != nil {
		slog.Error("subagents: catalogue invalide", "error", err)
		os.Exit(1)
	}

	plugin := newPlugin(cat)
	pluginsdk.ServeWithUI(plugin, "subagents", plugin.newUIHandler())
}
