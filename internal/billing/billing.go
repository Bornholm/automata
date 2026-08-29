// Package billing tient les comptes du portefeuille de crédits de chaque
// organisation : conversion de la consommation mesurée
// (internal/usage, table usage_records) en débits, et remise à niveau
// mensuelle des organisations offertes par la maison.
//
// Le portefeuille est un livre de comptes immuable (wallet_entries) : on
// n'y corrige jamais une ligne, on en ajoute une. Le solde est toujours la
// somme des mouvements — jamais une valeur stockée qui pourrait diverger.
package billing

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/observability"
	"github.com/bornholm/automata/internal/persistence"
)

// Noms des tâches de maintenance (table maintenance_runs) : le débiteur
// avance par période fermée, jamais deux fois la même.
const (
	debitTaskName     = "wallet-debit"
	allowanceTaskName = "wallet-allowance"
)

// tickInterval est la cadence de vérification. Le débit n'a pas besoin
// d'être temps réel : une organisation dont le solde vient de passer à zéro
// est arrêtée au tour suivant, pas au milieu d'une phrase.
const tickInterval = 10 * time.Minute

// debitPeriod borne chaque passage : les traces plus récentes que cette
// marge sont laissées au passage suivant, le temps que les enregistrements
// concurrents d'un tour en cours soient tous écrits.
const debitPeriod = time.Minute

// Debiter convertit la consommation en débits de crédits et applique les
// allocations mensuelles.
type Debiter struct {
	db      *persistence.DB
	cfg     *config.Config
	logger  *slog.Logger
	metrics *observability.Metrics
	now     func() time.Time
	// rate est le taux de conversion lu en base au début de chaque
	// passage : l'écran de tarification doit pouvoir l'ajuster sans
	// redémarrage.
	rate float64
	// notifier prévient les organisations dont les crédits s'épuisent.
	notifier Notifier

	runs    *persistence.MaintenanceRunRepository
	pricing *persistence.PricingRepository
	usage   *persistence.UsageRecordRepository
	wallet  *persistence.WalletRepository
	orgs    *persistence.OrganizationRepository
}

// New construit le débiteur.
func New(db *persistence.DB, cfg *config.Config, logger *slog.Logger, metrics *observability.Metrics) *Debiter {
	if logger == nil {
		logger = slog.Default()
	}

	return &Debiter{
		db:      db,
		cfg:     cfg,
		logger:  logger,
		metrics: metrics,
		now:     time.Now,
		runs:    persistence.NewMaintenanceRunRepository(),
		pricing: persistence.NewPricingRepository(),
		usage:   persistence.NewUsageRecordRepository(),
		wallet:  persistence.NewWalletRepository(),
		orgs:    persistence.NewOrganizationRepository(),
	}
}

// WithClock remplace l'horloge (tests).
func (d *Debiter) WithClock(now func() time.Time) *Debiter {
	d.now = now
	return d
}

// Run applique débits et allocations toutes les tickInterval, jusqu'à
// l'annulation de ctx. Une erreur est journalisée, jamais fatale : le
// passage suivant reprendra où celui-ci s'est arrêté.
func (d *Debiter) Run(ctx context.Context) error {
	if err := d.Tick(ctx); err != nil && ctx.Err() == nil {
		d.logger.ErrorContext(ctx, "billing: échec du passage initial", "error", err)
	}

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := d.Tick(ctx); err != nil && ctx.Err() == nil {
				d.logger.ErrorContext(ctx, "billing: échec d'un passage", "error", err)
			}
		}
	}
}

// Tick enchaîne un débit, les allocations du mois, puis les alertes de
// solde bas — dans cet ordre : prévenir sur un solde qui n'a pas encore
// été débité donnerait un chiffre faux.
func (d *Debiter) Tick(ctx context.Context) error {
	if err := d.Debit(ctx); err != nil {
		return err
	}

	if err := d.ApplyAllowances(ctx); err != nil {
		return err
	}

	return d.notifyLowBalances(ctx)
}

