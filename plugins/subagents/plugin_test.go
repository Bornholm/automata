package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/bornholm/automata/pkg/pluginsdk"
	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// fakeHost tient les configurations et les secrets en mémoire, cloisonnés
// par (organisation, membre) comme le vrai service hôte.
type fakeHost struct {
	pluginsdk.UnimplementedHostClient
	configs map[string]string
	secrets map[string]string
}

func newFakeHost() *fakeHost {
	return &fakeHost{configs: map[string]string{}, secrets: map[string]string{}}
}

func (h *fakeHost) GetConfig(_ context.Context, orgID, memberID string) (string, bool, error) {
	raw, ok := h.configs[orgID+"|"+memberID]
	return raw, ok, nil
}

func (h *fakeHost) SaveConfig(_ context.Context, orgID, memberID, raw string) error {
	h.configs[orgID+"|"+memberID] = raw
	return nil
}

func (h *fakeHost) GetSecret(_ context.Context, orgID, memberID, key string) (string, bool, error) {
	value, ok := h.secrets[orgID+"|"+memberID+"|"+key]
	return value, ok, nil
}

func (h *fakeHost) SetSecret(_ context.Context, orgID, memberID, key, value string) error {
	h.secrets[orgID+"|"+memberID+"|"+key] = value
	return nil
}

// testPlugin monte le plugin sur un catalogue d'une entrée dont le serveur
// stdio relance le binaire de test (voir mcp_test.go).
func testPlugin(t *testing.T) (*Plugin, *fakeHost) {
	t.Helper()

	p := newPlugin(catalog{Agents: []catalogAgent{fakeAgent()}})
	t.Cleanup(p.pool.close)

	host := newFakeHost()
	p.SetHostClient(host)

	return p, host
}

func listSubAgents(t *testing.T, p *Plugin, orgID, memberID string) []*proto.NamedSubAgent {
	t.Helper()
	out, err := p.ListSubAgents(context.Background(), &proto.ListSubAgentsInput{
		Ctx: &proto.CallContext{OrgId: orgID, MemberId: memberID},
	})
	if err != nil {
		t.Fatalf("ListSubAgents: %v", err)
	}
	return out.SubAgents
}

func TestDescribe_AnnouncesACatalog(t *testing.T) {
	desc, err := newPlugin(catalog{}).Describe(context.Background(), &proto.DescribeRequest{})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}

	if !desc.ProvidesSubAgents {
		t.Error("sans provides_sub_agents, l'hôte n'appellerait jamais ListSubAgents")
	}
	// Le descripteur ne porte PAS de sous-agent unique : les entrées
	// viennent du catalogue, par membre.
	if desc.SubAgent != nil {
		t.Error("le descripteur ne doit pas déclarer de sous-agent unique")
	}
	if desc.PermissionDomain == "" {
		t.Error("domaine de permission absent : les écritures ne seraient rattachées à rien")
	}
}

// Une entrée non activée n'est pas montée : le catalogue propose, le
// membre choisit.
func TestListSubAgents_MountsOnlyWhatTheMemberEnabled(t *testing.T) {
	p, host := testPlugin(t)

	if agents := listSubAgents(t, p, "atelier", "cam"); len(agents) != 0 {
		t.Fatalf("sous-agents montés sans activation: %+v", agents)
	}

	// Activée, mais l'identifiant requis manque : toujours rien. Un
	// sous-agent dont chaque outil échoue est pire qu'un sous-agent absent.
	_ = host.SaveConfig(context.Background(), "atelier", "cam", memberConfig{Enabled: []string{"probe"}}.marshal())
	if agents := listSubAgents(t, p, "atelier", "cam"); len(agents) != 0 {
		t.Fatalf("sous-agent monté sans son identifiant requis: %+v", agents)
	}

	_ = host.SetSecret(context.Background(), "atelier", "cam", secretKey("probe", "token"), "jeton-de-cam")

	agents := listSubAgents(t, p, "atelier", "cam")
	if len(agents) != 1 {
		t.Fatalf("%d sous-agent(s), attendu 1", len(agents))
	}
	if agents[0].Name != "probe" {
		t.Errorf("nom du sous-agent: %q", agents[0].Name)
	}
	if agents[0].SubAgent.GetSystemPrompt() == "" || agents[0].SubAgent.GetDescription() == "" {
		t.Error("prompt ou description absents : le modèle ne saurait ni quoi déléguer ni comment")
	}
	if len(agents[0].Tools) != 2 {
		t.Errorf("%d outil(s) annoncé(s), attendu 2", len(agents[0].Tools))
	}

	// L'activation de l'un ne monte rien chez l'autre.
	if agents := listSubAgents(t, p, "atelier", "yann"); len(agents) != 0 {
		t.Errorf("l'activation d'un membre a débordé sur un autre: %+v", agents)
	}
}

