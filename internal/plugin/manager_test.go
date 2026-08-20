package plugin

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/secretbox"
	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// echoBinary est compilé une fois pour toute la suite : lancer de vrais
// sous-processus est le seul moyen d'éprouver le handshake, le broker et
// les redémarrages.
var echoBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "automata-plugin-test")
	if err != nil {
		panic(err)
	}
	echoBinary = filepath.Join(dir, "echo")

	build := exec.Command("go", "build", "-o", echoBinary, ".")
	build.Dir = "testdata/plugin-echo"
	build.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := build.CombinedOutput(); err != nil {
		panic("compilation du plugin d'essai: " + err.Error() + "\n" + string(out))
	}

	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

const testSessionSecret = "un-secret-de-session-de-test-32-octets"

// newTestManager installe le binaire echo dans un répertoire de plugins et
// démarre un manager complet sur une base réelle.
func newTestManager(t *testing.T, cfg config.Plugins) (*Manager, *persistence.DB) {
	t.Helper()

	if cfg.Dir == "" {
		cfg.Dir = t.TempDir()
		data, err := os.ReadFile(echoBinary)
		if err != nil {
			t.Fatalf("lecture du binaire echo: %v", err)
		}
		if err := os.WriteFile(filepath.Join(cfg.Dir, "echo"), data, 0o755); err != nil {
			t.Fatalf("installation du binaire echo: %v", err)
		}
	}

	db, err := persistence.Open(context.Background(), config.StorageApplication{
		Driver: "sqlite3",
		Path:   filepath.Join(t.TempDir(), "app.sqlite"),
	})
	if err != nil {
		t.Fatalf("ouverture de la base: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	box, err := secretbox.NewPlugins(testSessionSecret)
	if err != nil {
		t.Fatalf("dérivation de la clé: %v", err)
	}

	manager := NewManager(cfg, NewHostService(db, box), []string{"main"})
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(manager.Shutdown)

	return manager, db
}

func TestManager_LoadsAndDescribes(t *testing.T) {
	manager, _ := newTestManager(t, config.Plugins{})

	statuses := manager.Statuses()
	if len(statuses) != 1 {
		t.Fatalf("%d plugin(s) chargé(s), attendu 1", len(statuses))
	}

	st := statuses[0]
	if st.Name != "echo" || !st.Running || !st.HasUI || !st.HasSubAgent {
		t.Errorf("état inattendu: %+v", st)
	}

	// L'interface embarquée a un port et un jeton.
	port, token, ok := manager.UIEndpoint("echo")
	if !ok || port == 0 || len(token) < 32 {
		t.Errorf("point d'accès UI inattendu: port=%d token=%d car.", port, len(token))
	}
}

// Un lien symbolique vers l'extérieur du répertoire ne doit jamais être
// exécuté : c'est la barrière contre un répertoire de plugins altéré.
func TestManager_RefusesEscapingSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "evil")
	if err := os.WriteFile(outside, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("création du binaire extérieur: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "evil")); err != nil {
		t.Fatalf("création du lien: %v", err)
	}

	manager, _ := newTestManager(t, config.Plugins{Dir: dir})

	if statuses := manager.Statuses(); len(statuses) != 0 {
		t.Errorf("%d plugin(s) chargé(s) depuis un lien sortant, attendu 0", len(statuses))
	}
}

// La collision avec un nom d'agent configuré est refusée : le nom du
// plugin devient un outil delegate_to_<nom>, deux homonymes seraient
// indiscernables pour le modèle.
func TestManager_RefusesAgentNameCollision(t *testing.T) {
	dir := t.TempDir()
	data, _ := os.ReadFile(echoBinary)
	if err := os.WriteFile(filepath.Join(dir, "echo"), data, 0o755); err != nil {
		t.Fatalf("installation: %v", err)
	}

	db, err := persistence.Open(context.Background(), config.StorageApplication{
		Driver: "sqlite3", Path: filepath.Join(t.TempDir(), "app.sqlite"),
	})
	if err != nil {
		t.Fatalf("ouverture de la base: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	box, _ := secretbox.NewPlugins(testSessionSecret)

	manager := NewManager(config.Plugins{Dir: dir}, NewHostService(db, box), []string{"echo"})
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(manager.Shutdown)

	if statuses := manager.Statuses(); len(statuses) != 0 {
		t.Errorf("le plugin homonyme d'un agent a été chargé")
	}
}

func TestManager_RestartsDeadPlugin(t *testing.T) {
	manager, _ := newTestManager(t, config.Plugins{RestartCooldown: config.Duration(time.Millisecond)})

	client, _, ok := manager.GetOrRestart(context.Background(), "echo")
	if !ok {
		t.Fatal("plugin introuvable")
	}
	if _, err := client.Describe(context.Background(), &proto.DescribeRequest{}); err != nil {
		t.Fatalf("Describe avant kill: %v", err)
	}

	// Tuer le sous-processus par l'intérieur.
	manager.mu.Lock()
	manager.plugins[0].gopClient.Kill()
	manager.mu.Unlock()

	// Le point d'usage relance.
	client, desc, ok := manager.GetOrRestart(context.Background(), "echo")
	if !ok {
		t.Fatal("le plugin mort n'a pas été relancé")
	}
	if desc.Name != "echo" {
		t.Errorf("descripteur inattendu après relance: %q", desc.Name)
	}
	if _, err := client.Describe(context.Background(), &proto.DescribeRequest{}); err != nil {
		t.Errorf("Describe après relance: %v", err)
	}
}

func TestManager_CooldownBlocksRestartStorm(t *testing.T) {
	manager, _ := newTestManager(t, config.Plugins{RestartCooldown: config.Duration(time.Hour)})

	kill := func() {
		manager.mu.Lock()
		for _, e := range manager.plugins {
			e.gopClient.Kill()
		}
		manager.mu.Unlock()
	}

	kill()
	if _, _, ok := manager.GetOrRestart(context.Background(), "echo"); !ok {
		t.Fatal("première relance refusée")
	}

	kill()
	if _, _, ok := manager.GetOrRestart(context.Background(), "echo"); ok {
		t.Fatal("relance acceptée pendant le refroidissement")
	}

	// L'action humaine ignore le refroidissement.
	if !manager.Restart(context.Background(), "echo") {
		t.Fatal("le redémarrage manuel a été refusé")
	}
}

// seedOrgAndMember prépare une organisation et un membre pour les tests du
// service hôte.
func seedOrgAndMember(t *testing.T, db *persistence.DB, orgID, memberID string) {
	t.Helper()

	now := time.Now()
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		if err := persistence.NewOrganizationRepository().Insert(context.Background(), tx, persistence.Organization{
			ID: orgID, DisplayName: orgID, CreatedAt: now, UpdatedAt: now,
		}, true); err != nil {
			return err
		}
		if memberID == "" {
			return nil
		}
		return persistence.NewMemberRepository().Insert(context.Background(), tx, persistence.Member{
			ID: memberID, OrgID: orgID, DisplayName: "Membre", Role: persistence.MemberRoleMember,
			CreatedAt: now, UpdatedAt: now,
		}, true)
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// Le scoping par connexion est la correction centrale du patron Xolo : un
// plugin ne lit jamais les secrets d'un autre, quoi qu'il envoie.
func TestHostService_ScopesSecretsByPlugin(t *testing.T) {
	_, db := newTestManager(t, config.Plugins{})
	seedOrgAndMember(t, db, "atelier", "cam")

	box, _ := secretbox.NewPlugins(testSessionSecret)
	host := NewHostService(db, box)

	a := host.scopedTo("plugin-a")
	b := host.scopedTo("plugin-b")

	ctx := context.Background()
	if _, err := a.SetSecret(ctx, &proto.SetSecretRequest{OrgId: "atelier", MemberId: "cam", Key: "token", Value: "secret-de-a"}); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}

	resp, err := b.GetSecret(ctx, &proto.GetSecretRequest{OrgId: "atelier", MemberId: "cam", Key: "token"})
	if err != nil {
		t.Fatalf("GetSecret par b: %v", err)
	}
	if resp.Found {
		t.Fatal("le plugin b lit un secret du plugin a")
	}

	resp, err = a.GetSecret(ctx, &proto.GetSecretRequest{OrgId: "atelier", MemberId: "cam", Key: "token"})
	if err != nil {
		t.Fatalf("GetSecret par a: %v", err)
	}
	if !resp.Found || resp.Value != "secret-de-a" {
		t.Errorf("le plugin a ne relit pas son secret: %+v", resp)
	}
}

// Un membre d'une autre organisation est refusé : les identifiants d'un
// événement ou d'un appel de plugin ne sont jamais crus sur parole.
func TestHostService_RejectsForeignMember(t *testing.T) {
	_, db := newTestManager(t, config.Plugins{})
	seedOrgAndMember(t, db, "atelier", "cam")
	seedOrgAndMember(t, db, "autre", "")

	box, _ := secretbox.NewPlugins(testSessionSecret)
	scoped := NewHostService(db, box).scopedTo("plugin-a")

	_, err := scoped.SetSecret(context.Background(), &proto.SetSecretRequest{
		OrgId: "autre", MemberId: "cam", Key: "token", Value: "x",
	})
	if err == nil {
		t.Fatal("un membre étranger à l'organisation a été accepté")
	}
}

// ListConfigs ne sert que les organisations où le plugin est actif : une
// organisation qui désactive un plugin cesse immédiatement d'être visible
// de lui.
func TestHostService_ListConfigsHonorsActivation(t *testing.T) {
	_, db := newTestManager(t, config.Plugins{})
	seedOrgAndMember(t, db, "active", "cam")
	seedOrgAndMember(t, db, "inactive", "")

	box, _ := secretbox.NewPlugins(testSessionSecret)
	host := NewHostService(db, box)
	scoped := host.scopedTo("echo")

	ctx := context.Background()
	now := time.Now().UTC()
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		repo := persistence.NewPluginActivationRepository()
		if err := repo.Upsert(ctx, tx, persistence.PluginActivation{PluginName: "echo", OrgID: "active", Enabled: true, CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		return repo.Upsert(ctx, tx, persistence.PluginActivation{PluginName: "echo", OrgID: "inactive", Enabled: false, CreatedAt: now, UpdatedAt: now})
	})
	if err != nil {
		t.Fatalf("activations: %v", err)
	}

	for _, org := range []string{"active", "inactive"} {
		if _, err := scoped.SaveConfig(ctx, &proto.SaveConfigRequest{OrgId: org, ConfigJson: `{"org":"` + org + `"}`}); err != nil {
			t.Fatalf("SaveConfig %s: %v", org, err)
		}
	}

	resp, err := scoped.ListConfigs(ctx, &proto.ListConfigsRequest{})
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	if len(resp.Entries) != 1 || resp.Entries[0].OrgId != "active" {
		t.Errorf("configurations servies: %+v, attendu la seule organisation active", resp.Entries)
	}
}

// Le test différentiel des secrets : rien de ce qui est confié au service
// hôte ne doit être lisible dans le fichier de base.
func TestHostService_NothingInClearOnDisk(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "app.sqlite")
	db, err := persistence.Open(context.Background(), config.StorageApplication{Driver: "sqlite3", Path: dbPath})
	if err != nil {
		t.Fatalf("ouverture: %v", err)
	}
	seedOrgAndMember(t, db, "atelier", "cam")

	box, _ := secretbox.NewPlugins(testSessionSecret)
	scoped := NewHostService(db, box).scopedTo("email")

	ctx := context.Background()
	if _, err := scoped.SetSecret(ctx, &proto.SetSecretRequest{
		OrgId: "atelier", MemberId: "cam", Key: "imap_password", Value: "s3cr3t-imap-pass",
	}); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	if _, err := scoped.SaveConfig(ctx, &proto.SaveConfigRequest{
		OrgId: "atelier", MemberId: "cam", ConfigJson: `{"imap_host":"HOTE-VISIBLE.example"}`,
	}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	// L'aller-retour fonctionne…
	got, err := scoped.GetSecret(ctx, &proto.GetSecretRequest{OrgId: "atelier", MemberId: "cam", Key: "imap_password"})
	if err != nil || !got.Found || got.Value != "s3cr3t-imap-pass" {
		t.Fatalf("relecture du secret: %+v, %v", got, err)
	}

	// …et le disque ne montre rien.
	if err := db.Close(); err != nil {
		t.Fatalf("fermeture: %v", err)
	}
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("lecture du fichier: %v", err)
	}
	for _, needle := range []string{"s3cr3t-imap-pass", "HOTE-VISIBLE"} {
		if strings.Contains(string(raw), needle) {
			t.Errorf("%q apparaît en clair dans le fichier de base", needle)
		}
	}
}
