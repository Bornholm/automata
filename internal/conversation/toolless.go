package conversation

import "strings"

// Rupture de la boucle de refus.
//
// Un refus que l'assistant a inventé ne reste pas sans suite : il retourne
// dans l'historique, le modèle l'y relit au tour suivant et le recopie
// plutôt que d'essayer. Chaque refus en engendre donc un autre, et la
// conversation s'enferme — jusqu'à ce que quelqu'un ordonne explicitement
// « utilise ton outil », ce qu'aucun message ordinaire ne dit.
//
// Constaté en production le 2026-09-03 : sept tours d'affilée terminés sans
// aucun appel d'outil sur une demande de lien de profil, deux liens
// entièrement inventés au passage, alors que open_profile_link figurait
// dans les vingt-quatre outils offerts à chaque tour. La sonde
// (automata admin probe-tools) l'a reproduit ensuite hors conversation :
// tous les étages passent, SAUF celui qui injecte un refus dans
// l'historique — trois échecs sur six, où le modèle recrache le refus
// injecté quasiment mot pour mot.
//
// Le prompt interdit déjà tout cela, deux fois plutôt qu'une. Une troisième
// formulation de la même consigne n'y changerait rien : ce qui agit, c'est
// ce que le modèle relit juste avant de répondre. On annote donc le message
// lui-même, à l'endroit où il est relu, comme redact.go le fait déjà pour
// les liens de profil périmés.
//
// Ce qui est annoté ne dépend PAS de ce que le message dit. Reconnaître un
// refus à ses mots demanderait un lexique, et un lexique se trompe dans les
// deux sens : il manque le refus déguisé en constat (« le service n'a pas
// accepté la requête ») et se déclenche sur des phrases anodines. La seule
// chose vérifiable ici est structurelle — ce tour disposait d'outils et n'en
// a appelé aucun — et c'est elle, et elle seule, qui décide.
//
// Les messages ne sont pas modifiés en base : seul le texte remis au modèle
// l'est. La personne garde sa conversation telle qu'elle l'a lue.

// toollessMarker est accolé, dans l'historique remis au modèle, à chaque
// réponse écrite sans qu'aucun outil ait été appelé.
//
// C'est un CONSTAT, pas une consigne : il énonce un fait que le modèle n'a
// aucun moyen de retrouver seul, et n'ordonne rien. C'est ce qui lui permet
// d'être posé partout où il est vrai — sur une salutation comme sur un
// refus — sans jamais se tromper. La conséquence à en tirer est écrite une
// fois pour toutes dans les règles d'honnêteté du prompt (voir
// agent.honestyRules), là où elle est lue en entier plutôt que répétée à
// chaque ligne.
//
// En ANGLAIS, comme tout ce qui s'adresse au modèle (AGENTS.md), et pour la
// même raison que redactedProfileLink : au milieu d'un fil français, une
// annotation française se lit comme une phrase à recopier, et finit dans la
// réponse envoyée à la personne.
const toollessMarker = " [no tool was called for this message]"

// annotateToolless accole le constat au texte lorsque le tour qui l'a
// produit disposait d'outils et n'en a appelé aucun.
//
// answeredWithoutTools vient de la persistance et ne vaut que pour les
// messages "assistant" produits par un modèle disposant d'outils : une
// réponse écrite par l'hôte (visite d'accueil, confirmation d'action) ou par
// un agent sans outils est enregistrée à faux, et n'est donc jamais annotée.
// C'est voulu — pour celles-là, le constat serait faux ou vide de sens.
func annotateToolless(text string, answeredWithoutTools bool) string {
	if !answeredWithoutTools || text == "" {
		return text
	}

	return text + toollessMarker
}

// containsToollessMarker dit si un texte porte le constat. Utilisé sur la
// réponse SORTANTE : comme le marqueur de caviardage, il ne s'adresse qu'au
// modèle, et une personne ne doit jamais le lire.
func containsToollessMarker(text string) bool {
	return strings.Contains(text, strings.TrimSpace(toollessMarker))
}

// stripToollessMarker retire le constat d'un texte destiné à une personne.
//
// Le modèle peut le recopier : c'est exactement ce qui est arrivé au
// marqueur de caviardage, en français, le 2026-09-03 (voir redact.go). La
// rédaction anglaise rend la recopie moins probable, pas impossible — et un
// filet qui coûte une comparaison de chaîne se met avant l'incident, pas
// après.
func stripToollessMarker(text string) string {
	if !containsToollessMarker(text) {
		return text
	}

	cleaned := strings.ReplaceAll(text, strings.TrimSpace(toollessMarker), "")

	return strings.TrimSpace(cleaned)
}