// Un appel d'outil est routé vers l'entrée nommée, et exécuté avec les
// identifiants du membre qui appelle.
func TestCallTool_UsesTheCallersOwnCredentials(t *testing.T) {
	p, host := testPlugin(t)

	for member, token := range map[string]string{"cam": "jeton-de-cam", "yann": "jeton-de-yann"} {
		_ = host.SaveConfig(context.Background(), "atelier", member, memberConfig{Enabled: []string{"probe"}}.marshal())
		_ = host.SetSecret(context.Background(), "atelier", member, secretKey("probe", "token"), token)
	}

	for member, want := range map[string]string{"cam": "token=jeton-de-cam", "yann": "token=jeton-de-yann"} {
		out, err := p.CallTool(context.Background(), &proto.CallToolInput{
			Ctx:      &proto.CallContext{OrgId: "atelier", MemberId: member},
			SubAgent: "probe",
			Name:     "whoami",
		})
		if err != nil {
			t.Fatalf("CallTool(%s): %v", member, err)
		}
		if out.IsError {
			t.Fatalf("CallTool(%s) en échec: %s", member, out.ResultText)
		}
		if !strings.Contains(out.ResultText, want) {
			t.Errorf("%s a obtenu %q, attendu %q", member, out.ResultText, want)
		}
	}
}

// L'activation est re-vérifiée à l'appel : une entrée désactivée entre la
// proposition d'une écriture et sa confirmation n'exécute rien.
func TestCallTool_RefusesADisabledEntry(t *testing.T) {
	p, host := testPlugin(t)
	_ = host.SetSecret(context.Background(), "atelier", "cam", secretKey("probe", "token"), "jeton")

	out, err := p.CallTool(context.Background(), &proto.CallToolInput{
		Ctx:      &proto.CallContext{OrgId: "atelier", MemberId: "cam"},
		SubAgent: "probe",
		Name:     "whoami",
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !out.IsError {
		t.Fatalf("un outil d'une entrée désactivée a été exécuté: %s", out.ResultText)
	}
}

// Un nom d'entrée qui ne vient pas du catalogue n'ouvre rien : c'est la
// seule barrière si un appel arrivait avec un sous-agent inventé.
func TestCallTool_RefusesAnUnknownSubAgent(t *testing.T) {
	p, _ := testPlugin(t)

	out, err := p.CallTool(context.Background(), &proto.CallToolInput{
		Ctx:      &proto.CallContext{OrgId: "atelier", MemberId: "cam"},
		SubAgent: "inventé",
		Name:     "whoami",
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !out.IsError {
		t.Error("un sous-agent inconnu a été servi")
	}
}

// ListTools reste vide : les outils voyagent avec leur sous-agent, et un
// appel égaré ne doit rien exposer.
func TestListTools_ExposesNothing(t *testing.T) {
	p, _ := testPlugin(t)

	out, err := p.ListTools(context.Background(), &proto.ListToolsInput{
		Ctx: &proto.CallContext{OrgId: "atelier", MemberId: "cam"},
	})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(out.Tools) != 0 {
		t.Errorf("%d outil(s) exposé(s) hors sous-agent", len(out.Tools))
	}
}

// Le réglage du membre ne contient QUE des noms d'entrées : ni URL, ni
// commande, ni en-tête, ni identifiant. Ce que le membre décide, c'est de
// choisir dans le catalogue de l'exploitant.
func TestMemberConfig_HoldsOnlyChoices(t *testing.T) {
	cfg := memberConfig{}.withEntry("probe", true).withEntry("autre", true).withEntry("probe", true)
	if len(cfg.Enabled) != 2 {
		t.Fatalf("doublon dans les activations: %+v", cfg.Enabled)
	}

	cfg = cfg.withEntry("probe", false)
	if cfg.enabled("probe") {
		t.Error("désactivation sans effet")
	}
	if !cfg.enabled("autre") {
		t.Error("la désactivation d'une entrée a emporté l'autre")
	}

	raw := cfg.marshal()
	for _, forbidden := range []string{"http", "command", "token", "Authorization"} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("le réglage du membre porte %q: %s", forbidden, raw)
		}
	}
}

// Le catalogue est lu au démarrage : un chemin qui ne mène à rien doit
// arrêter le plugin, pas le laisser tourner à vide.
func TestLoadCatalog_RefusesAMissingFile(t *testing.T) {
	t.Setenv(envCatalogFile, "/introuvable/catalogue.yaml")

	if _, err := loadCatalog(); err == nil {
		t.Fatal("un catalogue introuvable a été accepté")
	} else if !strings.Contains(err.Error(), "introuvable") {
		t.Errorf("message sans le chemin: %v", err)
	}

	if _, err := os.Stat("catalog/default.yaml"); err != nil {
		t.Errorf("catalogue embarqué absent du dépôt: %v", err)
	}
}