// Debit convertit en crédits la consommation enregistrée depuis le dernier
// passage, et l'inscrit au débit de chaque organisation concernée.
//
// L'idempotence tient à la borne haute enregistrée dans maintenance_runs :
// une période fermée n'est jamais relue. Au tout premier passage, la borne
// est simplement posée à maintenant, sans débiter — sinon l'activation de
// la facturation débiterait rétroactivement toute l'histoire de l'instance.
func (d *Debiter) Debit(ctx context.Context) error {
	now := d.now()
	to := now.Add(-debitPeriod)

	var (
		from  time.Time
		found bool
	)
	err := d.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		from, found, err = d.runs.GetLastRun(ctx, tx, debitTaskName)
		return err
	})
	if err != nil {
		return fmt.Errorf("billing: lecture du dernier débit: %w", err)
	}

	if !found {
		return d.recordRun(ctx, debitTaskName, to)
	}
	if !from.Before(to) {
		return nil
	}

	var debited int

	err = d.db.WithTx(ctx, func(tx *sql.Tx) error {
		// Taux de conversion courant : réglable depuis l'administration
		// (écran de tarification), sinon celui de la configuration.
		if value, found, err := d.pricing.GetSetting(ctx, tx, persistence.SettingUSDPerCredit); err != nil {
			return err
		} else if found {
			if parsed, err := strconv.ParseFloat(value, 64); err == nil && parsed > 0 {
				d.rate = parsed
			}
		}

		// Groupé par organisation ET par nature d'appel : le libellé du
		// mouvement doit rester compréhensible dans le portefeuille du
		// client (« Usage — génération d'images »), sans jargon.
		aggregates, err := d.usage.AggregateUsage(ctx, tx, from, to, []string{"org", "kind", "agent"}, persistence.UsageFilter{})
		if err != nil {
			return err
		}

		totals := map[string]map[string]int64{}
		for _, agg := range aggregates {
			orgID, kind, agent := agg.Keys[0], agg.Keys[1], agg.Keys[2]
			if orgID == "" {
				// Consommation non attribuable : elle reste visible dans les
				// rapports d'usage, mais n'est facturée à personne.
				continue
			}

			credits := d.credits(agg.CostAmount)
			if credits <= 0 {
				continue
			}

			label := usageLabel(kind, agent)
			if totals[orgID] == nil {
				totals[orgID] = map[string]int64{}
			}
			totals[orgID][label] += credits
		}

		for orgID, byLabel := range totals {
			// Seules les organisations enregistrées ont un portefeuille :
			// une organisation encore purement configurée n'est pas facturée.
			org, exists, err := d.orgs.FindByID(ctx, tx, orgID)
			if err != nil {
				return err
			}
			if !exists {
				continue
			}

			// Mode gratuit sans limite : rien n'est inscrit au portefeuille.
			// La consommation reste dans usage_records, donc le coût réel de
			// l'organisation demeure visible dans les écrans d'usage et de
			// marge — c'est ce qui distingue « offert » de « non mesuré ».
			if org.Unlimited {
				continue
			}

			for label, credits := range byLabel {
				if err := d.wallet.Insert(ctx, tx, persistence.WalletEntry{
					OrgID:     orgID,
					Kind:      persistence.WalletKindUsage,
					Label:     label,
					Amount:    -credits,
					CreatedAt: now,
				}); err != nil {
					return err
				}
				debited++
			}
		}

		return d.runs.SetLastRun(ctx, tx, debitTaskName, to)
	})
	if err != nil {
		return fmt.Errorf("billing: débit de la consommation: %w", err)
	}

	if debited > 0 {
		d.metrics.AddWalletDebits(debited)
		d.logger.InfoContext(ctx, "billing: consommation débitée",
			"movements", debited, "from", from.Format(time.RFC3339), "to", to.Format(time.RFC3339))
	}

	return nil
}

// credits convertit un coût mesuré (USD) en crédits, arrondi à l'entier
// supérieur : un appel facturé ne coûte jamais zéro crédit.
func (d *Debiter) credits(cost float64) int64 {
	if cost <= 0 {
		return 0
	}

	rate := d.rate
	if rate <= 0 {
		rate = d.cfg.Web.Credits.EffectiveUSDPerCredit()
	}
	credits := int64(cost / rate)
	if float64(credits)*rate < cost {
		credits++
	}

	return credits
}

// usageLabel traduit la nature d'un appel en libellé lisible par le client.
func usageLabel(kind, agent string) string {
	switch {
	case kind == "image":
		return "Usage — génération d'images"
	case kind == "transcription":
		return "Usage — notes vocales"
	case agent == "research":
		return "Usage — recherche"
	default:
		return "Usage — conversations"
	}
}

// ApplyAllowances remet à niveau, une fois par mois civil, le portefeuille
// des organisations offertes par la maison : le solde est ramené à
// l'allocation, jamais cumulé d'un mois sur l'autre.
func (d *Debiter) ApplyAllowances(ctx context.Context) error {
	now := d.now()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	var (
		lastRun time.Time
		found   bool
	)
	err := d.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		lastRun, found, err = d.runs.GetLastRun(ctx, tx, allowanceTaskName)
		return err
	})
	if err != nil {
		return fmt.Errorf("billing: lecture de la dernière allocation: %w", err)
	}

	// Déjà passé ce mois-ci.
	if found && !lastRun.Before(monthStart) {
		return nil
	}

	var granted int

	err = d.db.WithTx(ctx, func(tx *sql.Tx) error {
		orgs, err := d.orgs.List(ctx, tx, "")
		if err != nil {
			return err
		}

		for _, org := range orgs {
			// Une organisation sans limite n'a pas d'allocation à recevoir :
			// elle n'est jamais débitée, un solde n'aurait rien à compenser.
			if org.Unlimited {
				continue
			}
			if !org.Offered || org.MonthlyAllowance <= 0 {
				continue
			}

			balance, err := d.wallet.Balance(ctx, tx, org.ID)
			if err != nil {
				return err
			}

			// Remise à niveau : on complète jusqu'à l'allocation. Un solde
			// déjà supérieur (geste commercial du mois) n'est pas rogné.
			delta := org.MonthlyAllowance - balance
			if delta <= 0 {
				continue
			}

			if err := d.wallet.Insert(ctx, tx, persistence.WalletEntry{
				OrgID:     org.ID,
				Kind:      persistence.WalletKindAllowance,
				Label:     "Allocation mensuelle offerte",
				Amount:    delta,
				CreatedAt: now,
			}); err != nil {
				return err
			}
			granted++
		}

		return d.runs.SetLastRun(ctx, tx, allowanceTaskName, now)
	})
	if err != nil {
		return fmt.Errorf("billing: application des allocations: %w", err)
	}

	if granted > 0 {
		d.logger.InfoContext(ctx, "billing: allocations mensuelles appliquées", "organizations", granted)
	}

	return nil
}

// recordRun pose la borne haute d'une tâche.
func (d *Debiter) recordRun(ctx context.Context, task string, at time.Time) error {
	err := d.db.WithTx(ctx, func(tx *sql.Tx) error {
		return d.runs.SetLastRun(ctx, tx, task, at)
	})
	if err != nil {
		return fmt.Errorf("billing: enregistrement du passage %q: %w", task, err)
	}
	return nil
}
