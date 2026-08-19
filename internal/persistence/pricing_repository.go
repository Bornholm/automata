package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CreditPack est une offre d'achat de crédits (table credit_packs).
type CreditPack struct {
	ID        int64
	Credits   int64
	PriceEUR  float64
	Featured  bool
	Position  int
	CreatedAt time.Time
}

// Clés des réglages de l'instance (table settings).
const (
	// SettingUSDPerCredit est le coût réel couvert par un crédit, en
	// dollars : il convertit la consommation mesurée en débits.
	SettingUSDPerCredit = "credits.usd_per_credit"
	// SettingWelcomeCredits est le montant offert à la création d'une
	// organisation.
	SettingWelcomeCredits = "credits.welcome"
	// SettingDefaultAllowance est l'allocation mensuelle proposée par
	// défaut aux organisations offertes.
	SettingDefaultAllowance = "credits.default_allowance"
	// SettingEURPerUSD sert à comparer des recettes en euros à des coûts
	// en dollars ; c'est une estimation, jamais une conversion comptable.
	SettingEURPerUSD = "credits.eur_per_usd"
	// SettingTargetMargin est la marge visée sur la vente de crédits, en
	// pourcentage du prix payé. Elle ne contraint rien par elle-même :
	// elle sert à calculer le prix recommandé d'une offre et à signaler
	// celles qui passent en dessous — une offre vendue à perte doit se
	// voir avant d'être publiée, pas à la fin du mois.
	SettingTargetMargin = "credits.target_margin"
)

// DefaultTargetMargin est la marge visée par défaut : les coûts non
// rapportés par les fournisseurs, les crédits offerts et les échecs
// d'appels ne sont pas facturés au client mais pèsent sur le résultat.
const DefaultTargetMargin = 60.0

// PricingRepository donne accès aux packs de crédits et aux réglages de
// l'instance (migration 0013).
type PricingRepository struct{}

// NewPricingRepository crée un PricingRepository.
func NewPricingRepository() *PricingRepository {
	return &PricingRepository{}
}

// ListPacks retourne les offres, dans l'ordre d'affichage.
func (r *PricingRepository) ListPacks(ctx context.Context, q Querier) ([]CreditPack, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, credits, price_eur, featured, position, created_at
		FROM credit_packs ORDER BY position, credits`)
	if err != nil {
		return nil, fmt.Errorf("liste des packs de crédits: %w", err)
	}
	defer rows.Close()

	var packs []CreditPack
	for rows.Next() {
		var (
			pack      CreditPack
			featured  int
			createdAt string
		)
		if err := rows.Scan(&pack.ID, &pack.Credits, &pack.PriceEUR, &featured, &pack.Position, &createdAt); err != nil {
			return nil, fmt.Errorf("lecture d'un pack: %w", err)
		}
		pack.Featured = featured != 0
		if pack.CreatedAt, err = parseTenantTime(createdAt); err != nil {
			return nil, err
		}
		packs = append(packs, pack)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des packs: %w", err)
	}

	return packs, nil
}

// InsertPack ajoute une offre.
func (r *PricingRepository) InsertPack(ctx context.Context, q Querier, pack CreditPack) error {
	featured := 0
	if pack.Featured {
		featured = 1
	}

	_, err := q.ExecContext(ctx, `INSERT INTO credit_packs
		(credits, price_eur, featured, position, created_at) VALUES (?, ?, ?, ?, ?)`,
		pack.Credits, pack.PriceEUR, featured, pack.Position, formatTenantTime(pack.CreatedAt))
	if err != nil {
		return fmt.Errorf("insertion d'un pack de crédits: %w", err)
	}

	return nil
}

// DeletePack retire une offre.
func (r *PricingRepository) DeletePack(ctx context.Context, q Querier, id int64) error {
	if _, err := q.ExecContext(ctx, `DELETE FROM credit_packs WHERE id = ?`, id); err != nil {
		return fmt.Errorf("suppression du pack %d: %w", id, err)
	}

	return nil
}

// ClearFeatured retire la mise en avant de tous les packs : un seul pack
// peut être « le plus choisi » à la fois.
func (r *PricingRepository) ClearFeatured(ctx context.Context, q Querier) error {
	if _, err := q.ExecContext(ctx, `UPDATE credit_packs SET featured = 0`); err != nil {
		return fmt.Errorf("réinitialisation de la mise en avant: %w", err)
	}

	return nil
}

// SetFeatured met un pack en avant.
func (r *PricingRepository) SetFeatured(ctx context.Context, q Querier, id int64) error {
	if _, err := q.ExecContext(ctx, `UPDATE credit_packs SET featured = 1 WHERE id = ?`, id); err != nil {
		return fmt.Errorf("mise en avant du pack %d: %w", id, err)
	}

	return nil
}

// GetSetting lit un réglage ; found est faux s'il n'a jamais été fixé.
func (r *PricingRepository) GetSetting(ctx context.Context, q Querier, key string) (string, bool, error) {
	row := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key)

	var value string
	if err := row.Scan(&value); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("lecture du réglage %q: %w", key, err)
	}

	return value, true, nil
}

// SetSetting écrit un réglage.
func (r *PricingRepository) SetSetting(ctx context.Context, q Querier, key, value string, at time.Time) error {
	_, err := q.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, formatTenantTime(at))
	if err != nil {
		return fmt.Errorf("écriture du réglage %q: %w", key, err)
	}

	return nil
}

// Revenue agrège ce que les crédits ont rapporté et coûté sur une période.
type Revenue struct {
	// SoldCredits et SoldEUR : crédits achetés et somme réellement payée.
	SoldCredits int64
	SoldEUR     float64
	// GivenCredits : crédits offerts (bienvenue, gestes commerciaux,
	// allocations des organisations offertes) — ils ne rapportent rien
	// mais coûtent, et pèsent donc sur la marge.
	GivenCredits int64
	// UsedCredits : crédits consommés (débits d'usage).
	UsedCredits int64
}

// AggregateRevenue résume les mouvements de portefeuille de la période
// [from, to).
func (r *PricingRepository) AggregateRevenue(ctx context.Context, q Querier, from, to time.Time) (Revenue, error) {
	row := q.QueryRowContext(ctx, `SELECT
			COALESCE(SUM(CASE WHEN kind = 'purchase' THEN amount ELSE 0 END), 0),
			COALESCE(SUM(price_eur), 0),
			COALESCE(SUM(CASE WHEN kind IN ('welcome', 'grant', 'allowance') THEN amount ELSE 0 END), 0),
			COALESCE(-SUM(CASE WHEN amount < 0 THEN amount ELSE 0 END), 0)
		FROM wallet_entries WHERE created_at >= ? AND created_at < ?`,
		from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))

	var revenue Revenue
	if err := row.Scan(&revenue.SoldCredits, &revenue.SoldEUR, &revenue.GivenCredits, &revenue.UsedCredits); err != nil {
		return Revenue{}, fmt.Errorf("agrégation des recettes: %w", err)
	}

	return revenue, nil
}
