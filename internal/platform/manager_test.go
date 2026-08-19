package platform_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bornholm/go-courier"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/platform"
)

func testDB(t *testing.T) *persistence.DB {
	t.Helper()

	db, err := persistence.Open(context.Background(), config.StorageApplication{
		Driver: "sqlite3",
		Path:   filepath.Join(t.TempDir(), "automata.sqlite"),
	})
	if err != nil {
		t.Fatalf("persistence.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return db
}

// fakeProvider est un courier.Provider inerte.
type fakeProvider struct{}

func (fakeProvider) Listen(ctx context.Context) (chan courier.Message, error) {
	return make(chan courier.Message), nil
}
func (fakeProvider) Send(context.Context, courier.Message) error { return nil }

// tracker enregistre les pipelines démarrés et arrêtés.
type tracker struct {
	mu      sync.Mutex
	started []string
	stopped []string
	// qr, s'il est renseigné, est émis comme code d'appairage au démarrage.
	qr string
}

func (tr *tracker) runner(ctx context.Context, id string, _ courier.Provider) error {
	tr.mu.Lock()
	tr.started = append(tr.started, id)
	tr.mu.Unlock()

	<-ctx.Done()

	tr.mu.Lock()
	tr.stopped = append(tr.stopped, id)
	tr.mu.Unlock()

	return ctx.Err()
}

func (tr *tracker) counts() (int, int) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return len(tr.started), len(tr.stopped)
}

func seedPlatform(t *testing.T, db *persistence.DB, p persistence.Platform) {
	t.Helper()

	now := time.Now()
	p.CreatedAt, p.UpdatedAt = now, now
	if p.Config == "" {
		raw, _ := json.Marshal(map[string]any{"session_path": "/tmp/session.db"})
		p.Config = string(raw)
	}

	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return persistence.NewPlatformRepository().Insert(context.Background(), tx, p, true)
	}); err != nil {
		t.Fatalf("seedPlatform: %v", err)
	}
}

func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// runManager démarre le gestionnaire et retourne sa fonction d'arrêt.
func runManager(t *testing.T, m *platform.Manager) func() {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = m.Run(ctx)
	}()

	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("le gestionnaire ne s'est pas arrêté")
		}
	}
}

func TestManager_StartsEnabledPlatformsAndReportsState(t *testing.T) {
	db := testDB(t)
	seedPlatform(t, db, persistence.Platform{ID: "whatsapp", Type: "whatsapp", Enabled: true})
	seedPlatform(t, db, persistence.Platform{ID: "signal", Type: "signal", Enabled: false})

	tr := &tracker{}
	manager := platform.NewManager(db, nil,
		func(string, string, map[string]any, func(string, bool)) (courier.Provider, error) {
			return fakeProvider{}, nil
		}, tr.runner, nil)

	stop := runManager(t, manager)
	defer stop()

	if !waitFor(t, func() bool {
		status, ok := manager.Status("whatsapp")
		return ok && status.State == platform.StateRunning
	}) {
		t.Fatalf("le compte actif n'a pas démarré: %+v", manager.Statuses())
	}

	// Un compte désactivé ne démarre pas : c'est ce qui permet de couper un
	// compte sans perdre sa session.
	if started, _ := tr.counts(); started != 1 {
		t.Errorf("%d pipeline(s) démarré(s), attendu 1", started)
	}
	if _, ok := manager.Status("signal"); ok {
		if status, _ := manager.Status("signal"); status.State == platform.StateRunning {
			t.Error("un compte désactivé ne doit jamais démarrer")
		}
	}

	// Le fournisseur est joignable pour l'envoi.
	if _, ok := manager.Get("whatsapp"); !ok {
		t.Error("le fournisseur d'un compte démarré doit être accessible")
	}
}

// Désactiver un compte l'arrête sans redémarrage du processus : c'est
// l'objet même du gestionnaire.
func TestManager_StopsPlatformWhenDisabled(t *testing.T) {
	db := testDB(t)
	seedPlatform(t, db, persistence.Platform{ID: "whatsapp", Type: "whatsapp", Enabled: true})

	tr := &tracker{}
	manager := platform.NewManager(db, nil,
		func(string, string, map[string]any, func(string, bool)) (courier.Provider, error) {
			return fakeProvider{}, nil
		}, tr.runner, nil)

	stop := runManager(t, manager)
	defer stop()

	if !waitFor(t, func() bool { started, _ := tr.counts(); return started == 1 }) {
		t.Fatal("le pipeline n'a pas démarré")
	}

	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		p, _, err := persistence.NewPlatformRepository().FindByID(context.Background(), tx, "whatsapp")
		if err != nil {
			return err
		}
		p.Enabled = false
		p.UpdatedAt = time.Now()
		return persistence.NewPlatformRepository().Update(context.Background(), tx, p)
	}); err != nil {
		t.Fatalf("désactivation: %v", err)
	}

	manager.Wake()

	if !waitFor(t, func() bool { _, stopped := tr.counts(); return stopped == 1 }) {
		t.Fatal("le pipeline n'a pas été arrêté après désactivation")
	}
	if !waitFor(t, func() bool {
		status, _ := manager.Status("whatsapp")
		return status.State == platform.StateStopped
	}) {
		t.Errorf("état inattendu après arrêt: %+v", manager.Statuses())
	}
	if _, ok := manager.Get("whatsapp"); ok {
		t.Error("le fournisseur d'un compte arrêté ne doit plus servir aux envois")
	}
}

// Un compte impossible à construire est signalé, sans empêcher les autres
// de tourner ni faire tomber le gestionnaire.
func TestManager_ReportsBuildFailure(t *testing.T) {
	db := testDB(t)
	seedPlatform(t, db, persistence.Platform{ID: "cassé", Type: "inconnu", Enabled: true})

	tr := &tracker{}
	manager := platform.NewManager(db, nil,
		func(_, providerType string, _ map[string]any, _ func(string, bool)) (courier.Provider, error) {
			if providerType == "inconnu" {
				return nil, errUnsupported
			}
			return fakeProvider{}, nil
		}, tr.runner, nil)

	stop := runManager(t, manager)
	defer stop()

	if !waitFor(t, func() bool {
		status, ok := manager.Status("cassé")
		return ok && status.State == platform.StateFailed && status.Err != ""
	}) {
		t.Fatalf("l'échec de construction n'a pas été signalé: %+v", manager.Statuses())
	}
}

// Le code d'appairage remonte jusqu'à l'administration : c'est ce qui
// permet d'afficher le QR dans le navigateur.
func TestManager_SurfacesPairingCode(t *testing.T) {
	db := testDB(t)
	seedPlatform(t, db, persistence.Platform{ID: "whatsapp", Type: "whatsapp", Enabled: true})

	tr := &tracker{}
	manager := platform.NewManager(db, nil,
		func(_, _ string, _ map[string]any, qr func(string, bool)) (courier.Provider, error) {
			qr("2@code-de-test", false)
			return fakeProvider{}, nil
		}, tr.runner, nil)

	stop := runManager(t, manager)
	defer stop()

	if !waitFor(t, func() bool {
		status, ok := manager.Status("whatsapp")
		return ok && status.Pairing() && status.PairingCode == "2@code-de-test"
	}) {
		t.Fatalf("le code d'appairage n'a pas été publié: %+v", manager.Statuses())
	}
}

var errUnsupported = errTest("type de compte non supporté")

type errTest string

func (e errTest) Error() string { return string(e) }
