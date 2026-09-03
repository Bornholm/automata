package main

import (
	"fmt"
	"regexp"
	"runtime"
	"slices"
	"strings"
)

// Rendu des patrons {{nom}}, mêmes règles que le gestionnaire MCP de
// l'hôte (internal/config/mcp_template.go), recopiées ici : un plugin ne
// dépend jamais du module de l'hôte.
//
// Deux règles tiennent tout :
//
//  1. un patron sans valeur est une ERREUR, jamais un passage tel quel —
//     lancer une commande avec un {{token}} littéral au mieux échoue de
//     façon obscure, au pire se connecte ailleurs que prévu ;
//  2. l'erreur ne cite que les NOMS. Les valeurs sont des identifiants de
//     la personne : elles ne partent ni dans un journal, ni au modèle, ni
//     dans un message d'erreur.
var templatePattern = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_]+)\s*\}\}`)

// renderTemplate remplace chaque patron {{nom}} de s par values[nom] et
// retourne la chaîne rendue ainsi que les noms sans valeur, dans l'ordre
// d'apparition.
func renderTemplate(s string, values map[string]string) (string, []string) {
	var missing []string

	rendered := templatePattern.ReplaceAllStringFunc(s, func(match string) string {
		name := templatePattern.FindStringSubmatch(match)[1]
		value, ok := values[name]
		if !ok {
			missing = append(missing, name)
			return match
		}
		return value
	})

	return rendered, missing
}

// templateNamesIn retourne les noms de patrons présents dans texts,
// dédupliqués et triés.
func templateNamesIn(texts ...string) []string {
	seen := map[string]bool{}
	var names []string

	for _, text := range texts {
		for _, match := range templatePattern.FindAllStringSubmatch(text, -1) {
			if !seen[match[1]] {
				seen[match[1]] = true
				names = append(names, match[1])
			}
		}
	}

	slices.Sort(names)

	return names
}

// hostValues sont les patrons fournis par le plugin, sans rien demander
// au membre. La liste s'étoffe avec l'installation des serveurs stdio
// ({{bin}}, {{files}}, {{version}}) ; ces deux-là servent déjà à composer
// l'URL d'un téléchargement par plateforme.
func hostValues() map[string]string {
	return map[string]string{
		"os":   runtime.GOOS,
		"arch": runtime.GOARCH,
	}
}

// errMissingPlaceholders transforme des noms non résolus en une erreur
// unique, dédupliquée et triée. nil si la liste est vide.
func errMissingPlaceholders(missing []string) error {
	if len(missing) == 0 {
		return nil
	}

	slices.Sort(missing)
	missing = slices.Compact(missing)

	return fmt.Errorf("identifiants manquants ({{%s}})", strings.Join(missing, "}}, {{"))
}
