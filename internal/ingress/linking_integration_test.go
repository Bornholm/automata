package ingress_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/go-courier"

	"github.com/bornholm/automata/internal/ingress"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/weblink"
)

// seedTenant crée une organisation, un membre pré-créé et un jeton de
// liaison en attente. Retourne le jeton en clair.
func seedTenant(t *testing.T, db *persistence.DB, kind, memberID string) string {
	t.Helper()

	clear, hash, _, err := weblink.NewLinkToken()
	if err != nil {
		t.Fatalf("NewLinkToken: %v", err)
	}

	now := time.Now()
	err = db.WithTx(context.Background(), func(tx *sql.Tx) error {
		if err := persistence.NewOrganizationRepository().Insert(context.Background(), tx, persistence.Organization{
			ID: "atelier", DisplayName: "Atelier Nord", CreatedAt: now, UpdatedAt: now,
		}, true); err != nil {
			return err
		}

		if memberID != "" {
			if err := persistence.NewMemberRepository().Insert(context.Background(), tx, persistence.Member{
				ID: memberID, OrgID: "atelier", DisplayName: "Camille Roux",
				Role: persistence.MemberRoleMember, CreatedAt: now, UpdatedAt: now,
			}, true); err != nil {
				return err
			}
		}

		return persistence.NewLinkTokenRepository().Insert(context.Background(), tx, persistence.LinkToken{
			ID: "tok-" + kind, Kind: kind, MemberID: memberID, OrgID: "atelier",
			TokenHash: hash, Status: persistence.LinkTokenStatusPending,
			ExpiresAt: now.Add(7 * 24 * time.Hour), CreatedAt: now,
		})
	})
	if err != nil {
		t.Fatalf("seedTenant: %v", err)
	}

	return clear
}

// deliverFromUnknown livre un message d'un expéditeur inconnu du pipeline.
func deliverFromUnknown(t *testing.T, provider *readyProvider, channelID, userID, text string) {
	t.Helper()

	msg := courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannelRef(courier.ChannelID(channelID)),
		courier.NewUser(courier.UserID(userID), "Inconnu"),
		courier.WithMessageMainPart(text),
	)
	if err := provider.Deliver(context.Background(), msg); err != nil {
		t.Fatalf("provider.Deliver: %v", err)
	}
}

// lastSentText retourne le texte de la dernière réponse envoyée.
func lastSentText(t *testing.T, provider *readyProvider) string {
	t.Helper()

	sent := provider.Sent()
	if len(sent) == 0 {
		return ""
	}
	content, err := courier.GetMessageMainContent(context.Background(), sent[len(sent)-1])
	if err != nil {
		t.Fatalf("lecture de la réponse: %v", err)
	}
	return content
}

func TestPipeline_PersonalTokenLinksMemberAndChannel(t *testing.T) {
	handler := &countingHandler{reply: "ignoré"}
	pipeline, provider, db := newLinkingPipeline(t, handler)
	stop := runPipeline(t, pipeline)
	defer stop()
	provider.waitReady(t)

	clear := seedTenant(t, db, persistence.LinkTokenKindPersonal, "camille")

	// La personne envoie son jeton depuis sa conversation privée : sur les
	// messageries directes, l'identifiant de canal est celui de l'expéditeur.
	deliverFromUnknown(t, provider, "camille-ext", "camille-ext", "bonjour, voici mon code "+clear)

	if !waitUntil(t, 2*time.Second, func() bool { return len(provider.Sent()) > 0 }) {
		t.Fatal("aucune réponse de bienvenue envoyée")
	}

	if reply := lastSentText(t, provider); !strings.Contains(reply, "Camille Roux") || !strings.Contains(reply, "rattaché") {
		t.Errorf("réponse de bienvenue inattendue: %q", reply)
	}

	// Le message porteur du jeton ne déclenche pas de tour d'agent : il sert
	// uniquement au rattachement.
	if calls := handler.Calls(); calls != 0 {
		t.Errorf("le handler a été appelé %d fois, attendu 0", calls)
	}

	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		member, found, err := persistence.NewMemberRepository().FindByExternalUser(context.Background(), tx, testProviderName, "camille-ext")
		if err != nil {
			return err
		}
		if !found || member.ID != "camille" {
			t.Fatalf("membre non rattaché à l'identité de messagerie (found=%v)", found)
		}

		binding, found, err := persistence.NewChannelBindingRepository().Find(context.Background(), tx, testProviderName, "camille-ext")
		if err != nil {
			return err
		}
		if !found || binding.OrgID != "atelier" || binding.MemberID != "camille" {
			t.Errorf("liaison de canal privé inattendue: %+v (found=%v)", binding, found)
		}

		token, _, err := persistence.NewLinkTokenRepository().LatestByMember(context.Background(), tx, "camille")
		if err != nil {
			return err
		}
		if token.Status != persistence.LinkTokenStatusUsed {
			t.Errorf("jeton dans l'état %q, attendu %q", token.Status, persistence.LinkTokenStatusUsed)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("vérification en base: %v", err)
	}
}

