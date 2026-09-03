package conversation

import "regexp"

// Les liens de profil sont des SECRETS à usage unique : le segment
// « <id>.<secret> » vaut mot de passe pendant un quart d'heure. Ils n'ont
// donc rien à faire dans le contexte relu à chaque tour, pour deux raisons
// distinctes.
//
// La première tient au modèle. Malgré une consigne explicite dans le
// prompt ET dans la description de l'outil (« generate a fresh one every
// time, never repeat an old one »), l'orchestrateur finit par recopier un
// lien qu'il voit dans l'historique plutôt que d'appeler
// open_profile_link. Vu en production le 2026-08-23 : deux tours de suite
// terminés sans aucun appel d'outil, l'utilisateur recevant un lien
// périmé depuis une heure. C'est un travers connu des petits modèles, et
// une troisième formulation de la même consigne n'y changerait rien : ce
// qu'il ne voit pas, il ne peut pas le recopier.
//
// La seconde tient au fournisseur. Sans ce nettoyage, chaque tour renvoie
// au modèle TOUS les liens de la conversation, indéfiniment — des secrets
// d'accès à la page de profil, expédiés en boucle chez un tiers.
//
// Les messages ne sont pas modifiés en base : seul le texte remis au
// modèle l'est. La personne garde son lien dans sa messagerie.

// profileLinkPattern reconnaît un lien de profil : six caractères de
// l'alphabet Crockford, un point, vingt caractères (voir
// weblink.NewProfileLink). La forme est assez stricte pour qu'aucune autre
// URL ne s'y méprenne, et l'adresse complète est absorbée avec le segment
// — laisser « https://exemple.fr/p/ » tronqué serait pire que rien.
var profileLinkPattern = regexp.MustCompile(`(?i)(?:https?://\S+?)?/p/[0-9a-hjkmnp-tv-z]{6}\.[0-9a-hjkmnp-tv-z]{20}`)

// redactedProfileLink remplace le lien. Le marqueur est explicite plutôt
// que muet : il dit au modèle qu'un lien a bien été donné, donc qu'il doit
// en produire un neuf s'il en faut un autre.
//
// Il est rédigé en ANGLAIS et à l'impératif, comme toute consigne destinée
// au modèle. La première version, « [lien de profil déjà utilisé] », était
// en français : au milieu d'une conversation française, elle se lisait
// comme une réponse à recopier, et c'est exactement ce qui s'est produit —
// « Voici votre lien : [lien de profil déjà utilisé] », envoyé tel quel à
// la personne le 2026-09-03. Une annotation en anglais dans un fil
// français ne se confond plus avec le texte de la réponse.
//
// Le modèle peut malgré tout la recopier : c'est pourquoi l'hôte vérifie
// la réponse sortante avant de l'envoyer (repairRedactedProfileLink).
const redactedProfileLink = "[expired profile link removed — call open_profile_link to issue a new one]"

// redactProfileLinks retire les liens de profil d'un texte destiné au
// modèle.
func redactProfileLinks(text string) string {
	if text == "" {
		return text
	}
	return profileLinkPattern.ReplaceAllString(text, redactedProfileLink)
}

// redactedProfileLinkPattern reconnaît le marqueur, y compris ALTÉRÉ :
// crochet fermant absent, casse changée, fin tronquée ou reformulée.
//
// La comparaison exacte ne suffit pas, et c'est la panne du 2026-09-03 :
// le modèle a envoyé « Voici un nouveau lien […] : [expired profile link
// removed — call open_profile_link to issue a new one » sans son crochet
// fermant. strings.Contains n'a rien reconnu, la réparation ne s'est pas
// déclenchée, et la personne a reçu le marqueur au lieu d'un lien. Un
// modèle ne recopie pas au caractère près : le motif s'accroche donc au
// début, seul repère stable.
var redactedProfileLinkPattern = regexp.MustCompile(`(?i)\[\s*expired profile link removed[^\]]{0,120}\]?`)

// containsRedactedProfileLink dit si un texte porte le marqueur, sous une
// forme même approximative. Utilisé sur la réponse SORTANTE : le marqueur
// ne s'adresse qu'au modèle, une personne ne doit jamais le lire.
func containsRedactedProfileLink(text string) bool {
	return redactedProfileLinkPattern.MatchString(text)
}
