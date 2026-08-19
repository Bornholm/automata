package registry

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
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
	prices  *persistence.ModelPriceRepository
	pricing *persistence.PricingRepository
	logger  *slog.Logger
	metrics *observability.Metrics

	// table est la grille tarifaire, rafraîchie périodiquement : elle sert
	// à estimer le coût des appels dont le fournisseur ne rapporte rien.
	mu          sync.RWMutex
	table       persistence.PriceTable
	tableLoaded time.Time
}

// priceTableTTL borne la fraîcheur de la grille : un tarif corrigé dans
// l'administration s'applique au plus tard une minute après.
const priceTableTTL = time.Minute

// newDBUsageRecorder construit le recorder adossé à db.
func newDBUsageRecorder(db *persistence.DB, logger *slog.Logger, metrics *observability.Metrics) *dbUsageRecorder {
	return &dbUsageRecorder{
		db:      db,
		repo:    persistence.NewUsageRecordRepository(),
		prices:  persistence.NewModelPriceRepository(),
		pricing: persistence.NewPricingRepository(),
		logger:  logger,
		metrics: metrics,
	}
}

// RecordUsage implémente usage.Recorder.
//
// Un appel dont le fournisseur ne rapporte aucun coût est estimé depuis
// ses volumes de tokens (grille tarifaire) avant d'être enregistré : sans
// ce repli, il serait facturé zéro crédit et la consommation fuirait. Le
// drapeau CostReported reste faux, de sorte que l'estimation ne se fasse
// jamais passer pour une mesure.
func (r *dbUsageRecorder) RecordUsage(ctx context.Context, rec usage.Record) {
	if !rec.CostReported && rec.CostAmount == 0 && (rec.PromptTokens > 0 || rec.CompletionTokens > 0) {
		table, err := r.priceTable(ctx)
		if err != nil {
			r.logger.ErrorContext(ctx, "registry: grille tarifaire indisponible, coût non estimé",
				"error", err, "model", rec.Model)
		} else {
			rec.CostAmount = table.EstimateUSD(rec.Model, rec.PromptTokens, rec.CompletionTokens)
			rec.CostCurrency = "USD"
		}
	}

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

// priceTable retourne la grille tarifaire, rechargée au plus une fois par
// minute : l'estimation est sur le chemin de chaque appel LLM, elle ne
// doit pas relire la base à chaque fois.
func (r *dbUsageRecorder) priceTable(ctx context.Context) (persistence.PriceTable, error) {
	r.mu.RLock()
	table, loaded := r.table, r.tableLoaded
	r.mu.RUnlock()

	if time.Since(loaded) < priceTableTTL {
		return table, nil
	}

	var (
		prices                      []persistence.ModelPrice
		defaultInput, defaultOutput float64
	)
	err := r.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		prices, err = r.prices.List(ctx, tx)
		if err != nil {
			return err
		}

		for key, target := range map[string]*float64{
			persistence.SettingDefaultInputPrice:  &defaultInput,
			persistence.SettingDefaultOutputPrice: &defaultOutput,
		} {
			value, found, err := r.pricing.GetSetting(ctx, tx, key)
			if err != nil {
				return err
			}
			if found {
				if parsed, err := strconv.ParseFloat(value, 64); err == nil && parsed > 0 {
					*target = parsed
				}
			}
		}

		return nil
	})
	if err != nil {
		return table, err
	}

	table = persistence.NewPriceTable(prices, defaultInput, defaultOutput)

	r.mu.Lock()
	r.table, r.tableLoaded = table, time.Now()
	r.mu.Unlock()

	return table, nil
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
		aggregates, err = repo.AggregateUsage(ctx, tx, from, to, groupBy, persistence.UsageFilter{})
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

// UsageReprice estime rétroactivement le coût des traces enregistrées sans
// coût exploitable — celles qui échapperaient à la facturation. Utilisée
// par la commande CLI « automata usage reprice », après avoir renseigné la
// grille tarifaire.
func UsageReprice(ctx context.Context, cfg *config.Config, out io.Writer) error {
	db, err := persistence.Open(ctx, cfg.Storage.Application)
	if err != nil {
		return fmt.Errorf("registry: ouverture de la persistance: %w", err)
	}
	defer db.Close()

	records := persistence.NewUsageRecordRepository()
	prices := persistence.NewModelPriceRepository()
	pricing := persistence.NewPricingRepository()

	var repriced int
	var total float64

	err = db.WithTx(ctx, func(tx *sql.Tx) error {
		grid, err := prices.List(ctx, tx)
		if err != nil {
			return err
		}

		var defaultInput, defaultOutput float64
		for key, target := range map[string]*float64{
			persistence.SettingDefaultInputPrice:  &defaultInput,
			persistence.SettingDefaultOutputPrice: &defaultOutput,
		} {
			value, found, err := pricing.GetSetting(ctx, tx, key)
			if err != nil {
				return err
			}
			if found {
				if parsed, err := strconv.ParseFloat(value, 64); err == nil && parsed > 0 {
					*target = parsed
				}
			}
		}

		table := persistence.NewPriceTable(grid, defaultInput, defaultOutput)

		unpriced, err := records.ListUnpriced(ctx, tx)
		if err != nil {
			return err
		}

		for _, record := range unpriced {
			cost := table.EstimateUSD(record.Model, record.PromptTokens, record.CompletionTokens)
			if cost <= 0 {
				continue
			}
			if err := records.SetEstimatedCost(ctx, tx, record.ID, cost); err != nil {
				return err
			}
			repriced++
			total += cost
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("registry: estimation rétroactive des coûts: %w", err)
	}

	fmt.Fprintf(out, "%d trace(s) estimée(s), %.4f $ de coût rendu visible\n", repriced, total)
	if repriced > 0 {
		fmt.Fprintln(out, "Ces coûts seront décomptés au prochain passage du débiteur s'ils tombent dans une période non encore facturée.")
	}

	return nil
}
