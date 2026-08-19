package web

import (
	"database/sql"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/bornholm/automata/internal/persistence"
)

// maxWebhookBody borne la lecture d'un événement Stripe : un corps
// anormalement gros est un abus, pas un paiement.
const maxWebhookBody = 1 << 20

// handleCheckout crée une session de paiement pour le pack choisi et
// redirige vers Stripe. Accessible depuis la page de crédits du profil,
// donc protégé par le lien temporaire comme le reste du profil.
func (s *Server) handleCheckout(w http.ResponseWriter, r *http.Request) {
	member, _, ok := s.resolveProfile(w, r)
	if !ok {
		return
	}
	if !checkCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}

	linkPath := "/p/" + r.PathValue("link")

	if s.stripe == nil {
		http.Redirect(w, r, linkPath+"/credits", http.StatusSeeOther)
		return
	}

	// Le pack est désigné par son rang dans la configuration : le
	// navigateur ne choisit jamais un montant ni un prix.
	index, err := strconv.Atoi(r.PostFormValue("pack"))
	if err != nil || index < 0 || index >= len(s.cfg.Web.Credits.Packs) {
		http.Redirect(w, r, linkPath+"/credits", http.StatusSeeOther)
		return
	}
	pack := s.cfg.Web.Credits.Packs[index]

	base := strings.TrimSuffix(s.cfg.Web.BaseURL, "/") + linkPath + "/credits"
	sessionURL, err := s.stripe.checkoutSession(r.Context(), member.OrgID, pack.Credits, pack.PriceEUR,
		base+"?paid=1", base+"?canceled=1")
	if err != nil {
		s.logger.ErrorContext(r.Context(), "web: création de session de paiement",
			"org_id", member.OrgID, "credits", pack.Credits, "error", err)
		http.Redirect(w, r, linkPath+"/credits?payment_error=1", http.StatusSeeOther)
		return
	}

	s.logger.InfoContext(r.Context(), "web: session de paiement ouverte",
		"org_id", member.OrgID, "member_id", member.ID, "credits", pack.Credits)

	http.Redirect(w, r, sessionURL, http.StatusSeeOther)
}

// handleStripeWebhook crédite le portefeuille à réception d'un paiement
// confirmé. Idempotent : la référence externe de la session est unique en
// base (migration 0011), un événement rejoué ne crédite jamais deux fois.
func (s *Server) handleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	if s.stripe == nil {
		http.NotFound(w, r)
		return
	}

	payload, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "corps illisible", http.StatusBadRequest)
		return
	}

	event, err := verifyStripeSignature(payload, r.Header.Get("Stripe-Signature"), s.cfg.Web.Stripe.WebhookSecret, s.now())
	if err != nil {
		// Journalisé sans le corps ni la signature : une tentative de
		// forge ne doit pas remplir les journaux de contenu attaquant.
		s.logger.WarnContext(r.Context(), "web: événement Stripe rejeté", "error", err)
		http.Error(w, "signature invalide", http.StatusBadRequest)
		return
	}

	// Tout autre événement est acquitté sans traitement : Stripe cesse
	// sinon de livrer, et nous n'attendons que celui-ci.
	if event.Type != "checkout.session.completed" || event.Data.Object.PaymentStatus != "paid" {
		w.WriteHeader(http.StatusOK)
		return
	}

	orgID := event.Data.Object.Metadata.OrgID
	credits, convErr := strconv.ParseInt(event.Data.Object.Metadata.Credits, 10, 64)
	if orgID == "" || convErr != nil || credits <= 0 {
		s.logger.ErrorContext(r.Context(), "web: paiement sans métadonnées exploitables", "session_id", event.Data.Object.ID)
		w.WriteHeader(http.StatusOK)
		return
	}

	credited := false
	err = s.db.WithTx(r.Context(), func(tx *sql.Tx) error {
		insertErr := s.wallet.Insert(r.Context(), tx, persistence.WalletEntry{
			OrgID:       orgID,
			Kind:        persistence.WalletKindPurchase,
			Label:       "Achat de crédits",
			Amount:      credits,
			CreatedAt:   s.now(),
			ExternalRef: event.Data.Object.ID,
		})
		if insertErr != nil {
			// Contrainte d'unicité : l'événement a déjà été traité.
			if isUniqueViolation(insertErr) {
				return nil
			}
			return insertErr
		}
		credited = true
		return nil
	})
	if err != nil {
		s.logger.ErrorContext(r.Context(), "web: échec du crédit d'un paiement",
			"org_id", orgID, "session_id", event.Data.Object.ID, "error", err)
		// 500 : Stripe réessaiera, et l'idempotence protège du doublon.
		http.Error(w, "erreur interne", http.StatusInternalServerError)
		return
	}

	if credited {
		s.logger.InfoContext(r.Context(), "web: paiement crédité",
			"org_id", orgID, "credits", credits, "session_id", event.Data.Object.ID)
	}

	w.WriteHeader(http.StatusOK)
}

// isUniqueViolation reconnaît une violation de contrainte d'unicité
// SQLite, sans dépendre du type d'erreur du pilote.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var target interface{ Error() string }
	if errors.As(err, &target) {
		return strings.Contains(strings.ToUpper(target.Error()), "UNIQUE")
	}
	return false
}
