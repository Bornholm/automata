package persistence_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/usage"
)

// insertUsage insère rec en échouant le test au moindre problème.
func insertUsage(t *testing.T, db *persistence.DB, rec usage.Record) {
	t.Helper()

	repo := persistence.NewUsageRecordRepository()
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return repo.Insert(context.Background(), tx, rec)
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
}

func aggregateUsage(t *testing.T, db *persistence.DB, from, to time.Time, groupBy []string) []persistence.UsageAggregate {
	t.Helper()

	repo := persistence.NewUsageRecordRepository()
	var aggregates []persistence.UsageAggregate
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		aggregates, err = repo.AggregateUsage(context.Background(), tx, from, to, groupBy)
		return err
	}); err != nil {
		t.Fatalf("AggregateUsage: %v", err)
	}

	return aggregates
}

func TestUsageRecords_AggregateByOrgWithinPeriod(t *testing.T) {
	db := openTestDB(t, testConfig(t))

	base := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

	// Deux appels facturés pour famille-petit, un pour atelier, et un hors
	// période qui ne doit jamais apparaître.
	insertUsage(t, db, usage.Record{
		CreatedAt: base, OrgID: "famille-petit", PrincipalID: "will",
		Component: "agent", Agent: "main", Kind: "chat", Provider: "openrouter", Model: "deepseek/x",
		PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, CachedTokens: 20,
		CostAmount: 0.002, CostCurrency: "USD", CostReported: true,
	})
	insertUsage(t, db, usage.Record{
		CreatedAt: base.Add(time.Hour), OrgID: "famille-petit", PrincipalID: "ivonne",
		Component: "agent", Agent: "main", Kind: "chat", Provider: "openrouter", Model: "deepseek/x",
		PromptTokens: 200, CompletionTokens: 100, TotalTokens: 300,
		CostAmount: 0.004, CostCurrency: "USD", CostReported: true,
	})
	insertUsage(t, db, usage.Record{
		CreatedAt: base, OrgID: "atelier", PrincipalID: "",
		Component: "consolidation", Kind: "chat", Provider: "openrouter", Model: "deepseek/x",
		PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
	})
	insertUsage(t, db, usage.Record{
		CreatedAt: base.AddDate(0, 1, 0), OrgID: "famille-petit", PrincipalID: "will",
		Component: "agent", Agent: "main", Kind: "chat", Provider: "openrouter", Model: "deepseek/x",
		TotalTokens: 999999, CostAmount: 42, CostCurrency: "USD", CostReported: true,
	})

	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	aggregates := aggregateUsage(t, db, from, to, []string{"org"})
	if len(aggregates) != 2 {
		t.Fatalf("attendu 2 lignes (une par org), obtenu %d: %+v", len(aggregates), aggregates)
	}

	// Trié par coût décroissant : famille-petit d'abord.
	first := aggregates[0]
	if first.Keys[0] != "famille-petit" {
		t.Fatalf("première ligne attendue famille-petit, obtenu %q", first.Keys[0])
	}
	if first.Calls != 2 || first.TotalTokens != 450 || first.CachedTokens != 20 {
		t.Errorf("totaux famille-petit inattendus: %+v", first)
	}
	if first.CostAmount != 0.006 || first.Currency != "USD" || first.ReportedCalls != 2 {
		t.Errorf("coûts famille-petit inattendus: %+v", first)
	}

	second := aggregates[1]
	if second.Keys[0] != "atelier" || second.Calls != 1 || second.ReportedCalls != 0 {
		t.Errorf("ligne atelier inattendue: %+v", second)
	}
}

func TestUsageRecords_AggregateByPrincipalAndModel(t *testing.T) {
	db := openTestDB(t, testConfig(t))

	base := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	insertUsage(t, db, usage.Record{
		CreatedAt: base, OrgID: "atelier", PrincipalID: "yann",
		Component: "agent", Agent: "research", Kind: "chat", Provider: "openrouter", Model: "deepseek/x",
		TotalTokens: 100, CostAmount: 0.001, CostCurrency: "USD", CostReported: true,
	})
	insertUsage(t, db, usage.Record{
		CreatedAt: base, OrgID: "atelier", PrincipalID: "yann",
		Component: "agent", Agent: "main", Kind: "chat", Provider: "openrouter", Model: "autre/y",
		TotalTokens: 40,
	})

	aggregates := aggregateUsage(t, db,
		base.AddDate(0, 0, -1), base.AddDate(0, 0, 1), []string{"principal", "model"})
	if len(aggregates) != 2 {
		t.Fatalf("attendu 2 lignes, obtenu %d", len(aggregates))
	}
	for _, agg := range aggregates {
		if agg.Keys[0] != "yann" {
			t.Errorf("principal attendu yann, obtenu %q", agg.Keys[0])
		}
	}
}

func TestUsageRecords_AggregateRejectsUnknownDimension(t *testing.T) {
	db := openTestDB(t, testConfig(t))

	repo := persistence.NewUsageRecordRepository()
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		_, err := repo.AggregateUsage(context.Background(), tx, time.Now().Add(-time.Hour), time.Now(), []string{"org; DROP TABLE usage_records"})
		return err
	})
	if err == nil {
		t.Fatal("une dimension hors liste blanche doit être rejetée")
	}
}
