package skills_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/skills"
)

func testDB(t *testing.T) *persistence.DB {
	t.Helper()

	db, err := persistence.Open(context.Background(), config.StorageApplication{
		Driver: "sqlite3",
		Path:   filepath.Join(t.TempDir(), "automata.sqlite"),
	})
	if err != nil {
		t.Fatalf("ouverture de la base: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return db
}

// Le frontmatter porte le contrat : sans nom, la compétence n'est pas
// désignable par le modèle et ne doit pas passer en silence.
func TestParseRequiresName(t *testing.T) {
	_, err := skills.Parse([]byte("---\ndescription: Something\n---\n\nBody.\n"))
	if err == nil {
		t.Fatal("un frontmatter sans nom devrait être refusé")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Errorf("l'erreur devrait nommer le champ manquant: %v", err)
	}
}

func TestParseReadsFrontmatterAndBody(t *testing.T) {
	def, err := skills.Parse([]byte("---\nname: mask-logo\ndescription: Mask a logo\nagents: [workspace, research]\n---\n\n# Title\n\nStep one.\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if def.Name != "mask-logo" || def.Description != "Mask a logo" {
		t.Errorf("frontmatter mal lu: %+v", def)
	}
	if len(def.Agents) != 2 || def.Agents[0] != "workspace" || def.Agents[1] != "research" {
		t.Errorf("ciblage mal lu: %v", def.Agents)
	}
	if !strings.HasPrefix(def.Content, "# Title") || !strings.Contains(def.Content, "Step one.") {
		t.Errorf("corps mal découpé: %q", def.Content)
	}
}

// Un nom hors kebab-case voyage jusque dans les arguments d'outil du
// modèle : le refuser au plus tôt.
func TestParseRejectsInvalidName(t *testing.T) {
	if _, err := skills.Parse([]byte("---\nname: Mask Logo\ndescription: x\n---\n\nBody.\n")); err == nil {
		t.Fatal("un nom avec espace et majuscules devrait être refusé")
	}
}

// Les compétences embarquées font partie du dépôt : leur frontmatter doit
// être valide avant tout déploiement.
func TestDefaultsParse(t *testing.T) {
	defs, err := skills.Defaults()
	if err != nil {
		t.Fatalf("Defaults: %v", err)
	}
	if len(defs) == 0 {
		t.Fatal("aucune compétence embarquée")
	}
	for _, def := range defs {
		if def.Name == "" || def.Description == "" || def.Content == "" {
			t.Errorf("compétence embarquée incomplète: %+v", def)
		}
	}
}

// Le semis insère les noms absents et ne touche JAMAIS un nom présent :
// une compétence éditée dans l'administration survit à un redéploiement.
func TestSeedNeverOverwritesExistingSkill(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if err := skills.Seed(ctx, db, nil); err != nil {
		t.Fatalf("premier semis: %v", err)
	}

	defs, err := skills.Defaults()
	if err != nil {
		t.Fatalf("Defaults: %v", err)
	}
	name := defs[0].Name

	repo := persistence.NewSkillRepository()
	edited := "Edited by the operator."
	if err := db.WithTx(ctx, func(tx *sql.Tx) error {
		skill, found, err := repo.Get(ctx, tx, name)
		if err != nil || !found {
			t.Fatalf("compétence %q absente après le semis (err=%v)", name, err)
		}
		skill.Content = edited
		// Marqueur d'édition : c'est lui, et non l'horodatage, qui protège
		// le travail de l'administrateur (les dates perdent en précision au
		// stockage et une édition dans la même seconde que le semis serait
		// indiscernable).
		skill.Edited = true
		skill.UpdatedAt = time.Now().UTC()
		return repo.Upsert(ctx, tx, skill)
	}); err != nil {
		t.Fatalf("édition: %v", err)
	}

	if err := skills.Seed(ctx, db, nil); err != nil {
		t.Fatalf("second semis: %v", err)
	}

	if err := db.WithTx(ctx, func(tx *sql.Tx) error {
		skill, _, err := repo.Get(ctx, tx, name)
		if err != nil {
			return err
		}
		if skill.Content != edited {
			t.Errorf("le semis a écrasé une compétence éditée: %q", skill.Content)
		}
		return nil
	}); err != nil {
		t.Fatalf("relecture: %v", err)
	}
}

// upsert écrit une compétence de test.
func upsert(t *testing.T, db *persistence.DB, skill persistence.Skill) {
	t.Helper()

	ctx := context.Background()
	now := time.Now().UTC()
	skill.CreatedAt, skill.UpdatedAt = now, now

	if err := db.WithTx(ctx, func(tx *sql.Tx) error {
		return persistence.NewSkillRepository().Upsert(ctx, tx, skill)
	}); err != nil {
		t.Fatalf("Upsert %q: %v", skill.Name, err)
	}
}

// Le catalogue ne montre que les compétences actives et ciblées.
func TestSkillsForFiltersEnabledAndTargeted(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	upsert(t, db, persistence.Skill{Name: "for-all", Description: "Everyone", Content: "A", Enabled: true})
	upsert(t, db, persistence.Skill{Name: "for-workspace", Description: "Workspace only", Content: "B", Agents: []string{"workspace"}, Enabled: true})
	upsert(t, db, persistence.Skill{Name: "disabled", Description: "Off", Content: "C", Enabled: false})

	provider := skills.NewProvider(db, nil)

	names := func(agentName string) []string {
		var out []string
		for _, s := range provider.SkillsFor(ctx, agentName) {
			out = append(out, s.Name)
		}
		return out
	}

	got := strings.Join(names("workspace"), ",")
	if got != "for-all,for-workspace" {
		t.Errorf("catalogue de workspace: %q", got)
	}

	got = strings.Join(names("research"), ",")
	if got != "for-all" {
		t.Errorf("catalogue de research: %q", got)
	}
}

// Le modèle désigne, il ne décide pas : un nom deviné pour un agent non
// ciblé reste introuvable, et une compétence désactivée aussi.
func TestLoadSkillRefusesUntargetedAgent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	upsert(t, db, persistence.Skill{Name: "for-workspace", Description: "Workspace only", Content: "SECRET RECIPE", Agents: []string{"workspace"}, Enabled: true})
	upsert(t, db, persistence.Skill{Name: "disabled", Description: "Off", Content: "D", Enabled: false})

	provider := skills.NewProvider(db, nil)

	content, found := provider.LoadSkill(ctx, "workspace", "for-workspace")
	if !found || content != "SECRET RECIPE" {
		t.Errorf("l'agent ciblé devrait charger la compétence: %q, %v", content, found)
	}

	if _, found := provider.LoadSkill(ctx, "research", "for-workspace"); found {
		t.Error("un agent non ciblé ne devrait rien charger")
	}

	if _, found := provider.LoadSkill(ctx, "workspace", "disabled"); found {
		t.Error("une compétence désactivée ne devrait rien charger")
	}
}

// BuiltinContent alimente la restauration : elle doit rendre la version
// embarquée, jamais celle de la base.
func TestBuiltinContent(t *testing.T) {
	defs, err := skills.Defaults()
	if err != nil {
		t.Fatalf("Defaults: %v", err)
	}

	def, ok := skills.BuiltinContent(defs[0].Name)
	if !ok || def.Content != defs[0].Content {
		t.Errorf("version d'origine introuvable pour %q", defs[0].Name)
	}

	if _, ok := skills.BuiltinContent("skill-that-does-not-exist"); ok {
		t.Error("une compétence inconnue ne devrait pas avoir de version d'origine")
	}
}

// Une compétence fournie par le projet et jamais éditée suit les mises à
// jour du dépôt : corriger une recette livrée doit profiter aux instances
// déjà semées, sans intervention manuelle.
func TestSeedRefreshesUneditedBuiltinSkills(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if err := skills.Seed(ctx, db, nil); err != nil {
		t.Fatalf("premier semis: %v", err)
	}

	defs, err := skills.Defaults()
	if err != nil {
		t.Fatalf("Defaults: %v", err)
	}
	name := defs[0].Name

	repo := persistence.NewSkillRepository()

	// Simule une version antérieure livrée par le projet : contenu
	// différent, jamais éditée par un administrateur.
	if err := db.WithTx(ctx, func(tx *sql.Tx) error {
		skill, found, err := repo.Get(ctx, tx, name)
		if err != nil || !found {
			t.Fatalf("compétence %q absente après le semis (err=%v)", name, err)
		}
		skill.Content = "Older version shipped by the project."
		return repo.Upsert(ctx, tx, skill)
	}); err != nil {
		t.Fatalf("mise en place: %v", err)
	}

	if err := skills.Seed(ctx, db, nil); err != nil {
		t.Fatalf("second semis: %v", err)
	}

	if err := db.WithTx(ctx, func(tx *sql.Tx) error {
		skill, _, err := repo.Get(ctx, tx, name)
		if err != nil {
			return err
		}
		if skill.Content != defs[0].Content {
			t.Errorf("la compétence non éditée aurait dû être rafraîchie, contenu = %q", skill.Content)
		}
		return nil
	}); err != nil {
		t.Fatalf("relecture: %v", err)
	}
}
