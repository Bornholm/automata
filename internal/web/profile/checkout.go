package profile

import (
	"database/sql"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/web/core"
	"github.com/bornholm/automata/internal/weblink"
)

// maxWebhookBody borne la lecture d'un événement Stripe : un corps
// anormalement gros est un abus, pas un paiement.
const maxWebhookBody = 1 << 20

// checkoutReturnTTL laisse au paiement le temps d'aboutir. Le retour de
// Stripe ne peut pas viser le lien d'origine : celui-ci est à usage
// unique et déjà consommé, et la session courte du profil peut expirer
// pendant la saisie de la carte ou une authentification bancaire. Sans
// lien neuf, le client tombe sur « ce lien a déjà servi » au pire moment
// — juste après avoir payé.
const checkoutReturnTTL = time.Hour

// HandleCheckout crée une session de paiement pour le pack choisi et
// redirige vers Stripe. Accessible depuis la page de crédits du profil,
// donc protégé par le lien temporaire comme le reste du profil.
func (h *Handlers) HandleCheckout(w http.ResponseWriter, r *http.Request) {
	member, _, ok := h.resolveProfile(w, r)
	if !ok {
		return
	}
	if !core.CheckCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}

	linkPath := "/p/" + r.PathValue("link")

	if h.Stripe == nil {
		http.Redirect(w, r, linkPath+"/credits", http.StatusSeeOther)
		return
	}

	// Le pack est désigné par son rang dans la configuration : le
	// navigateur ne choisit jamais un montant ni un prix.
	index, err := strconv.Atoi(r.PostFormValue("pack"))
	if err != nil || index < 0 || index >= len(h.Cfg.Web.Credits.Packs) {
		http.Redirect(w, r, linkPath+"/credits", http.StatusSeeOther)
		return
	}
	pack := h.Cfg.Web.Credits.Packs[index]

	returnID, returnHash, returnPath, err := weblink.NewProfileLink()
	if err != nil {
		h.Logger.ErrorContext(r.Context(), "web: génération du lien de retour de paiement",
			"member_id", member.ID, "error", err)
		http.Redirect(w, r, linkPath+"/credits?payment_error=1", http.StatusSeeOther)
		return
	}
	now := h.Now()
	if !h.WithTx(w, r, func(tx *sql.Tx) error {
		return h.ProfileLinks.Insert(r.Context(), tx, persistence.ProfileLink{
			ID:        returnID,
			MemberID:  member.ID,
			TokenHash: returnHash,
			Status:    persistence.ProfileLinkStatusPending,
			ExpiresAt: now.Add(checkoutReturnTTL),
			CreatedAt: now,
		})
	}) {
		return
	}

	base := strings.TrimSuffix(h.Cfg.Web.BaseURL, "/") + "/p/" + returnPath + "/credits"
	sessionURL, err := h.Stripe.CheckoutSession(r.Context(), member.OrgID, member.ID, pack.Credits, pack.PriceEUR,
		base+"?paid=1", base+"?canceled=1")
	if err != nil {
		h.Logger.ErrorContext(r.Context(), "web: création de session de paiement",
			"org_id", member.OrgID, "credits", pack.Credits, "error", err)
		http.Redirect(w, r, linkPath+"/credits?payment_error=1", http.StatusSeeOther)
		return
	}

	h.Logger.InfoContext(r.Context(), "web: session de paiement ouverte",
		"org_id", member.OrgID, "member_id", member.ID, "credits", pack.Credits)

	http.Redirect(w, r, sessionURL, http.StatusSeeOther)
}

// HandleStripeWebhook crédite le portefeuille à réception d'un paiement
// confirmé. Idempotent : la référence externe de la session est unique en
// base (migration 0011), un événement rejoué ne crédite jamais deux fois.
func (h *Handlers) HandleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	if h.Stripe == nil {
		http.NotFound(w, r)
		return
	}

	payload, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "corps illisible", http.StatusBadRequest)
		return
	}

	event, err := core.VerifyStripeSignature(payload, r.Header.Get("Stripe-Signature"), h.Cfg.Web.Stripe.WebhookSecret, h.Now())
	if err != nil {
		// Journalisé sans le corps ni la signature : une tentative de
		// forge ne doit pas remplir les journaux de contenu attaquant.
		h.Logger.WarnContext(r.Context(), "web: événement Stripe rejeté", "error", err)
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
		h.Logger.ErrorContext(r.Context(), "web: paiement sans métadonnées exploitables", "session_id", event.Data.Object.ID)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Le prix est facultatif : les paiements antérieurs à son ajout n'en
	// portent pas, et un achat sans prix connu vaut mieux qu'un achat
	// perdu.
	priceEUR, _ := strconv.ParseFloat(event.Data.Object.Metadata.PriceEUR, 64)

	credited := false
	err = h.DB.WithTx(r.Context(), func(tx *sql.Tx) error {
		insertErr := h.Wallet.Insert(r.Context(), tx, persistence.WalletEntry{
			OrgID:       orgID,
			Kind:        persistence.WalletKindPurchase,
			Label:       "Achat de crédits",
			Amount:      credits,
			PriceEUR:    priceEUR,
			CreatedAt:   h.Now(),
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
		h.Logger.ErrorContext(r.Context(), "web: échec du crédit d'un paiement",
			"org_id", orgID, "session_id", event.Data.Object.ID, "error", err)
		// 500 : Stripe réessaiera, et l'idempotence protège du doublon.
		http.Error(w, "erreur interne", http.StatusInternalServerError)
		return
	}

	if credited {
		h.Logger.InfoContext(r.Context(), "web: paiement crédité",
			"org_id", orgID, "credits", credits, "session_id", event.Data.Object.ID)
		h.confirmPurchase(r, event.Data.Object.Metadata.MemberID, orgID, credits)
	}

	w.WriteHeader(http.StatusOK)
}

// confirmPurchase annonce l'achat dans la conversation privée de
// l'acheteur. Un échec d'envoi n'est jamais remonté à Stripe : le
// paiement est encaissé et les crédits sont inscrits, rejouer
// l'événement pour un message manqué ne réparerait rien.
func (h *Handlers) confirmPurchase(r *http.Request, memberID, orgID string, credits int64) {
	if h.Purchases == nil || memberID == "" {
		return
	}

	var balance int64
	if err := h.DB.WithTx(r.Context(), func(tx *sql.Tx) error {
		var err error
		balance, err = h.Wallet.Balance(r.Context(), tx, orgID)
		return err
	}); err != nil {
		h.Logger.ErrorContext(r.Context(), "web: solde indisponible pour la confirmation d'achat",
			"org_id", orgID, "error", err)
		return
	}

	if err := h.Purchases.NotifyPurchase(r.Context(), memberID, credits, balance); err != nil {
		h.Logger.WarnContext(r.Context(), "web: confirmation d'achat non remise",
			"org_id", orgID, "member_id", memberID, "error", err)
	}
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
