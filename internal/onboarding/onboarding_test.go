package onboarding_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/memory"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/onboarding"
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

// recordingMemory retient ce que la visite a appris.
type recordingMemory struct {
	facts []string
}

func (m *recordingMemory) Remember(_ context.Context, mem memory.NewMemory) (memory.Memory, error) {
	m.facts = append(m.facts, mem.Content)
	if mem.Origin != onboarding.Origin {
		return memory.Memory{}, nil
	}
	return memory.Memory{}, nil
}

// seedMember crée une organisation et un membre dans l'état voulu.
func seedMember(t *testing.T, db *persistence.DB, state string) model.ExecutionIdentity {
	t.Helper()

	now := time.Now().UTC()
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		orgs := persistence.NewOrganizationRepository()
		if err := orgs.Insert(context.Background(), tx, persistence.Organization{
			ID: "atelier", DisplayName: "Atelier", CreatedAt: now, UpdatedAt: now,
		}, true); err != nil {
			return err
		}
		members := persistence.NewMemberRepository()
		if err := members.Insert(context.Background(), tx, persistence.Member{
			ID: "cam", OrgID: "atelier", DisplayName: "Cam", Role: "member",
			OnboardingState: state, CreatedAt: now, UpdatedAt: now,
		}, true); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("semis: %v", err)
	}

	return model.ExecutionIdentity{
		OrgID:          "atelier",
		PrincipalID:    "cam",
		Scope:          model.ScopePersonal,
		ScopeID:        "cam",
		ConversationID: "conv-1",
	}
}

func memberState(t *testing.T, db *persistence.DB) string {
	t.Helper()

	var member persistence.Member
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		member, _, err = persistence.NewMemberRepository().FindByID(context.Background(), tx, "cam")
		return err
	})
	if err != nil {
		t.Fatalf("relecture du membre: %v", err)
	}

	return member.OnboardingState
}

// Le parcours nominal : quatre questions, quatre souvenirs, et la main
// rendue à l'assistant à la fin.
func TestVisit_FullRunRemembersEveryAnswer(t *testing.T) {
	db := openTestDB(t)
	identity := seedMember(t, db, onboarding.StateOffered)
	mem := &recordingMemory{}
	service := onboarding.New(db, mem, nil)

	reply, handled, err := service.Handle(context.Background(), identity, "oui")
	if err != nil || !handled {
		t.Fatalf("acceptation: handled=%v err=%v", handled, err)
	}
	if !strings.Contains(reply, "appelle") {
		t.Errorf("première question inattendue: %q", reply)
	}

	answers := []string{"Cam", "Europe/Paris, plutôt le matin", "je monte une AMAP", "brèves"}
	for i, answer := range answers {
		reply, handled, err = service.Handle(context.Background(), identity, answer)
		if err != nil {
			t.Fatalf("réponse %d: %v", i+1, err)
		}
		if !handled {
			t.Fatalf("réponse %d : la visite devrait continuer", i+1)
		}
	}

	if state := memberState(t, db); state != onboarding.StateDone {
		t.Errorf("état final = %q, attendu %q", state, onboarding.StateDone)
	}
	if len(mem.facts) != len(answers) {
		t.Fatalf("%d souvenirs enregistrés, attendu %d : %q", len(mem.facts), len(answers), mem.facts)
	}
	if !strings.Contains(mem.facts[0], "Cam") {
		t.Errorf("le premier souvenir devrait porter la réponse, reçu %q", mem.facts[0])
	}

	// La visite terminée ne se rejoue jamais.
	if _, handled, _ := service.Handle(context.Background(), identity, "bonjour"); handled {
		t.Error("une visite terminée ne doit plus intercepter les messages")
	}
}

// Refuser l'invitation rend la main IMMÉDIATEMENT, et le message part à
// l'assistant : quelqu'un qui pose sa question au lieu de dire oui attend
// une réponse, pas un questionnaire.
func TestVisit_DeclinedOfferHandsBackTheMessage(t *testing.T) {
	db := openTestDB(t)
	identity := seedMember(t, db, onboarding.StateOffered)
	service := onboarding.New(db, &recordingMemory{}, nil)

	_, handled, err := service.Handle(context.Background(), identity,
		"peux-tu me rappeler le rendez-vous de jeudi ?")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if handled {
		t.Fatal("le message doit être rendu à l'assistant, pas absorbé par la visite")
	}
	if state := memberState(t, db); state != onboarding.StateSkipped {
		t.Errorf("état = %q, attendu %q", state, onboarding.StateSkipped)
	}
}

