package persistence

import (
	"context"
	"fmt"
)

// WalletRepository donne accès à la table wallet_entries (migration 0010) :
// le livre de comptes immuable du portefeuille de crédits de chaque
// organisation. Le solde n'est jamais stocké : c'est la somme des
// mouvements.
type WalletRepository struct{}

// NewWalletRepository crée un WalletRepository.
func NewWalletRepository() *WalletRepository {
	return &WalletRepository{}
}

// Insert enregistre le mouvement e (l'identifiant auto-incrémenté est
// laissé à SQLite).
func (r *WalletRepository) Insert(ctx context.Context, q Querier, e WalletEntry) error {
	_, err := q.ExecContext(ctx, `INSERT INTO wallet_entries
		(org_id, kind, label, amount, created_at) VALUES (?, ?, ?, ?, ?)`,
		e.OrgID, e.Kind, e.Label, e.Amount, formatTenantTime(e.CreatedAt))
	if err != nil {
		return fmt.Errorf("insertion d'un mouvement de portefeuille (%s): %w", e.OrgID, err)
	}

	return nil
}

// Balance retourne le solde de l'organisation (0 si aucun mouvement).
func (r *WalletRepository) Balance(ctx context.Context, q Querier, orgID string) (int64, error) {
	row := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(amount), 0) FROM wallet_entries WHERE org_id = ?`, orgID)

	var balance int64
	if err := row.Scan(&balance); err != nil {
		return 0, fmt.Errorf("solde du portefeuille de %q: %w", orgID, err)
	}

	return balance, nil
}

// Balances retourne le solde de chaque organisation ayant au moins un
// mouvement, en une seule requête (liste ADM-02).
func (r *WalletRepository) Balances(ctx context.Context, q Querier) (map[string]int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT org_id, SUM(amount) FROM wallet_entries GROUP BY org_id`)
	if err != nil {
		return nil, fmt.Errorf("soldes des portefeuilles: %w", err)
	}
	defer rows.Close()

	balances := map[string]int64{}
	for rows.Next() {
		var orgID string
		var balance int64
		if err := rows.Scan(&orgID, &balance); err != nil {
			return nil, fmt.Errorf("lecture d'un solde: %w", err)
		}
		balances[orgID] = balance
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des soldes: %w", err)
	}

	return balances, nil
}

// List retourne les mouvements de l'organisation, les plus récents
// d'abord, bornés à limit (<= 0 : tous).
func (r *WalletRepository) List(ctx context.Context, q Querier, orgID string, limit int) ([]WalletEntry, error) {
	query := `SELECT id, org_id, kind, label, amount, created_at FROM wallet_entries
		WHERE org_id = ? ORDER BY created_at DESC, id DESC`
	args := []any{orgID}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("mouvements du portefeuille de %q: %w", orgID, err)
	}
	defer rows.Close()

	var entries []WalletEntry
	for rows.Next() {
		var e WalletEntry
		var createdAt string
		if err := rows.Scan(&e.ID, &e.OrgID, &e.Kind, &e.Label, &e.Amount, &createdAt); err != nil {
			return nil, fmt.Errorf("lecture d'un mouvement: %w", err)
		}
		if e.CreatedAt, err = parseTenantTime(createdAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des mouvements: %w", err)
	}

	return entries, nil
}

// LastCredit retourne le montant du dernier mouvement positif de
// l'organisation (référence de la jauge de solde des maquettes : « 210
// sur 2 500 »), ou 0 s'il n'y en a aucun.
func (r *WalletRepository) LastCredit(ctx context.Context, q Querier, orgID string) (int64, error) {
	row := q.QueryRowContext(ctx, `SELECT COALESCE((SELECT amount FROM wallet_entries
		WHERE org_id = ? AND amount > 0 ORDER BY created_at DESC, id DESC LIMIT 1), 0)`, orgID)

	var amount int64
	if err := row.Scan(&amount); err != nil {
		return 0, fmt.Errorf("dernier apport du portefeuille de %q: %w", orgID, err)
	}

	return amount, nil
}
