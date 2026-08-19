package billing_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/billing"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/usage"
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

func testCfg() *config.Config {
	// 0,001 $ le crédit : 1 $ de consommation = 1 000 crédits.
	return &config.Config{Web: config.Web{Credits: config.WebCredits{USDPerCredit: 0.001}}}
}

func seedOrg(t *testing.T, db *persistence.DB, org persistence.Organization) {
	t.Helper()

	now := time.Now()
	org.CreatedAt, org.UpdatedAt = now, now
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return persistence.NewOrganizationRepository().Insert(context.Background(), tx, org, true)
	}); err != nil {
		t.Fatalf("seedOrg: %v", err)
	}
}

func seedUsage(t *testing.T, db *persistence.DB, rec usage.Record) {
	t.Helper()

	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return persistence.NewUsageRecordRepository().Insert(context.Background(), tx, rec)
	}); err != nil {
		t.Fatalf("seedUsage: %v", err)
	}
}

func balance(t *testing.T, db *persistence.DB, orgID string) int64 {
	t.Helper()

	var b int64
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		b, err = persistence.NewWalletRepository().Balance(context.Background(), tx, orgID)
		return err
	}); err != nil {
		t.Fatalf("Balance: %v", err)
	}

	return b
}

func entries(t *testing.T, db *persistence.DB, orgID string) []persistence.WalletEntry {
	t.Helper()

	var list []persistence.WalletEntry
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		list, err = persistence.NewWalletRepository().List(context.Background(), tx, orgID, 0)
		return err
	}); err != nil {
		t.Fatalf("List: %v", err)
	}

	return list
}

// Le premier passage pose seulement la borne : activer la facturation ne
// doit jamais débiter rétroactivement toute l'histoire de l'instance.
func TestDebit_FirstPassOnlyAnchors(t *testing.T) {
	db := testDB(t)
	seedOrg(t, db, persistence.Organization{ID: "atelier", DisplayName: "Atelier"})
	seedUsage(t, db, usage.Record{
		CreatedAt: time.Now().Add(-24 * time.Hour), OrgID: "atelier",
		Component: "agent", Kind: "chat", CostAmount: 1, CostReported: true,
	})

	debiter := billing.New(db, testCfg(), nil, nil)
	if err := debiter.Debit(context.Background()); err != nil {
		t.Fatalf("Debit: %v", err)
	}

	if b := balance(t, db, "atelier"); b != 0 {
		t.Fatalf("solde %d, attendu 0 (aucun débit au premier passage)", b)
	}
}

func TestDebit_ConvertsUsageAndIsIdempotent(t *testing.T) {
	db := testDB(t)
	seedOrg(t, db, persistence.Organization{ID: "atelier", DisplayName: "Atelier"})

	now := time.Now()
	debiter := billing.New(db, testCfg(), nil, nil).WithClock(func() time.Time { return now.Add(-time.Hour) })
	if err := debiter.Debit(context.Background()); err != nil {
		t.Fatalf("Debit (ancrage): %v", err)
	}

	// Consommation postérieure à l'ancrage : 0,25 $ de conversation et
	// 0,10 $ d'images.
	seedUsage(t, db, usage.Record{
		CreatedAt: now.Add(-30 * time.Minute), OrgID: "atelier",
		Component: "agent", Agent: "main", Kind: "chat", CostAmount: 0.25, CostReported: true,
	})
	seedUsage(t, db, usage.Record{
		CreatedAt: now.Add(-20 * time.Minute), OrgID: "atelier",
		Component: "agent", Agent: "imagine", Kind: "image", CostAmount: 0.10, CostReported: true,
	})

	debiter = billing.New(db, testCfg(), nil, nil).WithClock(func() time.Time { return now })
	if err := debiter.Debit(context.Background()); err != nil {
		t.Fatalf("Debit: %v", err)
	}

	if b := balance(t, db, "atelier"); b != -350 {
		t.Fatalf("solde %d, attendu -350 (250 + 100 crédits)", b)
	}

	// Deux libellés distincts : le client doit lire ce qu'il a consommé.
	labels := map[string]bool{}
	for _, entry := range entries(t, db, "atelier") {
		labels[entry.Label] = true
	}
	for _, expected := range []string{"Usage — conversations", "Usage — génération d'images"} {
		if !labels[expected] {
			t.Errorf("mouvement %q absent (obtenus: %v)", expected, labels)
		}
	}

	// Un second passage sur la même période ne redébite rien.
	if err := debiter.Debit(context.Background()); err != nil {
		t.Fatalf("Debit (rejeu): %v", err)
	}
	if b := balance(t, db, "atelier"); b != -350 {
		t.Fatalf("solde %d après rejeu, attendu -350 (débit non idempotent)", b)
	}
}

