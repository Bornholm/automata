package introspection

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDigestSender capture les synthèses.
type fakeDigestSender struct {
	mu       sync.Mutex
	messages []string
}

func (s *fakeDigestSender) Notify(_ context.Context, _, _, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, message)
	return nil
}

// La synthèse est anonyme PAR CONSTRUCTION : agrégats seulement. Le test le
// vérifie sur le texte produit — ni nom de membre, ni contenu de plan, ni
// titre de suggestion n'y figure, alors que tous sont en base.
func TestDigest_IsAnonymousByConstruction(t *testing.T) {
	db := openTestDB(t)
	member := seedMember(t, db, false)
	seedExpiredPlans(t, db, member, 3)

	client := &scriptedClient{response: `[]`}
	introspector := newIntrospector(t, db, client)

	message, err := introspector.composeDigest(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("composeDigest: %v", err)
	}

	for _, forbidden := range []string{"cam", "Cam", "CONTENU PRIVÉ", "conv-1", "plan-"} {
		if strings.Contains(message, forbidden) {
			t.Errorf("la synthèse contient %q : elle n'est plus anonyme\n%s", forbidden, message)
		}
	}
	if !strings.Contains(message, "3 action(s) proposée(s) jamais confirmée(s)") {
		t.Errorf("la synthèse devrait compter les plans expirés :\n%s", message)
	}
	if !strings.Contains(message, "email.personal.write") {
		t.Errorf("la synthèse devrait nommer le domaine de friction :\n%s", message)
	}
}

// L'ancrage de la synthèse suit la même règle que la passe : le premier
// tick pose l'horodatage sans rien envoyer, l'échéance passée envoie.
func TestDigest_AnchorsThenSends(t *testing.T) {
	db := openTestDB(t)
	member := seedMember(t, db, false)
	seedExpiredPlans(t, db, member, 2)

	sender := &fakeDigestSender{}
	introspector := newIntrospector(t, db, &scriptedClient{response: `[]`})
	introspector, err := introspector.WithDigest(sender, "")
	if err != nil {
		t.Fatalf("WithDigest: %v", err)
	}

	now := time.Now().UTC()
	if err := introspector.tickDigest(context.Background(), now); err != nil {
		t.Fatalf("premier tick: %v", err)
	}
	if len(sender.messages) != 0 {
		t.Fatal("le premier tick doit ancrer sans envoyer")
	}

	if err := introspector.tickDigest(context.Background(), now.AddDate(0, 2, 0)); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if len(sender.messages) != 1 {
		t.Errorf("%d synthèses envoyées, une attendue", len(sender.messages))
	}
}
