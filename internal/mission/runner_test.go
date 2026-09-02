package mission_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/provider/memory"

	"github.com/bornholm/automata/internal/action"
	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/authorization"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/mission"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
)

var testNow = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

func testConfig() *config.Config {
	return &config.Config{
		Organization: config.Organization{ID: "home", DisplayName: "Maison"},
		Identities: config.Identities{
			Roles: map[string]config.Role{
				"adult": {Permissions: []string{"memory.personal.read", "memory.personal.write", "memory.personal.delete"}},
			},
			Principals: []config.Principal{
				{ID: "alice", Kind: config.PrincipalKindHuman, DisplayName: "Alice", Roles: []string{"adult"}},
			},
		},
		Channels: []config.Channel{
			{Provider: "memory", ChannelID: "chan-alice", Kind: config.ChannelKindPrivate, OrgID: "home"},
		},
	}
}

func testDB(t *testing.T) *persistence.DB {
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

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeAgent capture la requête du réveil et rejoue un scénario. update, non
// nil, simule l'effet d'un appel à update_mission pendant le tour.
type fakeAgent struct {
	req    agent.Request
	result agent.Result
	err    error
	update func(ctx context.Context)
}

func (f *fakeAgent) Execute(ctx context.Context, req agent.Request) (agent.Result, error) {
	f.req = req
	if f.update != nil {
		f.update(ctx)
	}
	return f.result, f.err
}

// fakeAgents fournit toujours le même agent.
type fakeAgents struct{ agent *fakeAgent }

func (f fakeAgents) Get(name string) (agent.Agent, error) {
	if f.agent == nil {
		return nil, errors.New("agent introuvable")
	}
	return f.agent, nil
}

func dueMission(id string) persistence.Mission {
	return persistence.Mission{
		ID: id, OrgID: "home", PrincipalID: "alice",
		ConversationID: "memory:chan-alice", Provider: "memory", ChannelID: "chan-alice",
		AgentID: "main", Title: "Réclamation parking",
		Objective:   "Follow the parking complaint, chase weekly without a reply.",
		Journal:     "2026-08-27: complaint sent, waiting.",
		Status:      persistence.MissionStatusActive,
		NextCheckAt: testNow.Add(-time.Minute),
		LastRunAt:   testNow.Add(-7 * 24 * time.Hour),
		CreatedAt:   testNow.Add(-14 * 24 * time.Hour),
		UpdatedAt:   testNow.Add(-7 * 24 * time.Hour),
	}
}

func insertMission(t *testing.T, db *persistence.DB, m persistence.Mission) {
	t.Helper()

	repo := persistence.NewMissionRepository(nil)
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return repo.Insert(context.Background(), tx, m)
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
}

func readMission(t *testing.T, db *persistence.DB, id string) persistence.Mission {
	t.Helper()

	repo := persistence.NewMissionRepository(nil)
	var m persistence.Mission
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		var found bool
		var err error
		m, found, err = repo.FindByID(context.Background(), tx, id)
		if err == nil && !found {
			t.Fatalf("mission %q introuvable", id)
		}
		return err
	}); err != nil {
		t.Fatalf("FindByID: %v", err)
	}

	return m
}

func newTestRunner(t *testing.T, db *persistence.DB, a *fakeAgent, provider courier.Provider, engine *action.Engine) *mission.Runner {
	t.Helper()

	return mission.NewRunner(testConfig(), db, fakeAgents{agent: a}, mission.SenderMap{"memory": provider}, engine, testLogger()).
		WithClock(func() time.Time { return testNow })
}

func memoryProvider(t *testing.T) *memory.Provider {
	t.Helper()

	provider := memory.NewProvider(memory.WithChannels(courier.NewChannel("chan-alice", courier.ChannelKindDirect, "Alice")))
	t.Cleanup(func() { _ = provider.Close() })

	return provider
}

