package conversation

import (
	"context"
	"log/slog"
	"strings"

	"github.com/bornholm/automata/internal/model"
)

// Réparation d'une réponse où le modèle a recopié le marqueur de
// caviardage au lieu d'appeler open_profile_link.
//
// Le caviardage (redact.go) retire de l'historique les liens de profil, qui
// sont des secrets à usage unique, et laisse à leur place une consigne
// destinée au modèle. Un modèle assez faible recopie cette consigne dans sa
// réponse — vu en production le 2026-09-03 : « Voici votre lien :
// [lien de profil déjà utilisé] », envoyé tel quel à la personne.
//
// Reformuler la consigne une fois de plus ne réglerait rien : c'est le
// constat que redact.go faisait déjà pour les liens périmés. L'hôte
// vérifie donc la réponse SORTANTE, et fait lui-même ce que le modèle
// voulait faire — le lien est produit par le même générateur, pour la même
// identité, celle du tour. Le modèle n'a jamais eu son mot à dire sur le
// destinataire d'un lien, et il ne l'a pas davantage ici.

// repairFallback remplace le marqueur quand aucun lien ne peut être
// produit. La personne doit savoir que quelque chose a manqué, plutôt que
// de lire une phrase amputée.
const repairFallback = "je n'ai pas réussi à générer le lien, redemande-le moi"

// repairRedactedProfileLink rend une réponse expurgée du marqueur. Elle
// retourne le texte inchangé dans le cas nominal, où le marqueur est
// absent.
func (h *Handler) repairRedactedProfileLink(ctx context.Context, identity model.ExecutionIdentity, reply string) string {
	if !containsRedactedProfileLink(reply) {
		return reply
	}

	h.metrics.IncProfileLinkRepair()

	replacement := repairFallback
	if h.profileLinks != nil {
		url, ok, err := h.profileLinks.GenerateProfileLink(ctx, string(identity.OrgID), string(identity.PrincipalID))
		switch {
		case err != nil:
			h.warn(ctx, "conversation: génération du lien de profil de réparation en échec",
				"org_id", identity.OrgID, "principal_id", identity.PrincipalID, "error", err)
		case ok:
			replacement = url
		}
	}

	h.warn(ctx, "conversation: marqueur de lien de profil recopié par le modèle, réponse réparée",
		"org_id", identity.OrgID,
		"principal_id", identity.PrincipalID,
		"repaired", replacement != repairFallback,
		"remède", "le modèle affecté au rôle principal n'appelle pas open_profile_link de façon fiable")

	return strings.ReplaceAll(reply, redactedProfileLink, replacement)
}

// warn journalise sans exiger que le logger soit renseigné : le Handler
// n'en reçoit un qu'avec le compactor.
func (h *Handler) warn(ctx context.Context, msg string, args ...any) {
	logger := h.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.WarnContext(ctx, msg, args...)
}
