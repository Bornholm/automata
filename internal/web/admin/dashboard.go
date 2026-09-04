package admin

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/web/core"
	"github.com/bornholm/automata/internal/web/view"
)

// deliveryLookback borne la fenêtre des échecs de livraison remontés : un
// incident d'il y a trois semaines n'appelle plus d'action.
const deliveryLookback = 7 * 24 * time.Hour

// HandleDashboard — ADM-01.
func (h *Handlers) HandleDashboard(w http.ResponseWriter, r *http.Request) {
	now := h.Now()
	monthFrom, monthTo := core.MonthBounds(now)

	page := view.DashboardPage{
		Platforms: h.SidebarPlatforms(),
		CSRFToken: h.CSRFToken(w, r),
		Period:    strings.ToLower(view.FormatMonth(r.Context(), now)) + " " + fmt.Sprintf("%d", now.Year()),
	}

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		orgs, err := h.Orgs.List(r.Context(), tx, "")
		if err != nil {
			return err
		}
		balances, err := h.Wallet.Balances(r.Context(), tx)
		if err != nil {
			return err
		}
		monthUsage, err := h.OrgUsageCredits(r.Context(), tx, monthFrom, monthTo)
		if err != nil {
			return err
		}

		// Repères : ce qu'on veut savoir sans cliquer.
		connected, total := h.platformCounts(r.Context(), tx)
		platformTone := ""
		if connected < total {
			platformTone = "warn"
		}

		var totalUsage int64
		for _, credits := range monthUsage {
			totalUsage += credits
		}

		pricingSettings, err := h.Pricing(r.Context(), tx)
		if err != nil {
			return err
		}
		m, err := h.ComputeMargin(r.Context(), tx, pricingSettings, monthFrom, monthTo)
		if err != nil {
			return err
		}

		marginTone := "ok"
		if m.MarginEUR < 0 {
			marginTone = "crit"
		}

		page.Figures = []view.DashboardFigure{
			{Label: "Organisations", Value: fmt.Sprintf("%d", len(orgs)), Hint: h.countActiveMembers(r.Context(), tx)},
			{Label: "Comptes de messagerie", Value: fmt.Sprintf("%d/%d", connected, total), Hint: "connectés", Tone: platformTone},
			{Label: "Consommation", Value: view.FormatCredits(totalUsage), Hint: fmt.Sprintf("%.2f $ mesurés", m.CostUSD)},
			{Label: "Marge estimée", Value: core.FormatEuros(m.MarginEUR), Hint: m.Ratio(), Tone: marginTone},
		}

		// Organisations à surveiller : solde faible ou épuisé.
		for _, org := range orgs {
			lastCredit, err := h.Wallet.LastCredit(r.Context(), tx, org.ID)
			if err != nil {
				return err
			}
			state := core.ComputeWalletState(org, balances[org.ID], lastCredit, monthUsage[org.ID])

			switch state.State {
			case "empty":
				page.Alerts = append(page.Alerts, view.DashboardAlert{
					Kind:   "wallet",
					Title:  org.DisplayName + " est à court de crédits",
					Detail: "Le service est en pause pour cette organisation : elle reçoit un message d'explication et un lien de recharge.",
					Tone:   "crit",
					Href:   "/admin/orgs/" + org.ID,
				})
			case "low":
				page.Alerts = append(page.Alerts, view.DashboardAlert{
					Kind:   "wallet",
					Title:  org.DisplayName + " approche de la fin de ses crédits",
					Detail: fmt.Sprintf("Il lui reste %s.", view.FormatCredits(state.Balance)),
					Tone:   "warn",
					Href:   "/admin/orgs/" + org.ID,
				})
			}
		}

		// Comptes de messagerie en défaut.
		if h.PlatformMgr != nil {
			accounts, err := h.Platforms.List(r.Context(), tx)
			if err != nil {
				return err
			}
			statuses := h.PlatformMgr.Statuses()

			for _, account := range accounts {
				if !account.Enabled {
					continue
				}
				status := statuses[account.ID]
				switch status.State {
				case "failed":
					page.Alerts = append(page.Alerts, view.DashboardAlert{
						Kind:   "platform",
						Title:  platformTypeLabel(account.Type) + " ne répond plus",
						Detail: firstNonEmpty(status.Err, "Le pipeline s'est arrêté sur une erreur."),
						Tone:   "crit",
						Href:   "/admin/platforms",
					})
				case "pairing":
					page.Alerts = append(page.Alerts, view.DashboardAlert{
						Kind:   "platform",
						Title:  platformTypeLabel(account.Type) + " attend son appairage",
						Detail: "Un QR code est affiché sur l'écran des plateformes ; il se renouvelle tout seul.",
						Tone:   "warn",
						Href:   "/admin/platforms",
					})
				}
			}
		}

		// Échecs de livraison récents : un rappel qui n'est pas parti est
		// invisible du client comme de l'exploitant sans cette remontée.
		if failures, err := h.recentDeliveryFailures(r.Context(), tx, now.Add(-deliveryLookback)); err != nil {
			return err
		} else if failures > 0 {
			page.Alerts = append(page.Alerts, view.DashboardAlert{
				Kind:   "delivery",
				Title:  fmt.Sprintf("%d livraison(s) en échec cette semaine", failures),
				Detail: "Des messages planifiés ou des rappels n'ont pas pu être remis à leur destinataire.",
				Tone:   "warn",
			})
		}

		// Consommation non facturable : elle fausse la refacturation.
		if unattributed := monthUsage[""]; unattributed > 0 {
			page.Alerts = append(page.Alerts, view.DashboardAlert{
				Kind:   "usage",
				Title:  view.FormatCredits(unattributed) + " consommés sans organisation",
				Detail: "Cette consommation ne sera refacturée à personne : elle vient d'appels dont l'attribution a échoué.",
				Tone:   "warn",
				Href:   "/admin/usage?by=agent&by=model",
			})
		}

		// Classement de consommation.
		var maxCredits int64
		type orgUsage struct {
			id, name string
			credits  int64
		}
		var ranked []orgUsage
		for _, org := range orgs {
			credits := monthUsage[org.ID]
			if credits <= 0 {
				continue
			}
			if credits > maxCredits {
				maxCredits = credits
			}
			ranked = append(ranked, orgUsage{org.ID, org.DisplayName, credits})
		}
		sort.Slice(ranked, func(i, j int) bool { return ranked[i].credits > ranked[j].credits })

		for i, row := range ranked {
			if i >= 6 {
				break
			}
			page.TopOrgs = append(page.TopOrgs, view.DashboardOrgUsage{
				ID:      row.id,
				Name:    row.name,
				Credits: row.credits,
				Pct:     view.GaugePercent(row.credits, maxCredits),
			})
		}

		return nil
	})
	if !ok {
		return
	}

	h.Render(w, r, http.StatusOK, view.AdminDashboard(page))
}

