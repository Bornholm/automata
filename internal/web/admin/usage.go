package admin

import (
	"database/sql"
	"encoding/csv"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/web/core"
	"github.com/bornholm/automata/internal/web/view"
)

// usageDimensions décrit les axes d'agrégation proposés, dans l'ordre
// d'affichage. Les clés sont celles de persistence.UsageGroupKeys.
var usageDimensions = []struct{ Value, Label string }{
	{"org", "Organisation"},
	{"principal", "Membre"},
	{"agent", "Agent"},
	{"model", "Modèle"},
	{"kind", "Nature"},
	{"component", "Composant"},
	{"provider", "Fournisseur"},
	{"day", "Jour"},
	{"month", "Mois"},
}

// usageQuery lit les filtres de la requête : période et dimensions.
func usageQuery(r *http.Request, now time.Time) (from, to time.Time, dimensions []string) {
	from, to = core.MonthBounds(now)

	if raw := r.URL.Query().Get("from"); raw != "" {
		if parsed, err := time.Parse("2006-01-02", raw); err == nil {
			from = parsed
		}
	}
	if raw := r.URL.Query().Get("to"); raw != "" {
		if parsed, err := time.Parse("2006-01-02", raw); err == nil {
			to = parsed
		}
	}

	valid := map[string]bool{}
	for _, dimension := range usageDimensions {
		valid[dimension.Value] = true
	}
	for _, requested := range r.URL.Query()["by"] {
		if valid[requested] {
			dimensions = append(dimensions, requested)
		}
	}
	if len(dimensions) == 0 {
		dimensions = []string{"org"}
	}

	return from, to, dimensions
}

// usageRows agrège la consommation et convertit les coûts en crédits.
func (h *Handlers) usageRows(r *http.Request, tx *sql.Tx, from, to time.Time, dimensions []string) ([]view.UsageRow, view.UsagePage, error) {
	aggregates, err := h.Usage.AggregateUsage(r.Context(), tx, from, to, dimensions, persistence.UsageFilter{})
	if err != nil {
		return nil, view.UsagePage{}, err
	}

	rate := h.CreditRate(r.Context(), tx)

	var (
		rows    []view.UsageRow
		totals  view.UsagePage
		costUSD float64
	)

	for _, agg := range aggregates {
		row := view.UsageRow{
			Keys:       agg.Keys,
			Calls:      agg.Calls,
			Tokens:     agg.TotalTokens,
			Credits:    h.UsageCredits(agg.CostAmount, rate),
			CostUSD:    fmt.Sprintf("%.4f $", agg.CostAmount),
			Unreported: agg.Calls - agg.ReportedCalls,
		}
		rows = append(rows, row)

		totals.TotalCalls += agg.Calls
		totals.TotalTokens += agg.TotalTokens
		totals.TotalCredits += row.Credits
		totals.TotalUnreported += row.Unreported
		costUSD += agg.CostAmount
	}
	totals.TotalCostUSD = fmt.Sprintf("%.2f $", costUSD)

	return rows, totals, nil
}

// HandleUsage — ADM-06.
func (h *Handlers) HandleUsage(w http.ResponseWriter, r *http.Request) {
	from, to, dimensions := usageQuery(r, h.Now())

	page := view.UsagePage{
		Platforms: h.SidebarPlatforms(),
		CSRFToken: h.CSRFToken(w, r),
		From:      from.Format("2006-01-02"),
		To:        to.Format("2006-01-02"),
	}

	selected := map[string]bool{}
	for _, dimension := range dimensions {
		selected[dimension] = true
	}
	for _, dimension := range usageDimensions {
		page.Dimensions = append(page.Dimensions, view.UsageOption{
			Value:    dimension.Value,
			Label:    dimension.Label,
			Selected: selected[dimension.Value],
		})
		if selected[dimension.Value] {
			page.Headers = append(page.Headers, dimension.Label)
		}
	}

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		rows, totals, err := h.usageRows(r, tx, from, to, dimensions)
		if err != nil {
			return err
		}

		page.Rows = rows
		page.TotalCalls = totals.TotalCalls
		page.TotalTokens = totals.TotalTokens
		page.TotalCredits = totals.TotalCredits
		page.TotalCostUSD = totals.TotalCostUSD
		page.TotalUnreported = totals.TotalUnreported

		// Les organisations sont affichées par leur nom : un identifiant
		// technique n'aide personne à refacturer.
		if len(dimensions) > 0 && dimensions[0] == "org" {
			for i := range page.Rows {
				if id := page.Rows[i].Keys[0]; id != "" {
					page.Rows[i].Keys[0] = h.OrgDisplayName(r.Context(), tx, id)
				}
			}
		}

		return nil
	})
	if !ok {
		return
	}

	query := url.Values{"from": {page.From}, "to": {page.To}}
	for _, dimension := range dimensions {
		query.Add("by", dimension)
	}
	page.ExportURL = "/admin/usage.csv?" + query.Encode()

	h.Render(w, r, http.StatusOK, view.AdminUsage(page))
}

// HandleUsageCSV exporte l'agrégation courante, filtres compris.
func (h *Handlers) HandleUsageCSV(w http.ResponseWriter, r *http.Request) {
	from, to, dimensions := usageQuery(r, h.Now())

	var rows []view.UsageRow
	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		var err error
		rows, _, err = h.usageRows(r, tx, from, to, dimensions)
		return err
	})
	if !ok {
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="consommation-%s-%s.csv"`,
		from.Format("2006-01-02"), to.Format("2006-01-02")))

	writer := csv.NewWriter(w)
	header := append(append([]string{}, dimensions...), "appels", "tokens", "credits", "cout_usd", "appels_sans_cout")
	_ = writer.Write(header)

	for _, row := range rows {
		record := append([]string{}, row.Keys...)
		record = append(record,
			strconv.FormatInt(row.Calls, 10),
			strconv.FormatInt(row.Tokens, 10),
			strconv.FormatInt(row.Credits, 10),
			strings.TrimSuffix(row.CostUSD, " $"),
			strconv.FormatInt(row.Unreported, 10),
		)
		_ = writer.Write(record)
	}
	writer.Flush()
}
