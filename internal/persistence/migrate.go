package persistence

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migration décrit une migration numérotée chargée depuis migrationsFS.
type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations lit et trie les migrations embarquées par numéro de
// version croissant.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("lecture du répertoire des migrations: %w", err)
	}

	migrations := make([]migration, 0, len(entries))

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}

		prefix, _, found := strings.Cut(name, "_")
		if !found {
			return nil, fmt.Errorf("nom de migration invalide %q: préfixe numérique attendu", name)
		}

		version, err := strconv.Atoi(prefix)
		if err != nil {
			return nil, fmt.Errorf("nom de migration invalide %q: %w", name, err)
		}

		content, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, fmt.Errorf("lecture de la migration %q: %w", name, err)
		}

		migrations = append(migrations, migration{
			version: version,
			name:    name,
			sql:     string(content),
		})
	}

	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].version < migrations[j].version
	})

	return migrations, nil
}

// migrate applique les migrations en attente sur sqlDB, dans une transaction
// par migration. Idempotent : une migration déjà appliquée n'est pas
// réexécutée.
func migrate(ctx context.Context, sqlDB *sql.DB) error {
	if _, err := sqlDB.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL
		)
	`); err != nil {
		return fmt.Errorf("création de schema_migrations: %w", err)
	}

	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	for _, m := range migrations {
		applied, err := isMigrationApplied(ctx, sqlDB, m.version)
		if err != nil {
			return fmt.Errorf("vérification de la migration %q: %w", m.name, err)
		}
		if applied {
			continue
		}

		if err := applyMigration(ctx, sqlDB, m); err != nil {
			return fmt.Errorf("application de la migration %q: %w", m.name, err)
		}
	}

	return nil
}

func isMigrationApplied(ctx context.Context, sqlDB *sql.DB, version int) (bool, error) {
	var count int
	row := sqlDB.QueryRowContext(ctx, `SELECT COUNT(1) FROM schema_migrations WHERE version = ?`, version)
	if err := row.Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func applyMigration(ctx context.Context, sqlDB *sql.DB, m migration) error {
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("début de transaction: %w", err)
	}

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("exécution du script SQL: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
		m.version, time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("enregistrement de la migration: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit de la migration: %w", err)
	}

	return nil
}
