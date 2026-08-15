package registry_test

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/registry"
)

// testStorageConfig construit une configuration de stockage SQLite
// temporaire, sur le même modèle que internal/action et internal/scheduler.
func testStorageConfig(t *testing.T) config.StorageApplication {
	t.Helper()
	return config.StorageApplication{
		Driver: "sqlite3",
		Path:   filepath.Join(t.TempDir(), "automata.sqlite"),
		Pragmas: config.Pragmas{
			ForeignKeys: true,
			JournalMode: "WAL",
			BusyTimeout: config.Duration(5 * time.Second),
		},
	}
}

// TestAdminInspect_EmptyDatabase vérifie que "automata admin inspect"
// (PLAN.md §18, "ajouter une commande d'inspection administrative") ouvre
// la base sans erreur et affiche un message explicite lorsqu'aucun plan
// d'actions ni aucune exécution planifiée n'existe encore — cas normal pour
// une instance qui vient d'être installée.
func TestAdminInspect_EmptyDatabase(t *testing.T) {
	cfg := &config.Config{Storage: config.Storage{Application: testStorageConfig(t)}}

	var plansOut bytes.Buffer
	if err := registry.AdminInspect(context.Background(), cfg, registry.AdminInspectKindPlans, &plansOut); err != nil {
		t.Fatalf("AdminInspect(plans): %v", err)
	}
	if !strings.Contains(plansOut.String(), "aucun plan") {
		t.Fatalf("message attendu pour une base vide, obtenu: %q", plansOut.String())
	}

	var runsOut bytes.Buffer
	if err := registry.AdminInspect(context.Background(), cfg, registry.AdminInspectKindRuns, &runsOut); err != nil {
		t.Fatalf("AdminInspect(runs): %v", err)
	}
	if !strings.Contains(runsOut.String(), "aucune exécution") {
		t.Fatalf("message attendu pour une base vide, obtenu: %q", runsOut.String())
	}
}

// TestAdminInspect_UnknownKind vérifie qu'une vue inconnue est rejetée
// clairement plutôt que silencieusement ignorée.
func TestAdminInspect_UnknownKind(t *testing.T) {
	cfg := &config.Config{Storage: config.Storage{Application: testStorageConfig(t)}}

	var out bytes.Buffer
	err := registry.AdminInspect(context.Background(), cfg, "bogus", &out)
	if err == nil {
		t.Fatal("une vue inconnue aurait dû être rejetée")
	}
}