// Le réveil reçoit le dossier complet — objectif ET journal accumulé : le
// journal est la mémoire de la mission, c'est ce qui remplace l'amnésie des
// tâches planifiées. Et l'identité est celle du créateur, liée à la mission.
func TestRunner_WakeCarriesTheWholeDossier(t *testing.T) {
	db := testDB(t)
	provider := memoryProvider(t)
	insertMission(t, db, dueMission("mis-1"))

	a := &fakeAgent{result: agent.Result{Reply: "Toujours rien reçu."}}
	r := newTestRunner(t, db, a, provider, nil)

	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if !strings.Contains(a.req.Input, "parking complaint") {
		t.Error("l'objectif manque dans l'entrée du réveil")
	}
	if !strings.Contains(a.req.Input, "complaint sent, waiting") {
		t.Error("le journal de bord manque dans l'entrée du réveil")
	}
	if !strings.Contains(a.req.Input, "update_mission") {
		t.Error("la consigne d'appel à update_mission manque")
	}
	if len(a.req.History) != 0 || a.req.Summary != "" {
		t.Error("History et Summary doivent rester vides : le journal est la seule mémoire")
	}

	identity := a.req.Identity
	if identity.Trigger != model.TriggerMission {
		t.Errorf("trigger = %q, attendu %q", identity.Trigger, model.TriggerMission)
	}
	if identity.MissionID != "mis-1" {
		t.Errorf("mission liée = %q : update_mission viserait le mauvais dossier", identity.MissionID)
	}
	if identity.PrincipalID != "alice" {
		t.Errorf("principal = %q, attendu le créateur", identity.PrincipalID)
	}

	// La réponse est délivrée sur le canal de la mission.
	if sent := provider.Sent(); len(sent) != 1 {
		t.Fatalf("messages envoyés = %d, attendu 1", len(sent))
	}
}

// Un tour terminé sans appel à update_mission est replanifié d'office :
// sans ce garde-fou, la mission resterait échue et se réveillerait à chaque
// tick — une boucle infinie de tours de modèle.
func TestRunner_ReschedulesWhenTheTurnLeftNoNote(t *testing.T) {
	db := testDB(t)
	insertMission(t, db, dueMission("mis-1"))

	a := &fakeAgent{result: agent.Result{Reply: "NOTHING_TO_REPORT"}}
	r := newTestRunner(t, db, a, memoryProvider(t), nil)

	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	m := readMission(t, db, "mis-1")
	if !m.NextCheckAt.Equal(testNow.Add(24 * time.Hour)) {
		t.Errorf("échéance = %s, attendu +24h", m.NextCheckAt)
	}
	if !strings.Contains(m.Journal, "left no note") {
		t.Errorf("journal = %q, la replanification d'office doit se voir", m.Journal)
	}
}

// Quand le tour a mis à jour la mission, le garde-fou ne touche à rien : la
// nouvelle échéance est celle choisie par l'agent.
func TestRunner_KeepsTheAgentScheduleWhenNoted(t *testing.T) {
	db := testDB(t)
	insertMission(t, db, dueMission("mis-1"))

	repo := persistence.NewMissionRepository(nil)
	chosen := testNow.Add(7 * 24 * time.Hour)
	a := &fakeAgent{
		result: agent.Result{Reply: "NOTHING_TO_REPORT"},
		update: func(ctx context.Context) {
			err := db.WithTx(ctx, func(tx *sql.Tx) error {
				return repo.UpdateJournal(ctx, tx, "mis-1", "2026-09-03: still waiting.",
					persistence.MissionStatusActive, chosen, 0, testNow)
			})
			if err != nil {
				t.Errorf("UpdateJournal: %v", err)
			}
		},
	}
	provider := memoryProvider(t)
	r := newTestRunner(t, db, a, provider, nil)

	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	m := readMission(t, db, "mis-1")
	if !m.NextCheckAt.Equal(chosen) {
		t.Errorf("échéance = %s, attendu celle posée par l'agent", m.NextCheckAt)
	}
	if strings.Contains(m.Journal, "left no note") {
		t.Error("le garde-fou a replanifié un tour qui avait noté")
	}

	// Réveil de routine sans nouvelle : le marqueur de silence n'envoie rien.
	if sent := provider.Sent(); len(sent) != 0 {
		t.Errorf("messages envoyés = %d, attendu 0 : NOTHING_TO_REPORT doit se taire", len(sent))
	}
}

