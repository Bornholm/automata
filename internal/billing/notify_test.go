package billing_test

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/billing"
	"github.com/bornholm/automata/internal/persistence"
)

// recordingNotifier retient les alertes émises.
type recordingNotifier struct {
	mu     sync.Mutex
	alerts []string
	fail   bool
}

func (n *recordingNotifier) NotifyLowBalance(_ context.Context, orgID string, _ int64) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.fail {
		return errNotified
	}
	n.alerts = append(n.alerts, orgID)

	return nil
}

func (n *recordingNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.alerts)
}

type errString string

func (e errString) Error() string { return string(e) }

const errNotified = errString("envoi impossible")

// setBalance amène le portefeuille d'une organisation au solde voulu, en
// partant d'un apport de référence.
func setBalance(t *testing.T, db *persistence.DB, orgID string, credit, spend int64) {
	t.Helper()

	wallet := persistence.NewWalletRepository()
	now := time.Now()

	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		if err := wallet.Insert(context.Background(), tx, persistence.WalletEntry{
			OrgID: orgID, Kind: persistence.WalletKindPurchase, Label: "achat",
			Amount: credit, CreatedAt: now,
		}); err != nil {
			return err
		}
		if spend == 0 {
			return nil
		}
		return wallet.Insert(context.Background(), tx, persistence.WalletEntry{
			OrgID: orgID, Kind: persistence.WalletKindUsage, Label: "usage",
			Amount: -spend, CreatedAt: now.Add(time.Second),
		})
	})
	if err != nil {
		t.Fatalf("setBalance: %v", err)
	}
}

func TestNotify_WarnsOnceWhenBalanceGetsLow(t *testing.T) {
	db := testDB(t)
	seedOrg(t, db, persistence.Organization{ID: "atelier", DisplayName: "Atelier"})
	setBalance(t, db, "atelier", 1000, 900) // 100 restants, soit 10 % de l'apport

	notifier := &recordingNotifier{}
	debiter := billing.New(db, testCfg(), nil, nil).WithNotifier(notifier)

	if err := debiter.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if notifier.count() != 1 {
		t.Fatalf("%d alerte(s), attendu 1", notifier.count())
	}

	// Le passage suivant ne répète pas : prévenir est utile, harceler
	// ferait fuir.
	if err := debiter.Tick(context.Background()); err != nil {
		t.Fatalf("Tick (2): %v", err)
	}
	if notifier.count() != 1 {
		t.Errorf("%d alerte(s) après second passage, attendu 1", notifier.count())
	}
}

// Une recharge remet le compteur à zéro : une nouvelle descente doit
// pouvoir déclencher une nouvelle alerte.
func TestNotify_ResetsAfterTopUp(t *testing.T) {
	db := testDB(t)
	seedOrg(t, db, persistence.Organization{ID: "atelier", DisplayName: "Atelier"})
	setBalance(t, db, "atelier", 1000, 900)

	notifier := &recordingNotifier{}
	debiter := billing.New(db, testCfg(), nil, nil).WithNotifier(notifier)

	if err := debiter.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	setBalance(t, db, "atelier", 2000, 0)
	if err := debiter.Tick(context.Background()); err != nil {
		t.Fatalf("Tick après recharge: %v", err)
	}
	if notifier.count() != 1 {
		t.Fatalf("%d alerte(s), attendu 1 (aucune alerte sur un solde reconstitué)", notifier.count())
	}

	setBalance(t, db, "atelier", 0, 1900)
	if err := debiter.Tick(context.Background()); err != nil {
		t.Fatalf("Tick après nouvelle baisse: %v", err)
	}
	if notifier.count() != 2 {
		t.Errorf("%d alerte(s), attendu 2 après une nouvelle descente", notifier.count())
	}
}

// Une organisation offerte n'a rien à recharger : l'avertir ne lui
// donnerait aucune action possible.
func TestNotify_SkipsOfferedOrganizations(t *testing.T) {
	db := testDB(t)
	seedOrg(t, db, persistence.Organization{ID: "offerte", DisplayName: "Offerte", Offered: true, MonthlyAllowance: 600})
	setBalance(t, db, "offerte", 600, 550)

	notifier := &recordingNotifier{}
	if err := billing.New(db, testCfg(), nil, nil).WithNotifier(notifier).Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if notifier.count() != 0 {
		t.Errorf("%d alerte(s), attendu 0 pour une organisation offerte", notifier.count())
	}
}

// Une alerte qui n'a pas pu partir ne doit pas être considérée comme
// envoyée : sinon le client ne serait jamais prévenu.
func TestNotify_RetriesAfterDeliveryFailure(t *testing.T) {
	db := testDB(t)
	seedOrg(t, db, persistence.Organization{ID: "atelier", DisplayName: "Atelier"})
	setBalance(t, db, "atelier", 1000, 900)

	notifier := &recordingNotifier{fail: true}
	debiter := billing.New(db, testCfg(), nil, nil).WithNotifier(notifier)

	if err := debiter.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	notifier.fail = false
	if err := debiter.Tick(context.Background()); err != nil {
		t.Fatalf("Tick (2): %v", err)
	}

	if notifier.count() != 1 {
		t.Errorf("%d alerte(s) délivrée(s), attendu 1 après reprise", notifier.count())
	}
}