func TestPipeline_GroupTokenBindsChannelToOrg(t *testing.T) {
	handler := &countingHandler{reply: "ignoré"}
	pipeline, provider, db := newLinkingPipeline(t, handler)
	stop := runPipeline(t, pipeline)
	defer stop()
	provider.waitReady(t)

	clear := seedTenant(t, db, persistence.LinkTokenKindGroup, "")

	deliverFromUnknown(t, provider, "atelier-group", "quelquun-ext", clear)

	if !waitUntil(t, 2*time.Second, func() bool { return len(provider.Sent()) > 0 }) {
		t.Fatal("aucune réponse de liaison envoyée")
	}
	if reply := lastSentText(t, provider); !strings.Contains(reply, "Atelier Nord") {
		t.Errorf("réponse de liaison inattendue: %q", reply)
	}

	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		binding, found, err := persistence.NewChannelBindingRepository().Find(context.Background(), tx, testProviderName, "atelier-group")
		if err != nil {
			return err
		}
		if !found || binding.OrgID != "atelier" || binding.Scope != "group" {
			t.Errorf("liaison de groupe inattendue: %+v (found=%v)", binding, found)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("vérification en base: %v", err)
	}
}

// Un jeton révoqué ne lie rien : la réponse reste volontairement vague, un
// message précis renseignerait un inconnu sur la validité d'un code.
func TestPipeline_RevokedTokenLinksNothing(t *testing.T) {
	handler := &countingHandler{reply: "ignoré"}
	pipeline, provider, db := newLinkingPipeline(t, handler)
	stop := runPipeline(t, pipeline)
	defer stop()
	provider.waitReady(t)

	clear := seedTenant(t, db, persistence.LinkTokenKindPersonal, "camille")

	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return persistence.NewLinkTokenRepository().RevokePendingByMember(context.Background(), tx, "camille")
	}); err != nil {
		t.Fatalf("révocation: %v", err)
	}

	deliverFromUnknown(t, provider, "camille-ext", "camille-ext", clear)

	if !waitUntil(t, 2*time.Second, func() bool { return len(provider.Sent()) > 0 }) {
		t.Fatal("aucune réponse envoyée")
	}
	if reply := lastSentText(t, provider); !strings.Contains(reply, "n'est plus valide") {
		t.Errorf("réponse inattendue pour un jeton révoqué: %q", reply)
	}

	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		_, found, err := persistence.NewMemberRepository().FindByExternalUser(context.Background(), tx, testProviderName, "camille-ext")
		if err != nil {
			return err
		}
		if found {
			t.Error("un jeton révoqué ne doit rattacher personne")
		}
		return nil
	}); err != nil {
		t.Fatalf("vérification en base: %v", err)
	}
}

// newLinkingPipeline construit un pipeline avec la liaison par jeton
// activée, et retourne aussi la base pour les vérifications.
func newLinkingPipeline(t *testing.T, handler ingress.Handler) (*ingress.Pipeline, *readyProvider, *persistence.DB) {
	t.Helper()

	pipeline, provider, db := newTestPipelineWithDB(t, handler)

	return pipeline.WithLinking(true), provider, db
}

// TestPipeline_PersonalTokenBindsDirectRoom : Rocket.Chat, Discord et le
// courriel donnent à un tête-à-tête un identifiant de salon distinct de
// celui de la personne. Se fier à leur égalité laissait la conversation
// inconnue du pipeline : le rattachement réussissait, puis le message
// suivant était ignoré.
func TestPipeline_PersonalTokenBindsDirectRoom(t *testing.T) {
	handler := &countingHandler{reply: "ignoré"}
	pipeline, provider, db := newLinkingPipeline(t, handler)
	stop := runPipeline(t, pipeline)
	defer stop()
	provider.waitReady(t)

	clear := seedTenant(t, db, persistence.LinkTokenKindPersonal, "camille")

	msg := courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannel("salon-prive-42", courier.ChannelKindDirect, "Camille Roux"),
		courier.NewUser("camille-ext", "Camille Roux"),
		courier.WithMessageMainPart("voici mon code "+clear),
	)
	if err := provider.Deliver(context.Background(), msg); err != nil {
		t.Fatalf("provider.Deliver: %v", err)
	}

	if !waitUntil(t, 2*time.Second, func() bool { return len(provider.Sent()) > 0 }) {
		t.Fatal("aucune réponse de bienvenue envoyée")
	}

	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		binding, found, err := persistence.NewChannelBindingRepository().Find(context.Background(), tx, testProviderName, "salon-prive-42")
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("le salon privé n'a pas été rattaché : le message suivant serait ignoré")
		}
		if binding.MemberID != "camille" || binding.OrgID != "atelier" {
			t.Errorf("liaison inattendue: %+v", binding)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("vérification en base: %v", err)
	}
}
