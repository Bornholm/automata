// Package persistence fournit la persistance transactionnelle applicative
// d'Automata : ouverture de la base SQLite, migrations et repositories par
// table. Voir PLAN.md §13.1 pour le schéma conceptuel et la Phase 4 pour les
// travaux attendus.
package persistence

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/ncruces/go-sqlite3/driver"

	"github.com/bornholm/automata/internal/config"
)

// DB encapsule la connexion SQLite applicative et ses opérations
// transactionnelles.
type DB struct {
	sqlDB *sql.DB
}

// Open ouvre la base SQLite applicative au chemin décrit par cfg, applique
// les pragmas configurés et exécute les migrations en attente.
//
// SQLite n'autorise qu'un seul writer à la fois, y compris en mode WAL (WAL
// autorise plusieurs lecteurs concurrents à un writer, mais pas plusieurs
// writers). Pour éviter les erreurs SQLITE_BUSY côté application et laisser
// le busy_timeout SQLite gérer l'attente proprement, la connexion est donc
// limitée à une seule connexion ouverte (SetMaxOpenConns(1)). Cela sérialise
// les écritures au niveau du pool Go plutôt que de compter uniquement sur le
// busy_timeout SQLite.
func Open(ctx context.Context, cfg config.StorageApplication) (*DB, error) {
	if cfg.Path != "" && cfg.Path != ":memory:" {
		dir := filepath.Dir(cfg.Path)
		if dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("création du répertoire %q: %w", dir, err)
			}
		}
	}

	driver := cfg.Driver
	if driver == "" {
		driver = "sqlite3"
	}

	sqlDB, err := sql.Open(driver, "file:"+cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("ouverture de la base %q: %w", cfg.Path, err)
	}

	sqlDB.SetMaxOpenConns(1)

	if err := applyPragmas(ctx, sqlDB, cfg.Pragmas); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("application des pragmas: %w", err)
	}

	if err := migrate(ctx, sqlDB); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("exécution des migrations: %w", err)
	}

	return &DB{sqlDB: sqlDB}, nil
}

// applyPragmas applique les pragmas explicitement configurés sur la
// connexion sqlDB.
func applyPragmas(ctx context.Context, sqlDB *sql.DB, pragmas config.Pragmas) error {
	if pragmas.ForeignKeys {
		if _, err := sqlDB.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
			return fmt.Errorf("PRAGMA foreign_keys: %w", err)
		}
	} else {
		if _, err := sqlDB.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
			return fmt.Errorf("PRAGMA foreign_keys: %w", err)
		}
	}

	if pragmas.JournalMode != "" {
		if _, err := sqlDB.ExecContext(ctx, "PRAGMA journal_mode = "+pragmas.JournalMode); err != nil {
			return fmt.Errorf("PRAGMA journal_mode: %w", err)
		}
	}

	if pragmas.BusyTimeout.Duration() > 0 {
		ms := pragmas.BusyTimeout.Duration().Milliseconds()
		if _, err := sqlDB.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout = %d", ms)); err != nil {
			return fmt.Errorf("PRAGMA busy_timeout: %w", err)
		}
	}

	return nil
}

// Close ferme proprement la connexion à la base.
func (db *DB) Close() error {
	return db.sqlDB.Close()
}

// WithTx exécute fn au sein d'une transaction : commit si fn retourne nil,
// rollback si fn retourne une erreur ou panique. La panique est propagée
// après le rollback.
func (db *DB) WithTx(ctx context.Context, fn func(tx *sql.Tx) error) (err error) {
	tx, err := db.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("début de transaction: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()

	if err := fn(tx); err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			return fmt.Errorf("%w (rollback échoué: %v)", err, rollbackErr)
		}
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit de transaction: %w", err)
	}

	return nil
}

// Querier est implémenté à la fois par *sql.DB et *sql.Tx, permettant aux
// repositories d'être utilisés dans ou hors transaction.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

var (
	_ Querier = (*sql.DB)(nil)
	_ Querier = (*sql.Tx)(nil)
)
