package profile

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/bornholm/automata/internal/i18n"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/web/core"
	"github.com/bornholm/automata/internal/web/view"
)

// HandleProfileUsage — PRO-03. Les chiffres sont exprimés en crédits et en
// catégories parlantes ; le mot « token » n'apparaît nulle part, il ne dit
// rien à qui n'écrit pas de logiciel.
func (h *Handlers) HandleProfileUsage(w http.ResponseWriter, r *http.Request) {
	member, r, minutes, ok := h.resolveProfile(w, r)
	if !ok {
		return
	}

	now := h.Now()
	monthFrom, monthTo := core.MonthBounds(now)

	page := view.ProfileUsagePage{
		LinkID: r.PathValue("link"),
		Header: h.profileHeader(r, member, minutes),
		Shared: true,
	}

	plugins, ok := h.profilePluginUIs(w, r, member)
	if !ok {
		return
	}
	page.PluginUIs = plugins

	txOK := h.WithTx(w, r, func(tx *sql.Tx) error {
		rate := h.CreditRate(r.Context(), tx)

		// Répartition du mois par nature d'usage.
		aggregates, err := h.Usage.AggregateUsage(r.Context(), tx, monthFrom, monthTo,
			[]string{"agent", "kind"}, persistence.UsageFilter{OrgID: member.OrgID})
		if err != nil {
			return err
		}

		buckets := map[string]int64{}
		var monthTotal int64
		for _, agg := range aggregates {
			credits := h.UsageCredits(agg.CostAmount, rate)
			key := "usage.split.conversations"
			switch {
			case agg.Keys[1] == "image":
				key = "usage.split.images"
			case agg.Keys[1] == "transcription":
				key = "usage.split.voice"
			case agg.Keys[0] == "research":
				key = "usage.split.search"
			}
			buckets[key] += credits
			monthTotal += credits
		}

		shades := []string{"", "soft", "faint", "faint"}
		// Indexés par CLÉ, pas par libellé traduit : « Images » s'écrit
		// pareil en français et en anglais, et deux catégories distinctes
		// se retrouveraient dans le même seau.
		for i, key := range []string{"usage.split.conversations", "usage.split.search", "usage.split.images", "usage.split.voice"} {
			credits := buckets[key]
			if credits == 0 {
				continue
			}
			page.Split = append(page.Split, view.UsageSplit{
				Label:   i18n.TC(r.Context(), key),
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
			credits, err := h.SingleOrgUsageCredits(r.Context(), tx, member.OrgID, from, to)
			if err != nil {
				return err
			}
			if credits > maxMonth {
				maxMonth = credits
			}
			months = append(months, monthUsage{view.FormatMonth(r.Context(), from), credits, i == 0})
		}
		for _, month := range months {
			page.Months = append(page.Months, view.UsageMonth{
				Label:   month.label,
				Credits: month.credits,
				Pct:     view.GaugePercent(month.credits, maxMonth),
				Current: month.current,
			})
		}

		page.Summary = usageSummary(r.Context(), monthTotal, months[len(months)-2].credits, now)

		return nil
	})
	if !txOK {
		return
	}

	h.Render(w, r, http.StatusOK, view.ProfileUsage(page))
}

// usageSummary énonce l'usage du mois avant tout chiffre, et le compare au
// mois précédent quand la comparaison a un sens.
func usageSummary(ctx context.Context, current, previous int64, now time.Time) string {
	if current == 0 {
		return i18n.TC(ctx, "usage.summary.none")
	}

	// Comparer à un mois précédent vide, ou en tout début de mois, ne veut
	// rien dire : mieux vaut se taire que d'annoncer « +900 % ».
	if previous == 0 || now.Day() < 5 {
		return i18n.TC(ctx, "usage.summary.plain")
	}

	switch ratio := float64(current) / float64(previous); {
	case ratio > 1.5:
		return i18n.TC(ctx, "usage.summary.more")
	case ratio < 0.6:
		return i18n.TC(ctx, "usage.summary.less")
	default:
		return i18n.TC(ctx, "usage.summary.same")
	}
}
