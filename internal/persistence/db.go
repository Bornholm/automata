// Package persistence fournit la persistance transactionnelle applicative
// d'Automata : ouverture de la base SQLite, migrations et repositories par
// table. Voir plan de conception, §13.1 pour le schéma conceptuel et la Phase 4 pour les
// travaux attendus.
package persistence

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	sqlitedriver "github.com/ncruces/go-sqlite3/driver"

	"github.com/bornholm/automata/internal/secretbox"
	// Embarque le binaire SQLite WASM : requis par go-sqlite3 v0.23.x
	// (épinglé par compatibilité avec l'index sémantique d'amoxtli, voir
	// go.mod), sans quoi toute ouverture échoue avec « no SQLite binary
	// embed/set/loaded ». L'index sqlitevec remplace ensuite ce binaire par
	// sa variante vec0 (EnsureVecWASM), un sur-ensemble qui sert tout aussi
	// bien la persistance applicative.
	_ "github.com/ncruces/go-sqlite3/embed"

	"github.com/bornholm/automata/internal/config"
)

// init enregistre un alias "sqlite" pour le driver database/sql fourni par
// github.com/ncruces/go-sqlite3/driver, qui ne s'enregistre lui-même que
// sous le nom "sqlite3" (son comportement par défaut, voir la documentation
// du paquet driver). Or plan de conception, §12 et tous les exemples de configuration
// de ce dépôt (internal/config/testdata/valid/config.yaml,
// docs/deployment.md) utilisent systématiquement "storage.application.driver:
// sqlite" — sans le "3". Sans cet alias, une configuration suivant cette
// convention documentée échoue au démarrage réel avec "sql: unknown driver
// \"sqlite\"" (constaté lors du test de déploiement Docker, Phase 22) :
// jamais détecté avant, car les tests existants (internal/persistence,
// internal/e2e) construisent leur config.StorageApplication directement en
// Go avec Driver: "sqlite3", sans jamais charger le fichier YAML d'exemple
// canonique via un run réel du binaire.
func init() {
	sql.Register("sqlite", &sqlitedriver.SQLite{})
}

// DB encapsule la connexion SQLite applicative et ses opérations
// transactionnelles.
type DB struct {
	sqlDB *sql.DB
	// cipher chiffre les contenus personnels au repos. Nil : ils sont
	// écrits en clair (voir storage.encryption_key).
	cipher *secretbox.Box
}

// Vacuum reconstruit le fichier de base et tronque son journal WAL. Après
// une migration de chiffrement, c'est ce qui efface les restes du clair :
// les pages mortes du fichier et le journal gardent sinon les anciennes
// versions des lignes.
func (db *DB) Vacuum(ctx context.Context) error {
	// VACUUM refuse de s'exécuter dans une transaction : accès direct.
	if _, err := db.sqlDB.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("vacuum de la base: %w", err)
	}
	if _, err := db.sqlDB.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("troncature du journal WAL: %w", err)
	}
	return nil
}

// Cipher retourne le chiffrement des contenus, ou nil s'il n'est pas
// configuré. Les repositories porteurs de contenu le reçoivent à la
// construction : le passer explicitement rend impossible d'ouvrir un
// dépôt de contenu sans avoir décidé de son sort.
func (db *DB) Cipher() *secretbox.Box {
	return db.cipher
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
// OpenWithEncryption ouvre la base et arme le chiffrement des contenus
// avec la clé fournie. Une clé vide laisse les contenus en clair.
func OpenWithEncryption(ctx context.Context, cfg config.StorageApplication, encryptionKey string) (*DB, error) {
	db, err := Open(ctx, cfg)
	if err != nil {
		return nil, err
	}

	if encryptionKey != "" {
		cipher, err := secretbox.NewContentBox(encryptionKey)
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("chiffrement des contenus: %w", err)
		}
		db.cipher = cipher
	}

	return db, nil
}

func Open(ctx context.Context, cfg config.StorageApplication) (*DB, error) {
	isFile := cfg.Path != "" && cfg.Path != ":memory:"

	if isFile {
		dir := filepath.Dir(cfg.Path)
		if dir != "" && dir != "." {
			// 0o700 : la base contient potentiellement des données
			// personnelles (plan de conception, Phase 19, point 5) ; seul le
			// propriétaire du processus doit pouvoir y accéder.
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, fmt.Errorf("création du répertoire %q: %w", dir, err)
			}
		}

		// Pré-créer le fichier avec des permissions restrictives (0o600)
		// avant de laisser le driver SQLite l'ouvrir : sql.Open seul
		// créerait le fichier selon l'umask du processus, potentiellement
		// lisible par d'autres utilisateurs du système (plan de conception, Phase 19,
		// point 5 "restreindre les permissions des fichiers SQLite" — la
		// base contient des données personnelles, voir plan de conception, §13).
		f, err := os.OpenFile(cfg.Path, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return nil, fmt.Errorf("création du fichier de base %q: %w", cfg.Path, err)
		}
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("fermeture du fichier de base %q: %w", cfg.Path, err)
		}
		// Un fichier préexistant (créé avant ce durcissement, ou par un
		// umask permissif) garde ses permissions d'origine avec
		// O_CREATE seul : les forcer explicitement.
		if err := os.Chmod(cfg.Path, 0o600); err != nil {
			return nil, fmt.Errorf("restriction des permissions du fichier de base %q: %w", cfg.Path, err)
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

	if isFile {
		// Les fichiers annexes WAL/SHM (journal_mode=WAL) sont créés par
		// SQLite lui-même lors des pragmas/migrations ci-dessus, après la
		// restriction de permissions du fichier principal : les restreindre
		// à leur tour, au meilleur effort (leur absence, ex. journal_mode
		// autre que WAL, n'est pas une erreur).
		for _, suffix := range []string{"-wal", "-shm"} {
			_ = os.Chmod(cfg.Path+suffix, 0o600)
		}
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