// « passe » interrompt la visite en cours, avec un mot, et sans y revenir.
func TestVisit_QuitWordEndsItForGood(t *testing.T) {
	db := openTestDB(t)
	identity := seedMember(t, db, onboarding.StateOffered)
	service := onboarding.New(db, &recordingMemory{}, nil)

	if _, _, err := service.Handle(context.Background(), identity, "oui"); err != nil {
		t.Fatalf("acceptation: %v", err)
	}

	reply, handled, err := service.Handle(context.Background(), identity, "passe")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !handled || reply == "" {
		t.Error("la sortie explicite mérite un mot, pas un silence")
	}
	if state := memberState(t, db); state != onboarding.StateSkipped {
		t.Errorf("état = %q, attendu %q", state, onboarding.StateSkipped)
	}

	if _, handled, _ := service.Handle(context.Background(), identity, "bonjour"); handled {
		t.Error("une visite écartée ne doit plus revenir")
	}
}

// Une vraie question posée au milieu de la visite sort de la visite : la
// personne a besoin d'autre chose, et son message ne doit pas finir rangé
// comme une réponse absurde.
func TestVisit_RealQuestionMidWayHandsBack(t *testing.T) {
	db := openTestDB(t)
	identity := seedMember(t, db, onboarding.StateOffered)
	mem := &recordingMemory{}
	service := onboarding.New(db, mem, nil)

	if _, _, err := service.Handle(context.Background(), identity, "oui"); err != nil {
		t.Fatalf("acceptation: %v", err)
	}

	_, handled, err := service.Handle(context.Background(), identity,
		"attends, est-ce que tu peux d'abord me dire ce que tu sais faire exactement ?")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if handled {
		t.Fatal("la question doit repartir vers l'assistant")
	}
	if len(mem.facts) != 0 {
		t.Errorf("aucun souvenir ne devrait être enregistré, reçu %q", mem.facts)
	}
}

// Une réponse courte qui contient un point d'interrogation reste une
// réponse : l'heuristique ne doit pas écourter la visite au premier « ? ».
func TestVisit_ShortAnswerWithQuestionMarkIsStillAnAnswer(t *testing.T) {
	db := openTestDB(t)
	identity := seedMember(t, db, onboarding.StateOffered)
	mem := &recordingMemory{}
	service := onboarding.New(db, mem, nil)

	if _, _, err := service.Handle(context.Background(), identity, "oui"); err != nil {
		t.Fatalf("acceptation: %v", err)
	}

	_, handled, err := service.Handle(context.Background(), identity, "Cam, ça ira ?")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !handled {
		t.Fatal("une réponse courte reste une réponse")
	}
	if len(mem.facts) != 1 {
		t.Errorf("la réponse devrait être retenue, souvenirs = %q", mem.facts)
	}
}

// Sans mémoire câblée, la visite se déroule quand même : dégradée, mais
// jamais bloquante.
func TestVisit_RunsWithoutMemory(t *testing.T) {
	db := openTestDB(t)
	identity := seedMember(t, db, onboarding.StateOffered)
	service := onboarding.New(db, nil, nil)

	if _, handled, err := service.Handle(context.Background(), identity, "oui"); err != nil || !handled {
		t.Fatalf("acceptation: handled=%v err=%v", handled, err)
	}
	if _, handled, err := service.Handle(context.Background(), identity, "Cam"); err != nil || !handled {
		t.Fatalf("réponse: handled=%v err=%v", handled, err)
	}
}

// Un membre jamais invité n'est jamais intercepté : la visite ne s'invite
// pas dans une conversation existante.
func TestVisit_NeverOfferedStaysOutOfTheWay(t *testing.T) {
	db := openTestDB(t)
	identity := seedMember(t, db, onboarding.StateNone)
	service := onboarding.New(db, &recordingMemory{}, nil)

	if _, handled, err := service.Handle(context.Background(), identity, "bonjour"); err != nil || handled {
		t.Fatalf("handled=%v err=%v : aucune visite ne devait démarrer", handled, err)
	}
}
