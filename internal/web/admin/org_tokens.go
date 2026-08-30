package admin

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/web/core"
	"github.com/bornholm/automata/internal/weblink"
)

func (h *Handlers) HandleOrgGroupToken(w http.ResponseWriter, r *http.Request) {
	h.createGroupToken(w, r, r.PathValue("id"), "/admin/orgs/"+r.PathValue("id"))
}

// createGroupToken génère un jeton de groupe pour orgID et redirige vers
// redirectBase avec la clé de révélation.

// createGroupToken génère un jeton de groupe pour orgID et redirige vers
// redirectBase avec la clé de révélation.
func (h *Handlers) createGroupToken(w http.ResponseWriter, r *http.Request, orgID, redirectBase string) {
	now := h.Now()

	clear, hash, display, err := weblink.NewLinkToken()
	if err != nil {
		h.Logger.ErrorContext(r.Context(), "web: génération d'un jeton de groupe", "error", err)
		http.Error(w, "erreur interne", http.StatusInternalServerError)
		return
	}

	tokenID, err := weblink.RandomCrockford(10)
	if err != nil {
		http.Error(w, "erreur interne", http.StatusInternalServerError)
		return
	}

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		return h.LinkTokens.Insert(r.Context(), tx, persistence.LinkToken{
			ID:        strings.ToLower(tokenID),
			Kind:      persistence.LinkTokenKindGroup,
			OrgID:     orgID,
			TokenHash: hash,
			Status:    persistence.LinkTokenStatusPending,
			ExpiresAt: now.AddDate(0, 0, 7),
			CreatedAt: now,
		})
	})
	if !ok {
		return
	}

	key, err := h.Reveals.Put(core.RevealValue{Clear: clear, Display: display}, now)
	if err != nil {
		http.Error(w, "erreur interne", http.StatusInternalServerError)
		return
	}

	h.Logger.InfoContext(r.Context(), "web: jeton de groupe généré", "org_id", orgID, "token_id", tokenID)

	separator := "?"
	if strings.Contains(redirectBase, "?") {
		separator = "&"
	}
	http.Redirect(w, r, redirectBase+separator+"reveal="+key, http.StatusFound)
}

// HandleOrgWalletCSV exporte les mouvements du portefeuille.
