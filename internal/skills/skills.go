// Package skills porte la bibliothèque de compétences : des modes
// opératoires en markdown que les agents chargent à la demande, sur le
// patron de la divulgation progressive (voir docs/skills.md).
//
// Deux sources cohabitent : les compétences fournies par le projet, en
// fichiers embarqués sous defaults/, et celles écrites depuis
// l'administration web. Les premières sont SEMÉES en base au démarrage,
// et uniquement pour les noms absents : une compétence éditée par un
// opérateur n'est jamais écrasée par un redéploiement.
//
// Le contenu d'une compétence part au modèle : il s'écrit en ANGLAIS,
// comme tout ce qui traverse le prompt. Le code et les journaux de ce
// paquet restent en français, et ne journalisent que des noms et des
// compteurs.
package skills

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/persistence"
)

//go:embed defaults/*.md
var defaultsFS embed.FS

// nameRE contraint le nom d'une compétence : kebab-case, sans espace. Le
// nom est une clé primaire et voyage jusque dans les arguments d'outil du
// modèle — mieux vaut qu'il reste simple à recopier.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ValidName indique si name est un nom de compétence acceptable.
func ValidName(name string) bool {
	return nameRE.MatchString(name)
}

// Definition est une compétence telle qu'elle est écrite en fichier :
// frontmatter YAML puis corps markdown.
type Definition struct {
	Name        string
	Description string
	Agents      []string
	Content     string
}

// frontmatter est le bloc YAML en tête de fichier.
type frontmatter struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Agents      []string `yaml:"agents"`
}

// Parse lit un fichier de compétence : un bloc `---` de frontmatter YAML,
// puis le corps markdown. Un nom ou une description manquants sont une
// erreur — le semis doit échouer bruyamment plutôt que de servir aux
// agents un catalogue muet.
func Parse(raw []byte) (Definition, error) {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	text = strings.TrimLeft(text, "\ufeff\n \t")

	if !strings.HasPrefix(text, "---\n") {
		return Definition{}, fmt.Errorf("frontmatter absent: le fichier doit commencer par une ligne ---")
	}

	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return Definition{}, fmt.Errorf("frontmatter non terminé: ligne --- de fermeture attendue")
	}

	var meta frontmatter
	if err := yaml.Unmarshal([]byte(rest[:end]), &meta); err != nil {
		return Definition{}, fmt.Errorf("frontmatter illisible: %w", err)
	}

	body := rest[end+len("\n---"):]
	if idx := strings.Index(body, "\n"); idx >= 0 {
		body = body[idx+1:]
	} else {
		body = ""
	}

	def := Definition{
		Name:        strings.TrimSpace(meta.Name),
		Description: strings.TrimSpace(meta.Description),
		Content:     strings.TrimSpace(body),
	}
	for _, a := range meta.Agents {
		if a = strings.TrimSpace(a); a != "" {
			def.Agents = append(def.Agents, a)
		}
	}

	if def.Name == "" {
		return Definition{}, fmt.Errorf("champ 'name' manquant dans le frontmatter")
	}
	if !ValidName(def.Name) {
		return Definition{}, fmt.Errorf("nom de compétence invalide %q: kebab-case attendu", def.Name)
	}
	if def.Description == "" {
		return Definition{}, fmt.Errorf("compétence %q: champ 'description' manquant", def.Name)
	}
	if def.Content == "" {
		return Definition{}, fmt.Errorf("compétence %q: corps markdown vide", def.Name)
	}

	return def, nil
}

// Defaults retourne les compétences embarquées, triées par nom. Une
// erreur ici est un défaut du dépôt, pas de l'exploitation : le test du
// paquet la fait remonter avant tout déploiement.
func Defaults() ([]Definition, error) {
	entries, err := fs.ReadDir(defaultsFS, "defaults")
	if err != nil {
		return nil, fmt.Errorf("lecture des compétences embarquées: %w", err)
	}

	defs := make([]Definition, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}

		raw, err := defaultsFS.ReadFile("defaults/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("lecture de %q: %w", entry.Name(), err)
		}

		def, err := Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("compétence embarquée %q: %w", entry.Name(), err)
		}
		defs = append(defs, def)
	}

	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })

	return defs, nil
}

// BuiltinContent retourne la compétence embarquée portant ce nom. C'est
// ce qui alimente le bouton « restaurer la version d'origine » de
// l'administration.
func BuiltinContent(name string) (Definition, bool) {
	defs, err := Defaults()
	if err != nil {
		return Definition{}, false
	}
	for _, def := range defs {
		if def.Name == name {
			return def, true
		}
	}
	return Definition{}, false
}

