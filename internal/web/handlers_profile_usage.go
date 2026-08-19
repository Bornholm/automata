package web

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/web/view"
)

// handleProfileUsage — PRO-03. Les chiffres sont exprimés en crédits et en
// catégories parlantes ; le mot « token » n'apparaît nulle part, il ne dit
// rien à qui n'écrit pas de logiciel.
func (s *Server) handleProfileUsage(w http.ResponseWriter, r *http.Request) {
	member, minutes, ok := s.resolveProfile(w, r)
	if !ok {
		return
	}

	now := s.now()
	monthFrom, monthTo := monthBounds(now)

	page := view.ProfileUsagePage{
		LinkID: r.PathValue("link"),
		Header: s.profileHeader(r, member, minutes),
		Shared: true,
	}

	txOK := s.withTx(w, r, func(tx *sql.Tx) error {
		rate := s.creditRate(r.Context(), tx)

		// Répartition du mois par nature d'usage.
		aggregates, err := s.usage.AggregateUsage(r.Context(), tx, monthFrom, monthTo,
			[]string{"agent", "kind"}, persistence.UsageFilter{OrgID: member.OrgID})
		if err != nil {
			return err
		}

		buckets := map[string]int64{}
		var monthTotal int64
		for _, agg := range aggregates {
			credits := s.usageCredits(agg.CostAmount, rate)
			label := "Conversations"
			switch {
			case agg.Keys[1] == "image":
				label = "Images"
			case agg.Keys[1] == "transcription":
				label = "Notes vocales"
			case agg.Keys[0] == "research":
				label = "Recherches"
			}
			buckets[label] += credits
			monthTotal += credits
		}

		shades := []string{"", "soft", "faint", "faint"}
		for i, label := range []string{"Conversations", "Recherches", "Images", "Notes vocales"} {
			credits := buckets[label]
			if credits == 0 {
				continue
			}
			page.Split = append(page.Split, view.UsageSplit{
				Label:   label,
				Credits: credits,
				Pct:     view.GaugePercent(credits, monthTotal),
				Shade:   shades[i],
			})
		}

		// Six derniers mois.
		var maxMonth int64
		type monthUsage struct {
			label   string
			credits int64
			current bool
		}
		var months []monthUsage
		for i := 5; i >= 0; i-- {
			from := monthFrom.AddDate(0, -i, 0)
			to := from.AddDate(0, 1, 0)
			credits, err := s.singleOrgUsageCredits(r.Context(), tx, member.OrgID, from, to)
			if err != nil {
				return err
			}
			if credits > maxMonth {
				maxMonth = credits
			}
			months = append(months, monthUsage{view.FormatMonth(from), credits, i == 0})
		}
		for _, month := range months {
			page.Months = append(page.Months, view.UsageMonth{
				Label:   month.label,
				Credits: month.credits,
				Pct:     view.GaugePercent(month.credits, maxMonth),
				Current: month.current,
			})
		}

		page.Summary = usageSummary(monthTotal, months[len(months)-2].credits, now)

		return nil
	})
	if !txOK {
		return
	}

	s.render(w, r, http.StatusOK, view.ProfileUsage(page))
}

// usageSummary énonce l'usage du mois avant tout chiffre, et le compare au
// mois précédent quand la comparaison a un sens.
func usageSummary(current, previous int64, now time.Time) string {
	if current == 0 {
		return "Vous n'avez rien consommé ce mois-ci."
	}

	// Comparer à un mois précédent vide, ou en tout début de mois, ne veut
	// rien dire : mieux vaut se taire que d'annoncer « +900 % ».
	if previous == 0 || now.Day() < 5 {
		return "Voici ce que vos échanges avec Automata ont consommé ce mois-ci."
	}

	switch ratio := float64(current) / float64(previous); {
	case ratio > 1.5:
		return "Vous utilisez Automata nettement plus que le mois dernier."
	case ratio < 0.6:
		return "Vous utilisez Automata moins que le mois dernier."
	default:
		return "Votre usage ressemble à celui du mois dernier."
	}
}
