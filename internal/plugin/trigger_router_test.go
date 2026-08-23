package plugin

import (
	"context"
	"database/sql"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/bornholm/go-courier"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// fakeTriggerRunner enregistre les exécutions demandées.
type fakeTriggerRunner struct {
	mu    sync.Mutex
	calls []fakeTriggerCall
	reply string
}

type fakeTriggerCall struct {
	Plugin string
	Input  string
	ID     model.ExecutionIdentity
}

func (f *fakeTriggerRunner) RunTrigger(_ context.Context, pluginName string, identity model.ExecutionIdentity, _ model.Conversation, input string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeTriggerCall{Plugin: pluginName, Input: input, ID: identity})
	return f.reply, nil
}

func (f *fakeTriggerRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeSender capture les messages sortants.
type fakeSender struct {
	mu   sync.Mutex
	sent []courier.Message
}

func (f *fakeSender) Listen(context.Context) (chan courier.Message, error) { return nil, nil }
func (f *fakeSender) Send(_ context.Context, msg courier.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, msg)
	return nil
}

type fakeSenderSet map[string]courier.Provider

func (s fakeSenderSet) Get(name string) (courier.Provider, bool) {
	p, ok := s[name]
	return p, ok
}

func linkMember(t *testing.T, db *persistence.DB, memberID, provider, externalUserID string) {
	t.Helper()
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		repo := persistence.NewMemberRepository()
		m, _, err := repo.FindByID(context.Background(), tx, memberID)
		if err != nil {
			return err
		}
		m.Provider, m.ExternalUserID, m.LinkedAt = provider, externalUserID, time.Now()
		return repo.Update(context.Background(), tx, m)
	})
	if err != nil {
		t.Fatalf("rattachement du membre: %v", err)
	}
}

func newTestRouter(t *testing.T, cfg config.Plugins) (*TriggerRouter, *fakeTriggerRunner, *fakeSender, *persistence.DB) {
	t.Helper()

	manager, db := newTestManager(t, cfg)
	seedOrgAndMember(t, db, "atelier", "cam")
	linkMember(t, db, "cam", "rest", "cam-ext")
	activateEcho(t, db, "atelier")

	runner := &fakeTriggerRunner{reply: "Résumé du déclencheur."}
	sender := &fakeSender{}
	router := NewTriggerRouter(manager, db, runner, fakeSenderSet{"rest": sender}, nil)

	return router, runner, sender, db
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition jamais atteinte")
}

func testEvent(id string) *proto.TriggerEvent {
	return &proto.TriggerEvent{
		Id:         id,
		OrgId:      "atelier",
		MemberId:   "cam",
		Kind:       "test.event",
		AgentInput: "Handle the incoming event.",
	}
}

// Le chemin nominal : événement valide → exécution avec l'identité du
// membre → réponse envoyée sur son canal privé.
func TestTriggerRouter_ExecutesAndReplies(t *testing.T) {
	router, runner, sender, _ := newTestRouter(t, config.Plugins{})

	router.handle(context.Background(), "echo", testEvent("evt-1"))

	waitFor(t, func() bool { return runner.count() == 1 })
	waitFor(t, func() bool { sender.mu.Lock(); defer sender.mu.Unlock(); return len(sender.sent) == 1 })

	call := runner.calls[0]
	if call.Plugin != "echo" || call.Input != "Handle the incoming event." {
		t.Errorf("exécution inattendue: %+v", call)
	}
	if call.ID.Trigger != model.TriggerPlugin || call.ID.PrincipalID != "cam" || call.ID.OrgID != "atelier" {
		t.Errorf("identité inattendue: %+v", call.ID)
	}
	if call.ID.Scope != model.ScopePersonal || call.ID.ChannelID != "cam-ext" {
		t.Errorf("portée/canal inattendus: %+v", call.ID)
	}
}

// Un identifiant déjà vu ne déclenche qu'une exécution : les plugins
// ré-émettent après reconnexion, l'hôte dédoublonne.
func TestTriggerRouter_DeduplicatesByID(t *testing.T) {
	router, runner, _, _ := newTestRouter(t, config.Plugins{})

	router.handle(context.Background(), "echo", testEvent("evt-dup"))
	router.handle(context.Background(), "echo", testEvent("evt-dup"))

	waitFor(t, func() bool { return runner.count() == 1 })
	time.Sleep(50 * time.Millisecond)
	if runner.count() != 1 {
		t.Fatalf("%d exécution(s) pour un même id, attendu 1", runner.count())
	}
}

