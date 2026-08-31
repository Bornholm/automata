package registry

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bornholm/go-courier"

	"github.com/bornholm/automata/internal/alerting"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/platform"
)

func openWatchdogDB(t *testing.T) *persistence.DB {
	t.Helper()

	db, err := persistence.Open(context.Background(), config.StorageApplication{
		Driver: "sqlite3",
		Path:   filepath.Join(t.TempDir(), "automata.sqlite"),
		Pragmas: config.Pragmas{
			ForeignKeys: true,
			JournalMode: "WAL",
			BusyTimeout: config.Duration(5 * time.Second),
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return db
}

// fakePlatforms rejoue l'état d'un gestionnaire de comptes.
type fakePlatforms struct {
	statuses  map[string]platform.Status
	providers map[string]courier.Provider
}

func (f *fakePlatforms) Statuses() map[string]platform.Status   { return f.statuses }
func (f *fakePlatforms) Providers() map[string]courier.Provider { return f.providers }

// stuckProvider ne répond jamais à Self : c'est le fournisseur bloqué sur un
// verrou, celui qui se déclare en marche et n'entend plus rien.
type stuckProvider struct{ courier.Provider }

func (stuckProvider) Self(ctx context.Context) (courier.User, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// liveProvider répond normalement.
type liveProvider struct{ courier.Provider }

func (liveProvider) Self(context.Context) (courier.User, error) {
	return courier.NewUser("bot", "Automata"), nil
}

// recordingSender note les alertes remises.
type recordingSender struct {
	mu   sync.Mutex
	sent []string
}

func (s *recordingSender) NotifyOperator(_ context.Context, _, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, message)
	return nil
}

func recentAlerts(t *testing.T, db *persistence.DB) []persistence.Alert {
	t.Helper()

	var alerts []persistence.Alert
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		alerts, err = persistence.NewAlertRepository().ListRecent(context.Background(), tx, 10)
		return err
	}); err != nil {
		t.Fatalf("relecture des alertes: %v", err)
	}

	return alerts
}

// LE test de la panne du 2026-08-30 : le compte Rocket.Chat s'est figé sur un
// verrou, le gestionnaire l'a cru « en marche » toute la nuit, et rien dans
// l'instance ne pouvait s'en apercevoir. L'état seul ne suffit pas — il faut
// interroger le fournisseur.
func TestWatchdog_DetectsAMutePlatform(t *testing.T) {
	db := openWatchdogDB(t)
	sender := &recordingSender{}

	guard := &watchdog{
		notifier: alerting.New(db, sender, slog.Default()),
		platforms: &fakePlatforms{
			statuses: map[string]platform.Status{
				"rocket": {ID: "rocket", Type: "rocket", State: platform.StateRunning},
			},
			providers: map[string]courier.Provider{"rocket": stuckProvider{}},
		},
		logger:       slog.Default(),
		now:          time.Now,
		probeTimeout: 50 * time.Millisecond,
	}

	guard.inspect(context.Background())

	alerts := recentAlerts(t, db)
	if len(alerts) != 1 {
		t.Fatalf("%d alerte(s), une attendue pour un compte muet", len(alerts))
	}
	if alerts[0].Kind != alerting.KindPlatformMute {
		t.Errorf("nature = %q, attendu %q", alerts[0].Kind, alerting.KindPlatformMute)
	}
	if alerts[0].Subject != "rocket" {
		t.Errorf("sujet = %q, attendu le nom du compte", alerts[0].Subject)
	}
}

// Un compte qui répond ne déclenche rien : la sonde ne doit pas transformer
// chaque tour de veille en alerte.
func TestWatchdog_LivePlatformIsSilent(t *testing.T) {
	db := openWatchdogDB(t)

	guard := &watchdog{
		notifier: alerting.New(db, &recordingSender{}, slog.Default()),
		platforms: &fakePlatforms{
			statuses: map[string]platform.Status{
				"whatsapp": {ID: "whatsapp", Type: "whatsapp", State: platform.StateRunning},
			},
			providers: map[string]courier.Provider{"whatsapp": liveProvider{}},
		},
		logger: slog.Default(),
		now:    time.Now,
	}

	guard.inspect(context.Background())

	if alerts := recentAlerts(t, db); len(alerts) != 0 {
		t.Errorf("%d alerte(s) pour un compte sain, aucune attendue", len(alerts))
	}
}

// Un compte déjà signalé en échec est traité par checkPlatforms : le sonder
// en plus donnerait deux alertes pour un seul problème.
func TestWatchdog_FailedPlatformIsNotProbedTwice(t *testing.T) {
	db := openWatchdogDB(t)

	guard := &watchdog{
		notifier: alerting.New(db, &recordingSender{}, slog.Default()),
		platforms: &fakePlatforms{
			statuses: map[string]platform.Status{
				"rocket": {
					ID: "rocket", Type: "rocket", State: platform.StateFailed,
					Since: time.Now().Add(-time.Hour), Err: "connexion perdue",
				},
			},
			providers: map[string]courier.Provider{"rocket": stuckProvider{}},
		},
		logger:       slog.Default(),
		now:          time.Now,
		probeTimeout: 50 * time.Millisecond,
	}

	guard.inspect(context.Background())

	alerts := recentAlerts(t, db)
	if len(alerts) != 1 {
		t.Fatalf("%d alerte(s), une seule attendue", len(alerts))
	}
	if alerts[0].Kind != alerting.KindPlatformDown {
		t.Errorf("nature = %q, attendu %q", alerts[0].Kind, alerting.KindPlatformDown)
	}
}
