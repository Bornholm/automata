package admin

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/web/core"
	"github.com/bornholm/automata/internal/web/view"
	"github.com/bornholm/automata/internal/weblink"
)

// orgSubtitle décrit les canaux d'une organisation
// (« WhatsApp · 2 canaux »).
//
// Les deux sources comptent : les canaux déclarés en configuration et
// ceux rattachés en ligne par jeton de liaison. N'en compter qu'une
// affichait « Aucun canal lié » à des organisations dont toutes les
// conversations étaient rattachées — la liste contredisait alors l'écran
// des canaux.
func (h *Handlers) orgSubtitle(orgID string, bound []persistence.ChannelBinding) string {
	var count int
	firstType := ""
	for _, ch := range h.Cfg.Channels {
		if ch.OrgID != orgID {
			continue
		}
		count++
		if firstType == "" {
			firstType = h.ProviderTypeOf(ch.Provider)
		}
	}

	for _, binding := range bound {
		if binding.OrgID != orgID {
			continue
		}
		count++
		if firstType == "" {
			firstType = h.ProviderTypeOf(binding.Provider)
		}
	}

	if count == 0 {
		return "Aucun canal lié"
	}

	label := "canaux"
	if count == 1 {
		label = "canal"
	}
	return fmt.Sprintf("%s · %d %s", core.PlatformDisplayName(firstType, firstType), count, label)
}

// HandleOrgs — ADM-02.

// HandleOrgs — ADM-02.
func (h *Handlers) HandleOrgs(w http.ResponseWriter, r *http.Request) {
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	now := h.Now()
	monthFrom, monthTo := core.MonthBounds(now)

	page := view.OrgsPage{
		Search:    search,
		Platforms: h.SidebarPlatforms(),
		CSRFToken: h.CSRFToken(w, r),
	}

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		orgs, err := h.Orgs.List(r.Context(), tx, search)
		if err != nil {
			return err
		}
		balances, err := h.Wallet.Balances(r.Context(), tx)
		if err != nil {
			return err
		}
		memberCounts, err := h.Members.CountByOrg(r.Context(), tx)
		if err != nil {
			return err
		}
		bound, err := h.Bindings.ListAll(r.Context(), tx)
		if err != nil {
			return err
		}
		monthUsage, err := h.OrgUsageCredits(r.Context(), tx, monthFrom, monthTo)
		if err != nil {
			return err
		}

		for _, org := range orgs {
			lastCredit, err := h.Wallet.LastCredit(r.Context(), tx, org.ID)
			if err != nil {
				return err
			}

			state := core.ComputeWalletState(org, balances[org.ID], lastCredit, monthUsage[org.ID])

			usageLabel := "—"
			if monthUsage[org.ID] > 0 {
				usageLabel = view.FormatCredits(monthUsage[org.ID])
			}

			page.Rows = append(page.Rows, view.OrgRow{
				ID:           org.ID,
				Name:         org.DisplayName,
				Subtitle:     h.orgSubtitle(org.ID, bound),
				Chip:         state.Chip,
				BalanceLabel: state.BalanceLabel,
				GaugePct:     state.GaugePct,
				GaugeTone:    state.GaugeTone,
				Members:      memberCounts[org.ID],
				MonthUsage:   usageLabel,
				CreatedAt:    view.FormatShortDate(org.CreatedAt),
				RowTone:      state.RowTone,
			})
			if state.RowTone != "" {
				page.Flagged++
			}
		}
		page.Total = len(page.Rows)

		return nil
	})
	if !ok {
		return
	}

	h.Render(w, r, http.StatusOK, view.AdminOrgs(page))
}

func (h *Handlers) HandleOrgNewForm(w http.ResponseWriter, r *http.Request) {
	h.Render(w, r, http.StatusOK, view.AdminOrgNew(view.OrgNewPage{
		Platforms: h.SidebarPlatforms(),
		CSRFToken: h.CSRFToken(w, r),
	}))
}

// slugify dérive un identifiant stable d'un nom affiché.

// slugify dérive un identifiant stable d'un nom affiché.
func slugify(name string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case r == 'é' || r == 'è' || r == 'ê' || r == 'ë':
			b.WriteByte('e')
			lastDash = false
		case r == 'à' || r == 'â':
			b.WriteByte('a')
			lastDash = false
		case r == 'î' || r == 'ï':
			b.WriteByte('i')
			lastDash = false
		case r == 'ô' || r == 'ö':
			b.WriteByte('o')
			lastDash = false
		case r == 'ù' || r == 'û' || r == 'ü':
			b.WriteByte('u')
			lastDash = false
		case r == 'ç':
			b.WriteByte('c')
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}

	return strings.Trim(b.String(), "-")
}

