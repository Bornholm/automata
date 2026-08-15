package registry

import (
	"context"
	"database/sql"
	"fmt"
	"io"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
)

// adminInspectLimit borne le nombre de lignes récentes affichées par
// "automata admin inspect" (PLAN.md Phase 18, "ajouter une commande
// d'inspection administrative") : une commande d'inspection au démarrage
// n'a pas vocation à défiler un historique complet, seulement à donner une
// vue rapide de l'état récent.
const adminInspectLimit = 50

// AdminInspectKindPlans et AdminInspectKindRuns sont les valeurs acceptées
// par AdminInspect.
const (
	AdminInspectKindPlans = "plans"
	AdminInspectKindRuns  = "runs"
)

// AdminInspect ouvre la même base SQLite que le processus principal
// (cfg.Storage.Application) et écrit sur out une vue en LECTURE SEULE
// (aucune mutation) de l'état récent des plans d'actions (kind ==
// AdminInspectKindPlans) ou des exécutions planifiées (kind ==
// AdminInspectKindRuns), pour l'administrateur d'une instance (PLAN.md
// Phase 18, "ajouter une commande d'inspection administrative").
func AdminInspect(ctx context.Context, cfg *config.Config, kind string, out io.Writer) error {
	db, err := persistence.Open(ctx, cfg.Storage.Application)
	if err != nil {
		return fmt.Errorf("registry: ouverture de la persistance: %w", err)
	}
	defer func() { _ = db.Close() }()

	switch kind {
	case AdminInspectKindPlans:
		return inspectActionPlans(ctx, db, out)
	case AdminInspectKindRuns:
		return inspectScheduledRuns(ctx, db, out)
	default:
		return fmt.Errorf("registry: vue d'inspection %q non supportée (attendu %q ou %q)", kind, AdminInspectKindPlans, AdminInspectKindRuns)
	}
}

// inspectActionPlans liste les action_plans récents avec leur statut,
// portée, nombre d'actions et dates de création/expiration.
func inspectActionPlans(ctx context.Context, db *persistence.DB, out io.Writer) error {
	plans := persistence.NewActionPlanRepository()
	actions := persistence.NewActionRepository()

	var rows []persistence.ActionPlan
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		rows, err = plans.ListRecent(ctx, tx, adminInspectLimit)
		if err != nil {
			return err
		}

		if len(rows) == 0 {
			return nil
		}

		fmt.Fprintf(out, "%-36s  %-20s  %-6s  %-6s  %5s  %-20s  %-20s\n", "id", "statut", "portée", "réf.", "act.", "créé le", "expire le")
		for _, p := range rows {
			acts, err := actions.ListByPlanID(ctx, tx, p.ID)
			if err != nil {
				return fmt.Errorf("liste des actions du plan %q: %w", p.ID, err)
			}

			expiresAt := "-"
			if p.ExpiresAt != nil {
				expiresAt = *p.ExpiresAt
			}

			fmt.Fprintf(out, "%-36s  %-20s  %-6s  %-6s  %5d  %-20s  %-20s\n",
				p.ID, p.Status, p.Scope, p.ScopeID, len(acts), p.CreatedAt, expiresAt)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("registry: inspection des plans d'actions: %w", err)
	}

	if len(rows) == 0 {
		fmt.Fprintln(out, "aucun plan d'actions enregistré")
	}

	return nil
}

// inspectScheduledRuns liste les scheduled_runs récents avec leur statut,
// schedule_id, date planifiée et statut de livraison.
func inspectScheduledRuns(ctx context.Context, db *persistence.DB, out io.Writer) error {
	runs := persistence.NewScheduledRunRepository()

	var rows []persistence.ScheduledRun
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		rows, err = runs.ListRecent(ctx, tx, adminInspectLimit)
		return err
	})
	if err != nil {
		return fmt.Errorf("registry: inspection des exécutions planifiées: %w", err)
	}

	if len(rows) == 0 {
		fmt.Fprintln(out, "aucune exécution planifiée enregistrée")
		return nil
	}

	fmt.Fprintf(out, "%-36s  %-20s  %-10s  %-20s  %-12s\n", "id", "schedule_id", "statut", "planifiée pour", "livraison")
	for _, r := range rows {
		delivery := "-"
		if r.DeliveryStatus != nil {
			delivery = *r.DeliveryStatus
		}

		fmt.Fprintf(out, "%-36s  %-20s  %-10s  %-20s  %-12s\n", r.ID, r.ScheduleID, r.Status, r.ScheduledFor, delivery)
	}

	return nil
}
