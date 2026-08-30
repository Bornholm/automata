package alerting_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/alerting"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
)

func openTestDB(t *testing.T) *persistence.DB {
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

// fakeSender note ce qui est remis, et peut échouer sur commande.
type fakeSender struct {
	sent []string
	fail bool
}

func (s *fakeSender) NotifyOperator(_ context.Context, _, message string) error {
	if s.fail {
		return errors.New("messagerie injoignable")
	}
	s.sent = append(s.sent, message)
	return nil
}

// setOperator désigne le destinataire des alertes.
func setOperator(t *testing.T, db *persistence.DB, memberID string) {
	t.Helper()

	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return persistence.NewInstanceSettingRepository().
			Set(context.Background(), tx, persistence.SettingOperatorMemberID, memberID, time.Now())
	}); err != nil {
		t.Fatalf("désignation de l'exploitant: %v", err)
	}
}

func pendingCount(t *testing.T, db *persistence.DB) int {
	t.Helper()

	var pending []persistence.Alert
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		pending, err = persistence.NewAlertRepository().ListPending(context.Background(), tx, 100)
		return err
	}); err != nil {
		t.Fatalf("alertes en attente: %v", err)
	}

	return len(pending)
}

// Une alerte part, et la même alerte ne repart pas dans l'heure : une
// conversation inondée ne se lit plus, ce qui revient à n'alerter personne.
func TestNotify_SameAlertDoesNotRepeatWithinTheHour(t *testing.T) {
	db := openTestDB(t)
	setOperator(t, db, "cam")
	sender := &fakeSender{}

	now := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	notifier := alerting.New(db, sender, nil).WithClock(func() time.Time { return now })

	for range 3 {
		if err := notifier.Notify(context.Background(),
			alerting.KindPlatformDown, "whatsapp-perso", "le compte ne répond plus"); err != nil {
			t.Fatalf("Notify: %v", err)
		}
		now = now.Add(10 * time.Minute)
	}

	if len(sender.sent) != 1 {
		t.Errorf("%d alertes remises, une seule attendue dans l'heure", len(sender.sent))
	}

	// Passé le délai, l'alerte repart : le problème dure, il faut le redire.
	now = now.Add(time.Hour)
	if err := notifier.Notify(context.Background(),
		alerting.KindPlatformDown, "whatsapp-perso", "le compte ne répond toujours pas"); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if len(sender.sent) != 2 {
		t.Errorf("%d alertes remises, deux attendues après le délai", len(sender.sent))
	}
}

// Deux sujets différents sont deux alertes différentes, même nature.
func TestNotify_DistinctSubjectsAreDistinctAlerts(t *testing.T) {
	db := openTestDB(t)
	setOperator(t, db, "cam")
	sender := &fakeSender{}
	notifier := alerting.New(db, sender, nil)

	for _, subject := range []string{"whatsapp-perso", "whatsapp-pro"} {
		if err := notifier.Notify(context.Background(),
			alerting.KindPlatformDown, subject, "compte "+subject+" en panne"); err != nil {
			t.Fatalf("Notify: %v", err)
		}
	}

	if len(sender.sent) != 2 {
		t.Errorf("%d alertes remises, deux attendues", len(sender.sent))
	}
}

// LE cas qui compte : la panne touche le canal même qui devait porter
// l'alerte. Elle est conservée, puis rejouée quand le canal revient.
func TestNotify_UndeliveredAlertIsReplayedLater(t *testing.T) {
	db := openTestDB(t)
	setOperator(t, db, "cam")
	sender := &fakeSender{fail: true}
	notifier := alerting.New(db, sender, nil)

	if err := notifier.Notify(context.Background(),
		alerting.KindPlatformDown, "whatsapp-perso", "le compte ne répond plus"); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if len(sender.sent) != 0 {
		t.Fatal("aucune remise ne devait aboutir")
	}
	if pendingCount(t, db) != 1 {
		t.Fatal("l'alerte devrait rester en attente")
	}

	// Le canal revient.
	sender.fail = false
	notifier.Flush(context.Background())

	if len(sender.sent) != 1 {
		t.Errorf("%d alertes rejouées, une attendue", len(sender.sent))
	}
	if pendingCount(t, db) != 0 {
		t.Error("l'alerte remise ne devrait plus être en attente")
	}
}

// Sans exploitant désigné, l'alerte est tout de même enregistrée : elle
// reste consultable dans l'administration, et repartira le jour où
// quelqu'un est désigné.
func TestNotify_WithoutOperatorStillRecords(t *testing.T) {
	db := openTestDB(t)
	sender := &fakeSender{}
	notifier := alerting.New(db, sender, nil)

	if err := notifier.Notify(context.Background(),
		alerting.KindPluginFailed, "workspace", "le plugin est arrêté"); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	if len(sender.sent) != 0 {
		t.Error("aucune remise possible sans exploitant désigné")
	}
	if pendingCount(t, db) != 1 {
		t.Error("l'alerte devrait être enregistrée malgré tout")
	}

	setOperator(t, db, "cam")
	notifier.Flush(context.Background())
	if len(sender.sent) != 1 {
		t.Errorf("%d alertes remises après désignation, une attendue", len(sender.sent))
	}
}

// La purge borne le journal sans toucher aux alertes récentes.
func TestPurge_DropsOldAlertsOnly(t *testing.T) {
	db := openTestDB(t)
	sender := &fakeSender{}

	now := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	notifier := alerting.New(db, sender, nil).WithClock(func() time.Time { return now })

	if err := notifier.Notify(context.Background(),
		alerting.KindBackupFailed, "app", "sauvegarde en échec"); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	// Deux mois plus tard, l'alerte est hors rétention.
	now = now.Add(60 * 24 * time.Hour)
	notifier.Purge(context.Background())

	var recent []persistence.Alert
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		recent, err = persistence.NewAlertRepository().ListRecent(context.Background(), tx, 10)
		return err
	}); err != nil {
		t.Fatalf("relecture: %v", err)
	}
	if len(recent) != 0 {
		t.Errorf("%d alertes restantes, aucune attendue après purge", len(recent))
	}
}