// Les actions proposées pendant un réveil deviennent un plan en attente de
// confirmation dans la conversation de la mission — jamais jetées (tâches
// planifiées), jamais exécutées d'office.
func TestRunner_ProposedActionsBecomeAPendingPlan(t *testing.T) {
	db := testDB(t)
	provider := memoryProvider(t)
	insertMission(t, db, dueMission("mis-1"))

	cfg := testConfig()
	engine := action.NewEngine(db, authorization.NewAuthorizer(cfg), nil, cfg)

	a := &fakeAgent{result: agent.Result{
		Reply: "Une semaine sans réponse : je peux supprimer le souvenir obsolète.",
		ProposedActions: []delegation.ProposedAction{{
			Summary:            "Supprimer la note obsolète",
			MCPServer:          action.InternalServer,
			ToolName:           action.MemoryForgetTool,
			Arguments:          map[string]any{"id": "m1"},
			RequiredPermission: "memory.personal.delete",
			Scope:              model.ScopePersonal,
			ScopeID:            "alice",
		}},
	}}
	r := newTestRunner(t, db, a, provider, engine)

	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	var status, convID string
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(context.Background(),
			`SELECT status, conversation_id FROM action_plans`).Scan(&status, &convID)
	})
	if err != nil {
		t.Fatalf("aucun plan persisté: %v", err)
	}
	if status != action.StatusAwaitingConfirmation {
		t.Errorf("statut du plan = %q, attendu en attente de confirmation", status)
	}
	if convID != "memory:chan-alice" {
		t.Errorf("conversation du plan = %q, « confirmer » ne la retrouverait pas", convID)
	}

	// Le compte rendu envoyé porte l'invite de confirmation du plan.
	sent := provider.Sent()
	if len(sent) != 1 {
		t.Fatalf("messages envoyés = %d, attendu 1", len(sent))
	}
}

// Un réveil en échec recule l'échéance (backoff) sans toucher au journal,
// et le membre n'est prévenu qu'au franchissement du seuil — une fois, pas
// toutes les heures.
func TestRunner_FailuresBackOffAndAlertOnce(t *testing.T) {
	db := testDB(t)
	provider := memoryProvider(t)
	insertMission(t, db, dueMission("mis-1"))

	a := &fakeAgent{err: errors.New("modèle indisponible")}
	r := newTestRunner(t, db, a, provider, nil)

	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	m := readMission(t, db, "mis-1")
	if m.Attempts != 1 {
		t.Errorf("attempts = %d, attendu 1", m.Attempts)
	}
	if !m.NextCheckAt.Equal(testNow.Add(5 * time.Minute)) {
		t.Errorf("échéance = %s, attendu +5min", m.NextCheckAt)
	}
	if !strings.Contains(m.Journal, "complaint sent") {
		t.Error("le journal ne doit pas être altéré par un échec d'infrastructure")
	}
	if len(provider.Sent()) != 0 {
		t.Error("pas d'alerte avant le seuil")
	}

	// Au seuil : une alerte part, une seule.
	repo := persistence.NewMissionRepository(nil)
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return repo.UpdateJournal(context.Background(), tx, "mis-1", m.Journal,
			persistence.MissionStatusActive, testNow.Add(-time.Minute), 7, testNow)
	}); err != nil {
		t.Fatal(err)
	}

	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	sent := provider.Sent()
	if len(sent) != 1 {
		t.Fatalf("alertes envoyées = %d, attendu 1", len(sent))
	}
	if m := readMission(t, db, "mis-1"); m.Attempts != 8 {
		t.Errorf("attempts = %d, attendu 8", m.Attempts)
	}
}
