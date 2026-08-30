package admin

import (
	"database/sql"
	"net/http"
	"sort"

	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/web/core"
	"github.com/bornholm/automata/internal/web/view"
)

// Écran des alertes d'exploitation : qui est prévenu, et ce qui s'est passé.
//
// Il vit à part des paramètres d'instance, qui sont en lecture seule et
// l'annoncent en tête de page. Le destinataire des alertes, lui, se règle
// en ligne — c'est un réglage d'exploitation, pas une propriété de la
// machine.

// maxListedAlerts borne l'historique affiché : au-delà, l'écran ne sert plus
// à voir ce qui ne va pas, il sert à faire défiler.
const maxListedAlerts = 50

// HandleAlerts rend l'écran.
func (h *Handlers) HandleAlerts(w http.ResponseWriter, r *http.Request) {
	page := view.AlertsPage{
		Platforms: h.SidebarPlatforms(),
		CSRFToken: h.CSRFToken(w, r),
		Saved:     r.URL.Query().Get("saved") == "1",
	}

	settings := persistence.NewInstanceSettingRepository()
	alertsRepo := persistence.NewAlertRepository()

	var (
		alerts  []persistence.Alert
		members []persistence.Member
		orgs    map[string]string
	)
	if !h.WithTx(w, r, func(tx *sql.Tx) error {
		var err error
		if page.OperatorID, err = settings.Get(r.Context(), tx, persistence.SettingOperatorMemberID); err != nil {
			return err
		}
		if alerts, err = alertsRepo.ListRecent(r.Context(), tx, maxListedAlerts); err != nil {
			return err
		}

		organizations, err := h.Orgs.List(r.Context(), tx, "")
		if err != nil {
			return err
		}
		orgs = make(map[string]string, len(organizations))
		for _, org := range organizations {
			orgs[org.ID] = org.DisplayName
			orgMembers, err := h.Members.ListByOrg(r.Context(), tx, org.ID)
			if err != nil {
				return err
			}
			members = append(members, orgMembers...)
		}
		return nil
	}) {
		return
	}

	// Seul un membre rattaché à une conversation peut être prévenu : en
	// proposer d'autres promettrait des alertes qui n'arriveraient jamais.
	for _, member := range members {
		if !member.Linked() {
			continue
		}
		page.Operators = append(page.Operators, view.AlertOperator{
			ID:     member.ID,
			Name:   member.DisplayName,
			Detail: orgs[member.OrgID] + " · " + core.PlatformDisplayName(member.Provider, member.Provider),
		})
	}
	sort.Slice(page.Operators, func(i, j int) bool {
		return page.Operators[i].Name < page.Operators[j].Name
	})

	for _, alert := range alerts {
		page.Alerts = append(page.Alerts, view.AlertRow{
			Kind:      alert.Kind,
			Subject:   alert.Subject,
			Message:   alert.Message,
			CreatedAt: alert.CreatedAt.Local().Format("02/01/2006 15:04"),
			Delivered: alert.Delivered(),
		})
	}

	h.Render(w, r, http.StatusOK, view.AdminAlerts(page))
}

// HandleAlertsOperator enregistre le destinataire des alertes. Une valeur
// vide est légitime : elle éteint la remise sans rien casser — les alertes
// continuent d'être enregistrées et journalisées.
func (h *Handlers) HandleAlertsOperator(w http.ResponseWriter, r *http.Request) {
	if !core.CheckCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}

	memberID := r.FormValue("member_id")

	if memberID != "" {
		var linked bool
		if !h.WithTx(w, r, func(tx *sql.Tx) error {
			member, found, err := h.Members.FindByID(r.Context(), tx, memberID)
			if err != nil {
				return err
			}
			linked = found && member.Linked()
			return nil
		}) {
			return
		}
		if !linked {
			// Un membre sans conversation ne recevrait jamais rien : mieux
			// vaut refuser que laisser croire à une alerte qui partira.
			http.Redirect(w, r, "/admin/alerts", http.StatusSeeOther)
			return
		}
	}

	if !h.WithTx(w, r, func(tx *sql.Tx) error {
		return persistence.NewInstanceSettingRepository().
			Set(r.Context(), tx, persistence.SettingOperatorMemberID, memberID, h.Now())
	}) {
		return
	}

	http.Redirect(w, r, "/admin/alerts?saved=1", http.StatusSeeOther)
}