// platformCounts compte les comptes actifs et ceux qui répondent.
func (h *Handlers) platformCounts(ctx context.Context, q persistence.Querier) (connected, total int) {
	accounts, err := h.Platforms.List(ctx, q)
	if err != nil {
		return 0, 0
	}

	var statuses map[string]platformStatus
	if h.PlatformMgr != nil {
		statuses = map[string]platformStatus{}
		for id, status := range h.PlatformMgr.Statuses() {
			statuses[id] = platformStatus{State: string(status.State)}
		}
	}

	for _, account := range accounts {
		if !account.Enabled {
			continue
		}
		total++
		if statuses[account.ID].State == "running" {
			connected++
		}
	}

	return connected, total
}

// platformStatus réduit l'état d'un compte à ce dont le tableau de bord a
// besoin.
type platformStatus struct{ State string }

// recentDeliveryFailures compte les échecs de livraison depuis since.
func (h *Handlers) recentDeliveryFailures(ctx context.Context, q persistence.Querier, since time.Time) (int, error) {
	row := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM delivery_attempts
		WHERE status = 'failed' AND created_at >= ?`, since.UTC().Format(time.RFC3339))

	var failures int
	if err := row.Scan(&failures); err != nil {
		return 0, fmt.Errorf("comptage des échecs de livraison: %w", err)
	}

	return failures, nil
}

// countActiveMembers décrit le nombre de membres rattachés.
func (h *Handlers) countActiveMembers(ctx context.Context, q persistence.Querier) string {
	counts, err := h.Members.CountByOrg(ctx, q)
	if err != nil {
		return ""
	}

	var total int64
	for _, count := range counts {
		total += count
	}

	if total <= 1 {
		return fmt.Sprintf("%d membre", total)
	}
	return fmt.Sprintf("%d membres", total)
}

// firstNonEmpty retourne la première chaîne non vide.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
