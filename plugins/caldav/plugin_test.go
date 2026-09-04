package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/automata/pkg/pluginsdk"
	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// Le descripteur annonce le magasin d'événements : sans ce drapeau,
// l'hôte n'appellerait jamais PutEvent, et cocher « ranger mes rappels
// dans cet agenda » n'aurait aucun effet visible.
func TestDescribe_AnnouncesTheEventStore(t *testing.T) {
	desc, err := newPlugin().Describe(context.Background(), &proto.DescribeRequest{})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}

	if !desc.ProvidesEventStore {
		t.Error("le descripteur n'annonce pas de magasin d'événements")
	}
	if !desc.HasTriggers {
		t.Error("sans flux de déclencheurs, aucune échéance ne serait annoncée")
	}
	if desc.PermissionDomain == "" {
		t.Error("domaine de permission absent : les écritures ne seraient rattachées à rien")
	}
}

// La clé qui revendique le magasin doit être exactement celle que l'hôte
// lit. Une faute de frappe ne casserait rien de visible : les rappels
// partiraient simplement ailleurs, sans un mot.
func TestMemberConfig_UsesTheReservedKey(t *testing.T) {
	raw, err := json.Marshal(memberConfig{ServerURL: "https://exemple.fr", Username: "cam", EventStore: true})
	if err != nil {
		t.Fatalf("sérialisation: %v", err)
	}

	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("relecture: %v", err)
	}

	claimed, ok := probe[pluginsdk.EventStoreConfigKey].(bool)
	if !ok || !claimed {
		t.Errorf("la configuration ne revendique pas le magasin sous %q: %s", pluginsdk.EventStoreConfigKey, raw)
	}
}

// Sans magasin revendiqué, la clé vaut faux : l'hôte garde ses rappels
// dans sa table, ce qui est le comportement historique.
func TestMemberConfig_QuietByDefault(t *testing.T) {
	cfg, err := parseConfig(`{"server_url":"https://exemple.fr","username":"cam"}`)
	if err != nil {
		t.Fatalf("lecture: %v", err)
	}
	if cfg.EventStore {
		t.Error("le magasin est revendiqué alors que rien ne le demande")
	}
	if !cfg.complete() {
		t.Error("cette configuration devrait être jugée complète")
	}
}

// Un compte incomplet n'a pas d'outils : l'assistant ne doit pas proposer
// de consulter un agenda qui n'existe pas.
func TestListTools_EmptyWithoutAConfiguredCalendar(t *testing.T) {
	p := newPlugin()
	p.SetHostClient(stubHost{})

	out, err := p.ListTools(context.Background(), &proto.ListToolsInput{
		Ctx: &proto.CallContext{OrgId: "org", MemberId: "cam"},
	})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(out.Tools) != 0 {
		t.Errorf("%d outil(s) exposé(s) sans agenda configuré", len(out.Tools))
	}
}

// Les outils suivent les interrupteurs du membre : ce que l'agent ne voit
// pas, il ne peut pas le demander.
func TestListTools_FollowsTheMemberSwitches(t *testing.T) {
	cases := map[string]struct {
		cfg     memberConfig
		wantRO  bool
		wantRW  bool
		toolLen int
	}{
		"lecture seule": {
			cfg:     memberConfig{ServerURL: "https://exemple.fr", Username: "cam", AllowRead: true},
			wantRO:  true,
			toolLen: 2,
		},
		"lecture et écriture": {
			cfg:     memberConfig{ServerURL: "https://exemple.fr", Username: "cam", AllowRead: true, AllowWrite: true},
			wantRO:  true,
			wantRW:  true,
			toolLen: 4,
		},
		"tout fermé": {
			cfg:     memberConfig{ServerURL: "https://exemple.fr", Username: "cam"},
			toolLen: 0,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := newPlugin()
			p.SetHostClient(stubHost{config: tc.cfg.marshal()})

			out, err := p.ListTools(context.Background(), &proto.ListToolsInput{
				Ctx: &proto.CallContext{OrgId: "org", MemberId: "cam"},
			})
			if err != nil {
				t.Fatalf("ListTools: %v", err)
			}
			if len(out.Tools) != tc.toolLen {
				t.Fatalf("%d outil(s), attendu %d", len(out.Tools), tc.toolLen)
			}

			var names []string
			for _, tool := range out.Tools {
				names = append(names, tool.Name)
				// Toute écriture doit passer par la confirmation de
				// l'hôte : un outil d'écriture marqué read_only
				// s'exécuterait dans le tour, sans que personne n'ait dit
				// oui.
				if strings.HasPrefix(tool.Name, "calendar_create") || strings.HasPrefix(tool.Name, "calendar_cancel") {
					if tool.ReadOnly {
						t.Errorf("%s est marqué read_only : il s'exécuterait sans confirmation", tool.Name)
					}
				}
			}
			joined := strings.Join(names, " ")
			if tc.wantRO && !strings.Contains(joined, "calendar_list_events") {
				t.Errorf("outils de lecture absents: %v", names)
			}
			if tc.wantRW != strings.Contains(joined, "calendar_create_event") {
				t.Errorf("outils d'écriture inattendus: %v", names)
			}
		})
	}
}

