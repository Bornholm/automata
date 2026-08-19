// Package backup copie périodiquement les bases SQLite de l'instance vers
// un répertoire de sauvegarde, et fait tourner les copies anciennes.
//
// La copie se fait par « VACUUM INTO », qui produit une base cohérente
// pendant que le service écrit : copier le fichier à chaud, lui, donnerait
// une sauvegarde corrompue une fois sur deux en mode WAL. Chaque base est
// traitée séparément — la base applicative porte les conversations et les
// portefeuilles, la mémoire porte les souvenirs, la session de messagerie
// évite un ré-appairage : perdre l'une d'elles ne s'improvise pas.
package backup

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Source décrit une base à sauvegarder.
type Source struct {
	// Name nomme la base dans les fichiers produits (« app », « memory »).
	Name string
	// Path est le fichier SQLite source.
	Path string
	// Driver est le pilote d'ouverture ; vide = sqlite3.
	Driver string
}

// Options gouverne la tâche de sauvegarde.
type Options struct {
	// Directory reçoit les copies.
	Directory string
	// Interval sépare deux sauvegardes.
	Interval time.Duration
	// Keep borne le nombre de copies conservées par base (<= 0 : toutes).
	Keep    int
	Sources []Source
}

// Runner exécute les sauvegardes périodiques.
type Runner struct {
	opts   Options
	logger *slog.Logger
	now    func() time.Time
}

// New construit le Runner.
func New(opts Options, logger *slog.Logger) *Runner {
	if logger == nil {
		logger = slog.Default()
	}

	return &Runner{opts: opts, logger: logger, now: time.Now}
}

// WithClock remplace l'horloge (tests).
func (r *Runner) WithClock(now func() time.Time) *Runner {
	r.now = now
	return r
}

// Run sauvegarde au démarrage puis à chaque intervalle, jusqu'à
// l'annulation de ctx. Un échec est journalisé sans interrompre la boucle :
// une sauvegarde ratée ne doit jamais arrêter le service qu'elle protège.
func (r *Runner) Run(ctx context.Context) error {
	if err := r.Backup(ctx); err != nil && ctx.Err() == nil {
		r.logger.ErrorContext(ctx, "backup: échec de la sauvegarde initiale", "error", err)
	}

	ticker := time.NewTicker(r.opts.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := r.Backup(ctx); err != nil && ctx.Err() == nil {
				r.logger.ErrorContext(ctx, "backup: échec d'une sauvegarde", "error", err)
			}
		}
	}
}

// Backup copie chaque source une fois, puis fait tourner les anciennes.
// Les erreurs d'une source n'empêchent pas les suivantes : mieux vaut
// sauvegarder trois bases sur quatre que rien du tout.
func (r *Runner) Backup(ctx context.Context) error {
	if err := os.MkdirAll(r.opts.Directory, 0o700); err != nil {
		return fmt.Errorf("backup: création du répertoire %q: %w", r.opts.Directory, err)
	}

	stamp := r.now().UTC().Format("20060102-150405")

	var firstErr error
	for _, source := range r.opts.Sources {
		if source.Path == "" || source.Path == ":memory:" {
			continue
		}
		if _, err := os.Stat(source.Path); err != nil {
			// Une base pas encore créée n'est pas une anomalie.
			continue
		}

		target := filepath.Join(r.opts.Directory, fmt.Sprintf("%s-%s.sqlite", source.Name, stamp))
		if err := r.copy(ctx, source, target); err != nil {
			r.logger.ErrorContext(ctx, "backup: sauvegarde impossible", "source", source.Name, "error", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		size := int64(0)
		if info, err := os.Stat(target); err == nil {
			size = info.Size()
		}
		r.logger.InfoContext(ctx, "backup: base sauvegardée", "source", source.Name, "bytes", size)

		if err := r.rotate(source.Name); err != nil {
			r.logger.ErrorContext(ctx, "backup: rotation impossible", "source", source.Name, "error", err)
		}
	}

	return firstErr
}

// copy produit une copie cohérente de la base source.
func (r *Runner) copy(ctx context.Context, source Source, target string) error {
	driver := source.Driver
	if driver == "" {
		driver = "sqlite3"
	}

	// Le chemin devient un DSN : « ? » et « # » y séparent les paramètres
	// et le fragment. Un fichier qui en porte (la session WhatsApp de
	// go-courier s'appelle « whatsapp?_foreign_keys=on ») serait sinon
	// ouvert sous un autre nom, ou pas ouvert du tout.
	escaped := strings.NewReplacer("?", "%3F", "#", "%23").Replace(source.Path)

	db, err := sql.Open(driver, "file:"+escaped+"?mode=ro")
	if err != nil {
		return fmt.Errorf("ouverture de %q: %w", source.Path, err)
	}
	defer db.Close()

	// Une copie partielle laissée en place serait pire que pas de copie :
	// on écrit sous un nom temporaire, puis on renomme.
	temporary := target + ".part"
	_ = os.Remove(temporary)

	// VACUUM INTO n'accepte pas de paramètre lié : le chemin est construit
	// par l'application (répertoire configuré + horodatage), jamais par un
	// tiers, et les apostrophes sont échappées par précaution.
	if _, err := db.ExecContext(ctx, "VACUUM INTO '"+strings.ReplaceAll(temporary, "'", "''")+"'"); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("copie de %q: %w", source.Path, err)
	}

	if err := os.Rename(temporary, target); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("publication de la copie %q: %w", target, err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		return fmt.Errorf("restriction des permissions de %q: %w", target, err)
	}

	return nil
}

// rotate supprime les copies les plus anciennes d'une source au-delà de
// la rétention configurée.
func (r *Runner) rotate(name string) error {
	if r.opts.Keep <= 0 {
		return nil
	}

	entries, err := os.ReadDir(r.opts.Directory)
	if err != nil {
		return fmt.Errorf("lecture du répertoire de sauvegarde: %w", err)
	}

	var copies []string
	prefix := name + "-"
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.HasPrefix(entry.Name(), prefix) && strings.HasSuffix(entry.Name(), ".sqlite") {
			copies = append(copies, entry.Name())
		}
	}

	if len(copies) <= r.opts.Keep {
		return nil
	}

	// Les noms portent un horodatage trié lexicographiquement : l'ordre
	// alphabétique est l'ordre chronologique.
	sort.Strings(copies)

	for _, obsolete := range copies[:len(copies)-r.opts.Keep] {
		if err := os.Remove(filepath.Join(r.opts.Directory, obsolete)); err != nil {
			return fmt.Errorf("suppression de la copie %q: %w", obsolete, err)
		}
	}

	return nil
}