// La consommation d'une organisation absente des tables SaaS n'est
// facturée à personne : une instance non migrée ne se met pas à débiter.
func TestDebit_IgnoresUnknownOrganization(t *testing.T) {
	db := testDB(t)

	now := time.Now()
	debiter := billing.New(db, testCfg(), nil, nil).WithClock(func() time.Time { return now.Add(-time.Hour) })
	if err := debiter.Debit(context.Background()); err != nil {
		t.Fatalf("Debit (ancrage): %v", err)
	}

	seedUsage(t, db, usage.Record{
		CreatedAt: now.Add(-30 * time.Minute), OrgID: "fantome",
		Component: "agent", Kind: "chat", CostAmount: 1, CostReported: true,
	})

	debiter = billing.New(db, testCfg(), nil, nil).WithClock(func() time.Time { return now })
	if err := debiter.Debit(context.Background()); err != nil {
		t.Fatalf("Debit: %v", err)
	}

	if b := balance(t, db, "fantome"); b != 0 {
		t.Fatalf("solde %d, attendu 0 pour une organisation inconnue", b)
	}
}

func TestApplyAllowances_TopsUpWithoutAccumulating(t *testing.T) {
	db := testDB(t)
	seedOrg(t, db, persistence.Organization{ID: "offerte", DisplayName: "Offerte", Offered: true, MonthlyAllowance: 600})
	seedOrg(t, db, persistence.Organization{ID: "payante", DisplayName: "Payante"})

	january := time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC)
	debiter := billing.New(db, testCfg(), nil, nil).WithClock(func() time.Time { return january })
	if err := debiter.ApplyAllowances(context.Background()); err != nil {
		t.Fatalf("ApplyAllowances: %v", err)
	}

	if b := balance(t, db, "offerte"); b != 600 {
		t.Fatalf("solde offert %d, attendu 600", b)
	}
	if b := balance(t, db, "payante"); b != 0 {
		t.Fatalf("solde payant %d, attendu 0 (aucune allocation)", b)
	}

	// Deuxième passage le même mois : rien de plus.
	if err := debiter.ApplyAllowances(context.Background()); err != nil {
		t.Fatalf("ApplyAllowances (même mois): %v", err)
	}
	if b := balance(t, db, "offerte"); b != 600 {
		t.Fatalf("solde %d, attendu 600 (allocation rejouée dans le mois)", b)
	}

	// Le mois suivant, après consommation : remise à niveau, pas cumul.
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return persistence.NewWalletRepository().Insert(context.Background(), tx, persistence.WalletEntry{
			OrgID: "offerte", Kind: persistence.WalletKindUsage,
			Label: "Usage — conversations", Amount: -400, CreatedAt: january.AddDate(0, 0, 5),
		})
	}); err != nil {
		t.Fatalf("consommation: %v", err)
	}

	february := time.Date(2026, 2, 1, 0, 5, 0, 0, time.UTC)
	debiter = billing.New(db, testCfg(), nil, nil).WithClock(func() time.Time { return february })
	if err := debiter.ApplyAllowances(context.Background()); err != nil {
		t.Fatalf("ApplyAllowances (mois suivant): %v", err)
	}

	if b := balance(t, db, "offerte"); b != 600 {
		t.Fatalf("solde %d, attendu 600 (remise à niveau, jamais cumul)", b)
	}
}
