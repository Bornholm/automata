package plugin

import (
	"context"
	"testing"

	"github.com/bornholm/automata/internal/config"
	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// La revendication du magasin est un réglage du MEMBRE, lu dans sa
// configuration. L'hôte n'interprète que cette clé et reste ignorant du
// reste du document, qui appartient au plugin.
func TestEventStoreClaimed(t *testing.T) {
	claiming := []string{
		`{"automata_event_store":true}`,
		`{"caldav_url":"https://exemple.fr/dav","automata_event_store":true}`,
	}
	for _, cfg := range claiming {
		if !eventStoreClaimed(cfg) {
			t.Errorf("%s devrait revendiquer le magasin", cfg)
		}
	}

	quiet := []string{
		// Absente : le comportement historique, les rappels restent en base.
		`{"caldav_url":"https://exemple.fr/dav"}`,
		// Présente mais fausse : un agenda branché puis débranché.
		`{"automata_event_store":false}`,
		// D'un autre type : un plugin fautif ne prend pas la main par
		// accident.
		`{"automata_event_store":"oui"}`,
		`{"automata_event_store":1}`,
		// Documents illisibles ou vides : l'hôte retombe sur sa table,
		// qui est le choix sûr.
		`pas du json`,
		``,
	}
	for _, cfg := range quiet {
		if eventStoreClaimed(cfg) {
			t.Errorf("%q ne devrait pas revendiquer le magasin", cfg)
		}
	}
}

// La revendication ne suffit pas : sans le drapeau du descripteur, le
// plugin n'implémente pas les RPC du magasin. Les appeler quand même
// laisserait les rappels du membre dans le vide.
func TestEventStoreFor_RequiresTheDescriptorFlag(t *testing.T) {
	manager, db := newTestManager(t, config.Plugins{})
	seedOrgAndMember(t, db, "atelier", "cam")
	activateEcho(t, db, "atelier")

	ctx := context.Background()
	scoped := manager.hostService.scopedTo("echo")
	if _, err := scoped.SaveConfig(ctx, &proto.SaveConfigRequest{
		OrgId: "atelier", MemberId: "cam", ConfigJson: `{"automata_event_store":true}`,
	}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	// Le plugin echo ne déclare pas provides_event_store.
	if manager.providesEventStore("echo") {
		t.Fatal("le plugin de test ne devrait pas déclarer de magasin")
	}
	if name, ok := manager.EventStoreFor(ctx, db, "atelier", "cam"); ok {
		t.Errorf("magasin %q résolu alors que le descripteur ne l'annonce pas", name)
	}
}

// Un membre sans configuration, une organisation sans activation, une
// identité incomplète : autant de cas où la table de l'hôte reste le
// magasin.
func TestEventStoreFor_FallsBackToTheHostTable(t *testing.T) {
	manager, db := newTestManager(t, config.Plugins{})
	seedOrgAndMember(t, db, "atelier", "cam")
	activateEcho(t, db, "atelier")

	ctx := context.Background()
	cases := map[string][2]string{
		"membre sans configuration": {"atelier", "cam"},
		"organisation inconnue":     {"inconnue", "cam"},
		"membre vide":               {"atelier", ""},
		"organisation vide":         {"", "cam"},
	}
	for name, ids := range cases {
		if store, ok := manager.EventStoreFor(ctx, db, ids[0], ids[1]); ok {
			t.Errorf("%s: magasin %q résolu, attendu aucun", name, store)
		}
	}
}
