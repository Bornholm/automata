package registry

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/observability"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/usage"
)

// dbUsageRecorder implémente usage.Recorder sur la base applicative SQLite.
// L'insertion est synchrone : la base est locale, limitée à une connexion
// (voir persistence.Open), et le volume — un appel LLM par trace — reste
// négligeable devant la latence de l'appel lui-même. Un échec est journalisé
// et compté (usage_record_failures), jamais propagé : la comptabilité ne
// doit pas pouvoir faire échouer un tour.
type dbUsageRecorder struct {
	db      *persistence.DB
	repo    *persistence.UsageRecordRepository
	logger  *slog.Logger
	metrics *observability.Metrics
}

// newDBUsageRecorder construit le recorder adossé à db.
func newDBUsageRecorder(db *persistence.DB, logger *slog.Logger, metrics *observability.Metrics) *dbUsageRecorder {
	return &dbUsageRecorder{
		db:      db,
		repo:    persistence.NewUsageRecordRepository(),
		logger:  logger,
		metrics: metrics,
	}
}

// RecordUsage implémente usage.Recorder.
func (r *dbUsageRecorder) RecordUsage(ctx context.Context, rec usage.Record) {
	err := r.db.WithTx(ctx, func(tx *sql.Tx) error {
		return r.repo.Insert(ctx, tx, rec)
	})
	if err != nil {
		r.metrics.IncUsageRecordFailure()
		if r.logger != nil {
			// Seuls des identifiants et des volumes : aucun contenu privé.
			r.logger.ErrorContext(ctx, "registry: échec de l'enregistrement d'une trace d'usage",
				"error", err, "org_id", rec.OrgID, "component", rec.Component, "kind", rec.Kind, "model", rec.Model)
		}
		return
	}

	r.metrics.IncUsageRecord()
}

// UsageReport agrège les traces d'usage de la période [from, to) selon les
// dimensions groupBy et écrit un tableau lisible sur out. Utilisée par la
// commande CLI "automata usage report". La base est ouverte le temps du
// rapport puis refermée.
func UsageReport(ctx context.Context, cfg *config.Config, from, to time.Time, groupBy []string, out io.Writer) error {
	db, err := persistence.Open(ctx, cfg.Storage.Application)
	if err != nil {
		return fmt.Errorf("registry: ouverture de la persistance: %w", err)
	}
	defer db.Close()

	repo := persistence.NewUsageRecordRepository()

	var aggregates []persistence.UsageAggregate
	if err := db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		aggregates, err = repo.AggregateUsage(ctx, tx, from, to, groupBy)
		return err
	}); err != nil {
		return fmt.Errorf("registry: agrégation des traces d'usage: %w", err)
	}

	fmt.Fprintf(out, "Usage d'inférence du %s au %s (exclu), agrégé par %s\n\n",
		from.Format("2006-01-02"), to.Format("2006-01-02"), strings.Join(groupBy, ", "))

	if len(aggregates) == 0 {
		fmt.Fprintln(out, "Aucune trace d'usage sur la période.")
		return nil
	}

	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)

	headers := append(append([]string{}, groupBy...), "appels", "tokens entrée", "tokens sortie", "tokens cache", "coût", "devise", "appels sans coût")
	fmt.Fprintln(w, strings.Join(headers, "\t"))

	var totalCost float64
	var totalCalls, totalTokens int64
	currencies := map[string]struct{}{}

	for _, agg := range aggregates {
		cols := make([]string, 0, len(agg.Keys)+7)
		for _, key := range agg.Keys {
			if key == "" {
				key = "(non attribué)"
			}
			cols = append(cols, key)
		}
		currency := agg.Currency
		if currency == "" {
			currency = "-"
		}
		cols = append(cols,
			fmt.Sprintf("%d", agg.Calls),
			fmt.Sprintf("%d", agg.PromptTokens),
			fmt.Sprintf("%d", agg.CompletionTokens),
			fmt.Sprintf("%d", agg.CachedTokens),
			fmt.Sprintf("%.6f", agg.CostAmount),
			currency,
			fmt.Sprintf("%d", agg.Calls-agg.ReportedCalls),
		)
		fmt.Fprintln(w, strings.Join(cols, "\t"))

		totalCost += agg.CostAmount
		totalCalls += agg.Calls
		totalTokens += agg.TotalTokens
		if agg.Currency != "" {
			currencies[agg.Currency] = struct{}{}
		}
	}

	if err := w.Flush(); err != nil {
		return fmt.Errorf("registry: écriture du rapport d'usage: %w", err)
	}

	// Le total général n'est affiché que s'il a un sens : au plus une devise
	// sur la période.
	fmt.Fprintf(out, "\nTotal : %d appels, %d tokens", totalCalls, totalTokens)
	if len(currencies) <= 1 {
		currency := "-"
		for c := range currencies {
			currency = c
		}
		fmt.Fprintf(out, ", %.6f %s (coûts rapportés par les providers uniquement)", totalCost, currency)
	} else {
		fmt.Fprintf(out, " (devises multiples : totaux de coût par ligne uniquement)")
	}
	fmt.Fprintln(out)

	return nil
}
