package backup_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/bornholm/automata/internal/persistence" // enregistre le pilote sqlite

	"github.com/bornholm/automata/internal/backup"
)

// seedDB crée une base SQLite avec une table et une ligne.
func seedDB(t *testing.T, path string) {
	t.Helper()

	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE notes (contenu TEXT); INSERT INTO notes VALUES ('souvenir')`); err != nil {
		t.Fatalf("préparation de la base: %v", err)
	}
}

func TestBackup_CopiesEachSourceAndKeepsDataReadable(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "app.sqlite")
	seedDB(t, source)

	target := filepath.Join(dir, "backups")
	runner := backup.New(backup.Options{
		Directory: target,
		Interval:  time.Hour,
		Sources:   []backup.Source{{Name: "app", Path: source}},
	}, nil)

	if err := runner.Backup(context.Background()); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatalf("lecture du répertoire: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("%d fichier(s) produit(s), attendu 1", len(entries))
	}

	// La copie doit être une base valide, pas un fichier tronqué : c'est
	// tout l'intérêt de VACUUM INTO plutôt qu'une copie brute.
	copyPath := filepath.Join(target, entries[0].Name())
	db, err := sql.Open("sqlite3", "file:"+copyPath+"?mode=ro")
	if err != nil {
		t.Fatalf("ouverture de la copie: %v", err)
	}
	defer db.Close()

	var contenu string
	if err := db.QueryRow(`SELECT contenu FROM notes`).Scan(&contenu); err != nil {
		t.Fatalf("lecture de la copie: %v", err)
	}
	if contenu != "souvenir" {
		t.Errorf("contenu %q, attendu %q", contenu, "souvenir")
	}

	// Une sauvegarde porte des données personnelles : elle ne doit pas
	// être lisible par les autres utilisateurs du système.
	info, err := os.Stat(copyPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("permissions %o, attendues 600", perm)
	}
}

func TestBackup_RotatesOldCopies(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "app.sqlite")
	seedDB(t, source)

	target := filepath.Join(dir, "backups")
	moment := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)

	runner := backup.New(backup.Options{
		Directory: target,
		Interval:  time.Hour,
		Keep:      2,
		Sources:   []backup.Source{{Name: "app", Path: source}},
	}, nil).WithClock(func() time.Time { return moment })

	for i := 0; i < 4; i++ {
		moment = moment.Add(time.Hour)
		if err := runner.Backup(context.Background()); err != nil {
			t.Fatalf("Backup %d: %v", i, err)
		}
	}

	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatalf("lecture du répertoire: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("%d copie(s) conservée(s), attendu 2", len(entries))
	}

	// Ce sont bien les plus récentes qui restent.
	for _, entry := range entries {
		if !strings.Contains(entry.Name(), "-13") && !strings.Contains(entry.Name(), "-14") {
			t.Errorf("copie inattendue conservée: %s", entry.Name())
		}
	}
}

// Une base absente n'est pas une anomalie (mémoire non configurée, session
// pas encore créée) : la sauvegarde des autres doit continuer.
func TestBackup_SkipsMissingSourcesWithoutFailing(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "app.sqlite")
	seedDB(t, present)

	target := filepath.Join(dir, "backups")
	runner := backup.New(backup.Options{
		Directory: target,
		Interval:  time.Hour,
		Sources: []backup.Source{
			{Name: "absente", Path: filepath.Join(dir, "jamais-creee.sqlite")},
			{Name: "app", Path: present},
			{Name: "memoire", Path: ""},
		},
	}, nil)

	if err := runner.Backup(context.Background()); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	entries, _ := os.ReadDir(target)
	if len(entries) != 1 {
		t.Fatalf("%d copie(s), attendu 1 (seule la base existante)", len(entries))
	}
	if !strings.HasPrefix(entries[0].Name(), "app-") {
		t.Errorf("copie inattendue: %s", entries[0].Name())
	}
}

// Aucune copie partielle ne doit subsister : elle serait prise pour une
// sauvegarde valide le jour où l'on en a besoin.
func TestBackup_LeavesNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "app.sqlite")
	seedDB(t, source)

	target := filepath.Join(dir, "backups")
	runner := backup.New(backup.Options{
		Directory: target,
		Interval:  time.Hour,
		Sources:   []backup.Source{{Name: "app", Path: source}},
	}, nil)

	if err := runner.Backup(context.Background()); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	entries, _ := os.ReadDir(target)
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".part") {
			t.Fatalf("copie partielle laissée en place: %s", entry.Name())
		}
	}

	fmt.Fprint(os.Stderr, "")
}