func (h *Handlers) HandleOrgCreate(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("display_name"))
	if name == "" {
		h.Render(w, r, http.StatusBadRequest, view.AdminOrgNew(view.OrgNewPage{
			Platforms: h.SidebarPlatforms(),
			CSRFToken: h.CSRFToken(w, r),
			Error:     "Le nom de l'organisation est requis.",
		}))
		return
	}

	welcome, _ := strconv.ParseInt(r.PostFormValue("welcome_credits"), 10, 64)
	ownerName := strings.TrimSpace(r.PostFormValue("owner_name"))
	now := h.Now()

	orgID := slugify(name)
	if orgID == "" {
		orgID = "org"
	}

	// Deux organisations de même nom sont indiscernables partout où on
	// les liste : dans le sélecteur des jetons de liaison comme dans la
	// liste d'administration. Le doublon vient presque toujours d'une
	// création refaite, pas d'une intention — mieux vaut le dire que le
	// laisser passer et devoir démêler ensuite quel canal est allé où.
	var duplicate bool
	if !h.WithTx(w, r, func(tx *sql.Tx) error {
		existing, err := h.Orgs.List(r.Context(), tx, "")
		if err != nil {
			return err
		}
		for _, org := range existing {
			if strings.EqualFold(strings.TrimSpace(org.DisplayName), name) {
				duplicate = true
				break
			}
		}

		return nil
	}) {
		return
	}
	if duplicate {
		h.Render(w, r, http.StatusConflict, view.AdminOrgNew(view.OrgNewPage{
			Platforms: h.SidebarPlatforms(),
			CSRFToken: h.CSRFToken(w, r),
			Error:     "Une organisation nommée « " + name + " » existe déjà. Choisissez un autre nom, ou ouvrez l'existante.",
		}))
		return
	}

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		// Suffixe numérique en cas de collision d'identifiant.
		candidate := orgID
		for i := 2; ; i++ {
			_, exists, err := h.Orgs.FindByID(r.Context(), tx, candidate)
			if err != nil {
				return err
			}
			if !exists {
				break
			}
			candidate = fmt.Sprintf("%s-%d", orgID, i)
		}
		orgID = candidate

		org := persistence.Organization{
			ID:          orgID,
			DisplayName: name,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if err := h.Orgs.Insert(r.Context(), tx, org, false); err != nil {
			return err
		}

		if welcome > 0 {
			if err := h.Wallet.Insert(r.Context(), tx, persistence.WalletEntry{
				OrgID:     orgID,
				Kind:      persistence.WalletKindWelcome,
				Label:     "Crédits de bienvenue",
				Amount:    welcome,
				CreatedAt: now,
			}); err != nil {
				return err
			}
		}

		if ownerName != "" {
			memberID, err := weblink.RandomCrockford(10)
			if err != nil {
				return err
			}
			return h.Members.Insert(r.Context(), tx, persistence.Member{
				ID:          strings.ToLower(memberID),
				OrgID:       orgID,
				DisplayName: ownerName,
				Role:        persistence.MemberRoleOwner,
				CreatedAt:   now,
				UpdatedAt:   now,
			}, false)
		}

		return nil
	})
	if !ok {
		return
	}

	h.Logger.InfoContext(r.Context(), "web: organisation créée", "org_id", orgID)
	http.Redirect(w, r, "/admin/orgs/"+orgID, http.StatusFound)
}

// HandleOrg — ADM-03.

// HandleOrgDelete supprime une organisation et tout ce qui n'existe que
// par elle.
//
// La confirmation demande de retaper le nom : la liste d'administration
// peut présenter deux organisations homonymes, et se tromper de ligne
// n'aurait aucun recours — l'effacement est complet et définitif.
func (h *Handlers) HandleOrgDelete(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("id")
	typed := strings.TrimSpace(r.PostFormValue("confirm_name"))

	if h.Privacy == nil {
		h.redirectOrgError(w, r, orgID, "La suppression n'est pas disponible sur cette instance.")
		return
	}

	var (
		mismatch bool
		missing  bool
	)

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		org, found, err := h.Orgs.FindByID(r.Context(), tx, orgID)
		if err != nil {
			return err
		}
		if !found {
			missing = true
			return nil
		}
		if !strings.EqualFold(typed, strings.TrimSpace(org.DisplayName)) {
			mismatch = true
		}

		return nil
	})
	if !ok {
		return
	}

	if missing {
		http.NotFound(w, r)
		return
	}
	if mismatch {
		h.redirectOrgError(w, r, orgID, "Le nom saisi ne correspond pas : rien n'a été supprimé.")
		return
	}

	// La suppression touche la base mémoire autant que la base
	// applicative : elle passe par le service de confidentialité, qui
	// possède les deux.
	report, err := h.Privacy.DeleteOrganization(r.Context(), orgID)
	if err != nil {
		h.Logger.ErrorContext(r.Context(), "web: échec de la suppression d'une organisation", "org_id", orgID, "error", err)
		h.redirectOrgError(w, r, orgID, "La suppression a échoué. Rien n'a été supprimé, ou seulement une partie : consultez les journaux.")
		return
	}

	// Compteurs seulement : ce qui a été effacé ne se journalise pas.
	h.Logger.InfoContext(r.Context(), "web: organisation supprimée",
		"org_id", orgID,
		"members", report.Members,
		"orphan_members", report.OrphanMembers,
		"channels", report.Channels,
		"conversations", report.Conversations,
		"messages", report.Messages,
		"reminders", report.Reminders,
		"memories", report.Memories,
	)

	http.Redirect(w, r, "/admin/orgs?deleted="+url.QueryEscape(orgID), http.StatusFound)
}

// redirectOrgError renvoie sur la fiche avec un message d'échec.
