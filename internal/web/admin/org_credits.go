package admin

import (
	"database/sql"
	"encoding/csv"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bornholm/automata/internal/persistence"
)

func (h *Handlers) HandleOrgGrant(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("id")
	amount, _ := strconv.ParseInt(r.PostFormValue("amount"), 10, 64)
	label := strings.TrimSpace(r.PostFormValue("label"))
	if amount <= 0 || label == "" {
		http.Redirect(w, r, "/admin/orgs/"+orgID, http.StatusFound)
		return
	}

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		return h.Wallet.Insert(r.Context(), tx, persistence.WalletEntry{
			OrgID:     orgID,
			Kind:      persistence.WalletKindGrant,
			Label:     label,
			Amount:    amount,
			CreatedAt: h.Now(),
		})
	})
	if !ok {
		return
	}

	h.Logger.InfoContext(r.Context(), "web: crédits offerts", "org_id", orgID, "amount", amount)
	http.Redirect(w, r, "/admin/orgs/"+orgID+"?granted=1", http.StatusFound)
}

func (h *Handlers) HandleOrgOffered(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("id")
	offered := r.PostFormValue("offered") == "true"
	allowance, _ := strconv.ParseInt(r.PostFormValue("allowance"), 10, 64)

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		org, exists, err := h.Orgs.FindByID(r.Context(), tx, orgID)
		if err != nil || !exists {
			return err
		}
		org.Offered = offered
		if allowance >= 0 && offered {
			org.MonthlyAllowance = allowance
		}
		org.UpdatedAt = h.Now()
		if err := h.Orgs.Update(r.Context(), tx, org); err != nil {
			return err
		}

		// Apport immédiat : sans lui, une organisation qu'on vient d'offrir
		// devrait attendre le 1er du mois suivant pour recevoir ses crédits
		// — et son service resterait en pause jusque-là.
		if !org.Offered || org.MonthlyAllowance <= 0 {
			return nil
		}

		balance, err := h.Wallet.Balance(r.Context(), tx, orgID)
		if err != nil {
			return err
		}
		if balance >= org.MonthlyAllowance {
			return nil
		}

		return h.Wallet.Insert(r.Context(), tx, persistence.WalletEntry{
			OrgID:     orgID,
			Kind:      persistence.WalletKindAllowance,
			Label:     "Allocation mensuelle offerte",
			Amount:    org.MonthlyAllowance - balance,
			CreatedAt: h.Now(),
		})
	})
	if !ok {
		return
	}

	http.Redirect(w, r, "/admin/orgs/"+orgID, http.StatusFound)
}

// HandleOrgUnlimited bascule le mode gratuit sans limite : l'organisation
// n'est plus jamais débitée ni mise en pause, et son allocation mensuelle
// devient sans objet. Sa consommation reste mesurée : le coût réel demeure
// visible dans les écrans d'usage et de marge.

// HandleOrgUnlimited bascule le mode gratuit sans limite : l'organisation
// n'est plus jamais débitée ni mise en pause, et son allocation mensuelle
// devient sans objet. Sa consommation reste mesurée : le coût réel demeure
// visible dans les écrans d'usage et de marge.
func (h *Handlers) HandleOrgUnlimited(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("id")
	unlimited := r.PostFormValue("unlimited") == "true"

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		org, exists, err := h.Orgs.FindByID(r.Context(), tx, orgID)
		if err != nil || !exists {
			return err
		}
		org.Unlimited = unlimited
		org.UpdatedAt = h.Now()

		return h.Orgs.Update(r.Context(), tx, org)
	})
	if !ok {
		return
	}

	h.Logger.InfoContext(r.Context(), "web: mode gratuit sans limite modifié",
		"org", orgID, "unlimited", unlimited)

	http.Redirect(w, r, "/admin/orgs/"+orgID, http.StatusFound)
}

// HandleOrgWalletCSV exporte les mouvements du portefeuille.
func (h *Handlers) HandleOrgWalletCSV(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("id")

	var entries []persistence.WalletEntry
	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		var err error
		entries, err = h.Wallet.List(r.Context(), tx, orgID, 0)
		return err
	})
	if !ok {
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="portefeuille-`+orgID+`.csv"`)

	writer := csv.NewWriter(w)
	_ = writer.Write([]string{"date", "nature", "libelle", "montant"})
	for _, entry := range entries {
		_ = writer.Write([]string{
			entry.CreatedAt.UTC().Format(time.RFC3339),
			entry.Kind,
			entry.Label,
			strconv.FormatInt(entry.Amount, 10),
		})
	}
	writer.Flush()
}

// HandleOrgCustomization enregistre la personnalisation d'une
// organisation : consigne ajoutée, spécialistes conservés, plafond
// d'outils.
