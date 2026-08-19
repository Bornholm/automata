package billing

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// lowBalanceRatio définit le seuil d'alerte : moins de 15 % du dernier
// apport. Le même que celui des écrans, pour qu'une organisation signalée
// dans l'administration soit exactement celle qui a été prévenue.
const lowBalanceRatio = 0.15

// Notifier porte l'alerte jusqu'à l'organisation, dans sa conversation.
// Implémenté par internal/registry, qui connaît les canaux et les
// fournisseurs ; nil désactive les alertes.
type Notifier interface {
	// NotifyLowBalance prévient une organisation que ses crédits
	// s'épuisent. balance est le solde restant.
	NotifyLowBalance(ctx context.Context, orgID string, balance int64) error
}

// WithNotifier branche l'alerte de solde bas. Sans elle, une organisation
// découvre la coupure au moment où elle arrive — le pire moment.
func (d *Debiter) WithNotifier(notifier Notifier) *Debiter {
	d.notifier = notifier
	return d
}

// notifyLowBalances prévient les organisations dont le solde vient de
// passer sous le seuil, une seule fois par descente : l'horodatage
// d'alerte est effacé dès que le portefeuille repasse au-dessus, de sorte
// qu'une recharge suivie d'une nouvelle baisse redonne droit à un
// avertissement.
func (d *Debiter) notifyLowBalances(ctx context.Context) error {
	if d.notifier == nil {
		return nil
	}

	type candidate struct {
		orgID   string
		balance int64
	}

	var (
		toNotify []candidate
		toReset  []string
	)

	err := d.db.WithTx(ctx, func(tx *sql.Tx) error {
		orgs, err := d.orgs.List(ctx, tx, "")
		if err != nil {
			return err
		}

		for _, org := range orgs {
			// Une organisation offerte n'a rien à recharger : la prévenir
			// n'appellerait aucune action de sa part.
			if org.Offered {
				continue
			}

			balance, err := d.wallet.Balance(ctx, tx, org.ID)
			if err != nil {
				return err
			}
			lastCredit, err := d.wallet.LastCredit(ctx, tx, org.ID)
			if err != nil {
				return err
			}

			// Sans apport de référence, le seuil n'a pas de sens ; et un
			// solde nul relève de la mise en pause, pas de l'alerte.
			low := lastCredit > 0 && balance > 0 && float64(balance) < float64(lastCredit)*lowBalanceRatio

			switch {
			case low && org.LowBalanceNotifiedAt.IsZero():
				toNotify = append(toNotify, candidate{org.ID, balance})
			case !low && !org.LowBalanceNotifiedAt.IsZero():
				toReset = append(toReset, org.ID)
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("billing: recherche des soldes bas: %w", err)
	}

	for _, org := range toNotify {
		if err := d.notifier.NotifyLowBalance(ctx, org.orgID, org.balance); err != nil {
			// Une alerte qui ne part pas ne doit pas être marquée comme
			// envoyée : le passage suivant réessaiera.
			d.logger.ErrorContext(ctx, "billing: alerte de solde bas non délivrée", "org_id", org.orgID, "error", err)
			continue
		}

		if err := d.markNotified(ctx, org.orgID, d.now()); err != nil {
			return err
		}
		d.logger.InfoContext(ctx, "billing: organisation prévenue de son solde bas", "org_id", org.orgID, "balance", org.balance)
	}

	for _, orgID := range toReset {
		if err := d.markNotified(ctx, orgID, time.Time{}); err != nil {
			return err
		}
	}

	return nil
}

// markNotified inscrit (ou efface) la date de la dernière alerte.
func (d *Debiter) markNotified(ctx context.Context, orgID string, at time.Time) error {
	err := d.db.WithTx(ctx, func(tx *sql.Tx) error {
		org, found, err := d.orgs.FindByID(ctx, tx, orgID)
		if err != nil || !found {
			return err
		}
		org.LowBalanceNotifiedAt = at
		org.UpdatedAt = d.now()
		return d.orgs.Update(ctx, tx, org)
	})
	if err != nil {
		return fmt.Errorf("billing: enregistrement de l'alerte de %q: %w", orgID, err)
	}

	return nil
}
