package conversation_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/audio"
	"github.com/bornholm/automata/internal/conversation"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
)

// fakeProfileLinks fournit un lien de recharge fixe.
type fakeProfileLinks struct {
	url string
}

func (f fakeProfileLinks) GenerateProfileLink(_ context.Context, _, _ string) (string, bool, error) {
	if f.url == "" {
		return "", false, nil
	}
	return f.url, true, nil
}

// seedWallet crédite (ou débite) le portefeuille d'une organisation.
func seedWallet(t *testing.T, db *persistence.DB, orgID string, amounts ...int64) {
	t.Helper()

	wallet := persistence.NewWalletRepository()
	for i, amount := range amounts {
		if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
			return wallet.Insert(context.Background(), tx, persistence.WalletEntry{
				OrgID: orgID, Kind: persistence.WalletKindWelcome, Label: "test",
				Amount: amount, CreatedAt: time.Now().Add(time.Duration(i) * time.Second),
			})
		}); err != nil {
			t.Fatalf("seedWallet: %v", err)
		}
	}
}

func pauseIdentity() model.ExecutionIdentity {
	return model.ExecutionIdentity{
		PrincipalID:    model.PrincipalID("alice"),
		OrgID:          model.OrgID("home"),
		ConversationID: model.ConversationID("conv-a"),
	}
}

// Solde épuisé : la conversation reçoit une explication et un lien de
// recharge, sans jamais consulter le modèle — poursuivre creuserait une
// dette que le client n'a pas acceptée.
func TestHandler_PausedWhenBalanceIsExhausted(t *testing.T) {
	db := openTestDB(t)

	a := &recordingAgent{}
	h := conversation.NewHandler(db, a, nil, 0, audio.Config{}, nil, false, nil).
		WithBilling(fakeProfileLinks{url: "https://automata.test/p/abc.def"})

	seedWallet(t, db, "home", 500, -500)

	reply, _, err := h.Handle(context.Background(), pauseIdentity(),
		testConversation(model.ConversationID("conv-a"), "chan-a"), testMessage("alice", "bonjour"))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if !strings.Contains(reply, "crédits") || !strings.Contains(reply, "pause") {
		t.Errorf("réponse de pause inattendue: %q", reply)
	}
	if !strings.Contains(reply, "https://automata.test/p/abc.def") {
		t.Error("la réponse doit porter le lien de recharge")
	}
	if len(a.requests) != 0 {
		t.Errorf("l'agent a été appelé %d fois, attendu 0 (aucun appel LLM en pause)", len(a.requests))
	}

	// Message suivant dans la même conversation : silencieux, pour ne pas
	// répéter la même explication à chaque phrase.
	reply2, _, err := h.Handle(context.Background(), pauseIdentity(),
		testConversation(model.ConversationID("conv-a"), "chan-a"), testMessage("alice", "et là ?"))
	if err != nil {
		t.Fatalf("Handle (2): %v", err)
	}
	if reply2 != "" {
		t.Errorf("seconde réponse %q, attendue vide (anti-répétition)", reply2)
	}
	if len(a.requests) != 0 {
		t.Errorf("l'agent a été appelé %d fois, attendu 0", len(a.requests))
	}
}

func TestHandler_NotPausedWhenBalanceRemains(t *testing.T) {
	db := openTestDB(t)

	a := &recordingAgent{}
	h := conversation.NewHandler(db, a, nil, 0, audio.Config{}, nil, false, nil).
		WithBilling(fakeProfileLinks{})

	seedWallet(t, db, "home", 500)

	if _, _, err := h.Handle(context.Background(), pauseIdentity(),
		testConversation(model.ConversationID("conv-a"), "chan-a"), testMessage("alice", "bonjour")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(a.requests) != 1 {
		t.Errorf("l'agent a été appelé %d fois, attendu 1", len(a.requests))
	}
}

// Une organisation sans portefeuille (instance non facturée, tenant encore
// purement configuré) n'est jamais mise en pause : la facturation ne
// s'invite pas là où elle n'a pas été mise en place.
func TestHandler_NeverPausesOrganizationWithoutWallet(t *testing.T) {
	db := openTestDB(t)

	a := &recordingAgent{}
	h := conversation.NewHandler(db, a, nil, 0, audio.Config{}, nil, false, nil).
		WithBilling(fakeProfileLinks{})

	if _, _, err := h.Handle(context.Background(), pauseIdentity(),
		testConversation(model.ConversationID("conv-a"), "chan-a"), testMessage("alice", "bonjour")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(a.requests) != 1 {
		t.Errorf("l'agent a été appelé %d fois, attendu 1 (aucun portefeuille = aucune pause)", len(a.requests))
	}
}

// Une organisation en mode gratuit sans limite n'est JAMAIS mise en pause,
// même le portefeuille à zéro : rien ne la débite, un solde nul ne dit donc
// rien de son droit à être servie.
func TestHandler_NeverPausedWhenOrganizationIsUnlimited(t *testing.T) {
	db := openTestDB(t)

	now := time.Now()
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return persistence.NewOrganizationRepository().Insert(context.Background(), tx, persistence.Organization{
			ID: "home", DisplayName: "Maison", Unlimited: true, CreatedAt: now, UpdatedAt: now,
		}, true)
	}); err != nil {
		t.Fatalf("insertion de l'organisation: %v", err)
	}

	a := &recordingAgent{}
	h := conversation.NewHandler(db, a, nil, 0, audio.Config{}, nil, false, nil).
		WithBilling(fakeProfileLinks{url: "https://automata.test/p/abc.def"})

	// Même portefeuille épuisé que le test de mise en pause ci-dessus.
	seedWallet(t, db, "home", 500, -500)

	reply, _, err := h.Handle(context.Background(), pauseIdentity(),
		testConversation(model.ConversationID("conv-a"), "chan-a"), testMessage("alice", "bonjour"))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if strings.Contains(reply, "pause") {
		t.Errorf("réponse %q : une organisation sans limite ne doit jamais être mise en pause", reply)
	}
	if len(a.requests) != 1 {
		t.Errorf("l'agent a été appelé %d fois, attendu 1", len(a.requests))
	}
}