// Une rafale est bornée par le plafond par minute — abandon compté, jamais
// de file illimitée.
func TestTriggerRouter_RateLimitsBursts(t *testing.T) {
	router, runner, _, _ := newTestRouter(t, config.Plugins{
		Triggers: config.PluginTriggers{MaxPerMinute: 3, MaxConcurrent: 8},
	})

	for i := 0; i < 20; i++ {
		router.handle(context.Background(), "echo", testEvent("evt-burst-"+string(rune('a'+i))))
	}

	waitFor(t, func() bool { return runner.count() == 3 })
	time.Sleep(80 * time.Millisecond)
	if runner.count() != 3 {
		t.Fatalf("%d exécution(s), attendu 3 (plafond)", runner.count())
	}
}

// L'événement désigne, il ne décide pas : organisation inactive ou membre
// étranger sont refusés côté hôte.
func TestTriggerRouter_RejectsUndesignatedTargets(t *testing.T) {
	router, runner, _, db := newTestRouter(t, config.Plugins{})
	seedOrgAndMember(t, db, "autre", "zoe")
	linkMember(t, db, "zoe", "rest", "zoe-ext")

	// Membre d'une autre organisation que celle de l'événement.
	evt := testEvent("evt-foreign")
	evt.MemberId = "zoe"
	router.handle(context.Background(), "echo", evt)

	// Organisation sans activation.
	evt2 := testEvent("evt-inactive")
	evt2.OrgId, evt2.MemberId = "autre", "zoe"
	router.handle(context.Background(), "echo", evt2)

	time.Sleep(80 * time.Millisecond)
	if runner.count() != 0 {
		t.Fatalf("%d exécution(s) pour des cibles non désignées, attendu 0", runner.count())
	}
}

// Un texte de livraison verbatim part tel quel, sans tour de modèle. Un
// pense-bête que la personne a écrit elle-même ne doit être ni reformulé
// ni payé au prix d'un appel de LLM.
func TestTriggerRouter_DeliversVerbatimWithoutRunningTheAgent(t *testing.T) {
	router, runner, sender, _ := newTestRouter(t, config.Plugins{})

	evt := testEvent("evt-verbatim")
	evt.DeliverText = "Sortir les poubelles"
	router.handle(context.Background(), "echo", evt)

	waitFor(t, func() bool { sender.mu.Lock(); defer sender.mu.Unlock(); return len(sender.sent) == 1 })
	time.Sleep(50 * time.Millisecond)

	if runner.count() != 0 {
		t.Errorf("%d exécution(s) de sous-agent pour une livraison verbatim, attendu 0", runner.count())
	}

	sender.mu.Lock()
	defer sender.mu.Unlock()
	if got := messageText(t, sender.sent[0]); got != "Sortir les poubelles" {
		t.Errorf("texte livré %q, attendu le texte verbatim", got)
	}
}

// Le texte verbatim ne court-circuite AUCUN garde-fou : il passe par le
// même dédoublonnage et les mêmes vérifications d'appartenance.
func TestTriggerRouter_VerbatimKeepsTheGuards(t *testing.T) {
	router, _, sender, _ := newTestRouter(t, config.Plugins{})

	evt := testEvent("evt-verbatim-dup")
	evt.DeliverText = "Sortir les poubelles"
	router.handle(context.Background(), "echo", evt)
	router.handle(context.Background(), "echo", evt)

	waitFor(t, func() bool { sender.mu.Lock(); defer sender.mu.Unlock(); return len(sender.sent) == 1 })
	time.Sleep(50 * time.Millisecond)

	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.sent) != 1 {
		t.Fatalf("%d envoi(s) pour un même id, attendu 1", len(sender.sent))
	}
}

// messageText lit la première partie d'un message sortant.
func messageText(t *testing.T, msg courier.Message) string {
	t.Helper()

	parts := msg.Parts()
	if len(parts) == 0 {
		t.Fatal("message sans partie")
	}
	reader, err := parts[0].Reader(context.Background())
	if err != nil {
		t.Fatalf("lecture de la partie: %v", err)
	}
	defer reader.Close()

	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("lecture de la partie: %v", err)
	}
	return string(raw)
}
