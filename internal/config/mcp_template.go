package config

import (
	"regexp"

	"slices"
)

// mcpTemplatePattern reconnaît les patrons {{nom}} utilisés dans Command et
// Env d'un serveur MCP stdio (espaces intérieurs tolérés : {{ nom }}). Le
// nom est volontairement restreint à [A-Za-z0-9_] : les valeurs viennent de
// la configuration statique, jamais du modèle ni de la conversation, et une
// syntaxe minimale écarte toute tentation d'en faire un langage.
var mcpTemplatePattern = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_]+)\s*\}\}`)

// RenderMCPTemplate remplace chaque patron {{nom}} de s par values[nom] et
// retourne la chaîne rendue ainsi que les noms de patrons SANS valeur, dans
// l'ordre d'apparition. L'appelant décide d'en faire une erreur : la
// validation les signale par principal, le manager MCP refuse de lancer une
// commande incomplète. Les noms manquants peuvent être journalisés sans
// risque ; les valeurs, elles, sont des secrets potentiels.
func RenderMCPTemplate(s string, values map[string]string) (string, []string) {
	var missing []string

	rendered := mcpTemplatePattern.ReplaceAllStringFunc(s, func(match string) string {
		name := mcpTemplatePattern.FindStringSubmatch(match)[1]
		value, ok := values[name]
		if !ok {
			missing = append(missing, name)
			return match
		}
		return value
	})

	return rendered, missing
}

// TemplatePlaceholders retourne les noms de patrons {{nom}} utilisés par la
// commande et l'environnement du serveur, dédupliqués et triés.
func (s MCPServer) TemplatePlaceholders() []string {
	seen := map[string]bool{}
	var names []string

	collect := func(text string) {
		for _, match := range mcpTemplatePattern.FindAllStringSubmatch(text, -1) {
			if !seen[match[1]] {
				seen[match[1]] = true
				names = append(names, match[1])
			}
		}
	}

	for _, arg := range s.Command {
		collect(arg)
	}
	for _, key := range sortedKeys(s.Env) {
		collect(s.Env[key])
	}

	slices.Sort(names)

	return names
}