// Seed insère en base les compétences embarquées dont le nom est encore
// libre. Les autres sont laissées telles quelles : le semis ne remplace
// JAMAIS une compétence existante, éditée ou non.
func Seed(ctx context.Context, db *persistence.DB, logger *slog.Logger) error {
	if db == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}

	defs, err := Defaults()
	if err != nil {
		return err
	}

	repo := persistence.NewSkillRepository()
	now := time.Now().UTC()
	inserted := 0
	refreshed := 0

	if err := db.WithTx(ctx, func(tx *sql.Tx) error {
		for _, def := range defs {
			// Une compétence fournie par le projet et JAMAIS éditée suit
			// les mises à jour du dépôt : sans cela, corriger une recette
			// livrée n'aurait aucun effet sur les instances déjà semées, et
			// il faudrait la restaurer à la main sur chacune.
			//
			// Dès qu'un administrateur l'a modifiée (Edited), elle est
			// figée : son travail prime toujours sur le contenu embarqué.
			existing, found, err := repo.Get(ctx, tx, def.Name)
			if err != nil {
				return err
			}
			if found {
				if existing.Builtin && !existing.Edited && existing.Content != def.Content {
					existing.Description = def.Description
					existing.Content = def.Content
					existing.Agents = def.Agents
					existing.UpdatedAt = now
					if err := repo.Upsert(ctx, tx, existing); err != nil {
						return err
					}
					refreshed++
				}

				continue
			}

			ok, err := repo.InsertIfAbsent(ctx, tx, persistence.Skill{
				Name:        def.Name,
				Description: def.Description,
				Content:     def.Content,
				Agents:      def.Agents,
				Enabled:     true,
				Builtin:     true,
				CreatedAt:   now,
				UpdatedAt:   now,
			})
			if err != nil {
				return err
			}
			if ok {
				inserted++
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("semis des compétences: %w", err)
	}

	// Compteurs seulement : le contenu d'une compétence n'a rien à faire
	// dans les journaux.
	logger.InfoContext(ctx, "skills: semées", "count", len(defs), "inserted", inserted, "refreshed", refreshed)

	return nil
}

// Provider sert le catalogue et le chargement des compétences aux agents.
// Il relit la base À CHAQUE tour : une compétence désactivée dans
// l'administration s'applique au message suivant, sans redémarrage. Pas
// de cache — la bibliothèque est petite et la lecture locale.
type Provider struct {
	db     *persistence.DB
	repo   *persistence.SkillRepository
	logger *slog.Logger
}

// NewProvider construit le fournisseur de compétences ; nil si db est
// nil (instance sans persistance : le contrat côté agent est nil-safe).
func NewProvider(db *persistence.DB, logger *slog.Logger) *Provider {
	if db == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Provider{db: db, repo: persistence.NewSkillRepository(), logger: logger}
}

// SkillsFor implémente agent.SkillsProvider : le catalogue des
// compétences actives visibles de cet agent. Une lecture en échec rend un
// catalogue vide — un tour sans catalogue vaut mieux qu'un tour en échec.
func (p *Provider) SkillsFor(ctx context.Context, agentName string) []agent.SkillSummary {
	if p == nil {
		return nil
	}

	var summaries []agent.SkillSummary
	if err := p.db.WithTx(ctx, func(tx *sql.Tx) error {
		found, err := p.repo.ListEnabledForAgent(ctx, tx, agentName)
		if err != nil {
			return err
		}
		for _, skill := range found {
			summaries = append(summaries, agent.SkillSummary{
				Name:        skill.Name,
				Description: skill.Description,
			})
		}
		return nil
	}); err != nil {
		p.logger.WarnContext(ctx, "skills: lecture du catalogue en échec",
			"agent", agentName, "error", err)
		return nil
	}

	return summaries
}

// LoadSkill implémente agent.SkillsProvider : le contenu complet d'une
// compétence. Le ciblage et l'activation sont revérifiés ici — le nom
// vient du modèle, qui désigne sans jamais décider.
func (p *Provider) LoadSkill(ctx context.Context, agentName, skillName string) (string, bool) {
	if p == nil || skillName == "" {
		return "", false
	}

	var (
		content string
		found   bool
	)
	if err := p.db.WithTx(ctx, func(tx *sql.Tx) error {
		skill, ok, err := p.repo.Get(ctx, tx, skillName)
		if err != nil {
			return err
		}
		if !ok || !skill.Enabled || !skill.TargetsAgent(agentName) {
			return nil
		}
		content, found = skill.Content, true
		return nil
	}); err != nil {
		p.logger.WarnContext(ctx, "skills: chargement en échec",
			"agent", agentName, "skill", skillName, "error", err)
		return "", false
	}

	return content, found
}

var _ agent.SkillsProvider = (*Provider)(nil)
