package agent

import (
	"regexp"
	"strings"
)

// reasoningPrefix reconnaît un bloc de raisonnement laissé EN TÊTE de la
// réponse par certains modèles à réflexion (<think>, <thinking>,
// <reasoning>), quand le fournisseur ne le sépare pas du contenu.
//
// Seul un bloc de tête est retiré, et seulement s'il est refermé : au milieu
// d'une réponse, ces balises sont plus probablement du texte légitime (une
// question sur du HTML, un extrait de code), et une balise jamais refermée
// signalerait un flux tronqué — mieux vaut alors livrer la réponse telle
// quelle que la vider entièrement.
var reasoningPrefix = regexp.MustCompile(`(?is)^\s*<(think|thinking|reasoning)>.*?</(think|thinking|reasoning)>`)

// cleanReply prépare le texte produit par un modèle avant persistance et
// envoi.
//
// Les modèles à réflexion font précéder leur réponse de leur raisonnement.
// Quand le fournisseur l'expose dans un champ séparé, il ne reste que les
// sauts de ligne qui l'entouraient : le message arrive alors sur WhatsApp
// avec une ou deux lignes vides avant le premier mot. Quand il ne le sépare
// pas, le bloc <think> lui-même se retrouve dans le message.
//
// Le nettoyage est appliqué à tous les chemins de réponse (conversation,
// délégation, tâche planifiée, exécution planifiée) : c'est une propriété du
// texte livré, pas d'un cas d'usage particulier.
func cleanReply(text string) string {
	return strings.TrimSpace(reasoningPrefix.ReplaceAllString(text, ""))
}
