package conversation

import (
	"regexp"
	"strings"
)

// Ce qui est écrit DANS un message est recopié.
//
// Un refus que l'assistant a inventé retourne dans l'historique, le modèle
// l'y relit au tour suivant et le recopie plutôt que d'essayer : la panne
// s'entretient d'elle-même (voir docs/operations.md, « La boucle de refus »).
// La première réponse à ce travers a été d'accoler un constat au message
// fautif, « [no tool was called for this message] », dans l'historique
// remis au modèle.
//
// C'était une erreur, et elle s'est vue en production le 2026-09-03 : le
// modèle a recopié le constat dans sa réponse, TRONQUÉ de son crochet
// fermant, et la personne a lu « Le service de calendrier est indisponible.
// Réessayez plus tard. [no tool was called for this message ». Le filet de
// sortie comparait la chaîne exacte : il n'a rien vu passer.
//
// La leçon est structurelle, et elle valait déjà pour le caviardage des
// liens (voir redact.go) : tout texte glissé dans le contenu d'un message
// d'assistant finit un jour dans la réponse envoyée à quelqu'un, et aucun
// filtre de sortie ne rattrapera toutes les variantes qu'un modèle en
// produit. Le fait ne s'annote donc plus dans les messages : il s'énonce
// dans le message SYSTÈME, une fois, hors de tout ce que le modèle
// s'attend à recopier (voir agent.toollessNotice).
//
// Ne reste ici que le nettoyage — un filet, désormais tolérant à
// l'altération, et appliqué aux deux bouts : sur les réponses sortantes, et
// sur l'historique relu, où les recopies déjà enregistrées doivent
// disparaître.

// toollessMarkerPattern reconnaît le constat, y compris altéré : crochet
// fermant absent, casse changée, texte tronqué ou rallongé.
//
// Comparer la chaîne exacte est précisément ce qui a échoué. Un modèle ne
// recopie pas au caractère près : il tronque, reformule la fin, change une
// majuscule. Le motif s'accroche donc au début, seul repère stable, et
// avale ce qui suit jusqu'au crochet fermant s'il existe.
var toollessMarkerPattern = regexp.MustCompile(`(?i)\s*\[\s*no tool was called[^\]]{0,120}\]?`)

// containsToollessMarker dit si un texte porte le constat, sous une forme
// même approximative.
func containsToollessMarker(text string) bool {
	return toollessMarkerPattern.MatchString(text)
}

// stripToollessMarker retire le constat d'un texte.
//
// Appliqué à une réponse sortante, il évite qu'une personne le lise.
// Appliqué à l'historique, il efface les recopies déjà enregistrées en
// base : elles y sont, la base n'est jamais réécrite, et les relire au
// modèle serait lui montrer un modèle de réponse à imiter.
func stripToollessMarker(text string) string {
	if !containsToollessMarker(text) {
		return text
	}

	return strings.TrimSpace(toollessMarkerPattern.ReplaceAllString(text, ""))
}