// L'annonce d'une échéance porte le texte à livrer et un identifiant qui
// distingue les occurrences : sans cela, la deuxième sonnerie d'une série
// serait prise pour un doublon de la première et jamais délivrée.
func TestDueEvent_CarriesTheTextAndDistinguishesOccurrences(t *testing.T) {
	first := reminderEvent{UID: "automata-1", Text: "Arroser les plantes", FireAt: time.Date(2026, 8, 25, 20, 0, 0, 0, time.UTC)}
	second := first
	second.FireAt = first.FireAt.AddDate(0, 0, 7)

	a := dueEvent("org", "cam", first)
	b := dueEvent("org", "cam", second)

	if a.DeliverText != "Arroser les plantes" {
		t.Errorf("texte annoncé %q", a.DeliverText)
	}
	if a.AgentInput != "" {
		t.Error("un rappel ne doit pas faire travailler le sous-agent")
	}
	if a.Id == b.Id {
		t.Errorf("deux occurrences partagent l'identifiant %q", a.Id)
	}
	if !strings.HasPrefix(a.Id, "automata-1@") {
		t.Errorf("identifiant d'occurrence inattendu: %q", a.Id)
	}
}

// stubHost est un client hôte minimal : il rend une configuration figée et
// aucun secret.
type stubHost struct {
	pluginsdk.UnimplementedHostClient
	config string
}

func (s stubHost) GetConfig(context.Context, string, string) (string, bool, error) {
	return s.config, s.config != "", nil
}
func (s stubHost) SaveConfig(context.Context, string, string, string) error { return nil }
func (s stubHost) ListConfigs(context.Context) ([]pluginsdk.ConfigEntry, error) {
	return nil, nil
}
func (s stubHost) GetSecret(context.Context, string, string, string) (string, bool, error) {
	return "", false, nil
}
func (s stubHost) SetSecret(context.Context, string, string, string, string) error { return nil }
func (s stubHost) DeleteSecret(context.Context, string, string, string) error      { return nil }
func (s stubHost) Notify(context.Context, string, string, string) error            { return nil }

// La recherche par titre couvre le passé. Le 4 septembre 2026, une question
// au passé — « quelles étaient mes réunions cette semaine ? » — ne pouvait
// recevoir qu'« aucune » : la fenêtre commençait à l'instant de la question,
// et la réunion du mercredi était derrière elle.
func TestSearchWindow_ReachesIntoThePast(t *testing.T) {
	now := time.Date(2026, 9, 4, 9, 24, 0, 0, time.UTC)
	from, to := searchWindow(now)

	mercredi := time.Date(2026, 9, 2, 14, 0, 0, 0, time.UTC)
	if !from.Before(mercredi) {
		t.Errorf("fenêtre = [%s, %s], la réunion du %s reste hors de portée", from, to, mercredi)
	}
	if !to.After(now) {
		t.Errorf("fenêtre = [%s, %s], l'avenir n'est plus couvert", from, to)
	}
}

// Le défaut de calendar_list_events, lui, part bien de maintenant : c'est
// un choix, pas un oubli. Il doit rester dit dans le schéma, sans quoi le
// modèle croit lister « la semaine » et ne voit que ce qui reste du jour.
func TestListEventsSchema_SaysTheDefaultLeavesOutThePast(t *testing.T) {
	p := newPlugin()
	p.SetHostClient(stubHost{config: `{"server_url":"https://exemple.fr","username":"cam","allow_read":true}`})

	out, err := p.ListTools(context.Background(), &proto.ListToolsInput{
		Ctx: &proto.CallContext{OrgId: "home", MemberId: "cam"},
	})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	var schema, description string
	for _, tool := range out.Tools {
		if tool.Name == "calendar_list_events" {
			schema, description = tool.InputSchemaJson, tool.Description
		}
	}
	if schema == "" {
		t.Fatal("calendar_list_events absent des outils exposés")
	}
	if !strings.Contains(schema, "leaves out") {
		t.Errorf("le schéma ne dit pas ce que le défaut écarte: %s", schema)
	}
	if !strings.Contains(description, "PAST") {
		t.Errorf("la description ne dit pas comment interroger le passé: %s", description)
	}
}
