// Package consolidation réorganise périodiquement la mémoire à long terme
// (internal/memory) pour que les souvenirs ne s'accumulent pas sans limite :
// à une cadence cron configurée (memory.consolidation), il liste toutes les
// mémoires, les regroupe par portée (org/scope/scope_id), et demande à un
// LLM un plan de réorganisation — fusionner les souvenirs redondants,
// oublier les faits périmés — appliqué avec des garde-fous stricts.
//
// Contrairement au scheduler (internal/scheduler), rien ici n'exécute
// d'agent ni ne livre quoi que ce soit sur un canal : c'est une tâche de
// maintenance interne. Le contenu des souvenirs est du contenu privé : il
// transite vers le LLM (comme pour toute recherche mémoire) mais n'apparaît
// JAMAIS dans les journaux (AGENTS.md).
package consolidation

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bornholm/genai/llm"
	cron "github.com/robfig/cron/v3"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/memory"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/observability"
	"github.com/bornholm/automata/internal/persistence"
)

// taskName identifie cette tâche dans la table maintenance_runs.
const taskName = "memory_consolidation"

// tickInterval est la période de vérification de l'échéance cron. Une
// minute suffit largement pour une tâche quotidienne, et reste assez fin
// pour respecter l'heure configurée.
const tickInterval = time.Minute

// defaultCron est la cadence appliquée quand memory.consolidation.cron est
// vide : chaque nuit vers 4h40, heure locale du serveur — un creux
// d'activité, décalé des heures rondes.
const defaultCron = "40 4 * * *"

// defaultMinMemories est le seuil de consolidation d'une portée quand
// memory.consolidation.min_memories vaut 0 : en dessous, la portée est
// laissée intacte sans appel LLM.
const defaultMinMemories = 10

// maxMergedChars borne la taille d'un souvenir fusionné : une fusion doit
// condenser, pas concaténer.
const maxMergedChars = 2000

// consolidatorPrincipal est enregistré comme auteur (created_by) des
// souvenirs fusionnés : il n'existe aucun principal humain derrière cette
// écriture, et le marquer explicitement permet de l'auditer.
const consolidatorPrincipal = model.PrincipalID("system/consolidation")

// consolidationPrompt encadre la production du plan de réorganisation.
const consolidationPrompt = `Tu réorganises la mémoire à long terme d'un assistant personnel. On te fournit la liste complète des souvenirs d'une même portée, chacun avec son identifiant et sa date de création.

Ton objectif : moins de souvenirs, mieux rédigés, sans aucune perte d'information durable.
- Fusionne les souvenirs redondants ou traitant du même sujet en un seul texte à jour ; en cas de contradiction, le souvenir le plus récent fait foi.
- Propose l'oubli des souvenirs sans valeur durable : demandes ponctuelles passées, états transitoires, informations manifestement périmées.
- Un souvenir sain et unique reste intact : ne le liste nulle part.
- N'invente jamais d'information absente des souvenirs fournis.

Réponds UNIQUEMENT par un objet JSON de la forme :
{"merges": [{"ids": ["id1", "id2"], "content": "texte fusionné"}], "forget": ["id3"]}
- "merges" : chaque entrée remplace AU MOINS DEUX souvenirs par le texte donné.
- "forget" : souvenirs à supprimer purement et simplement, sans remplacement.
- Un identifiant apparaît AU PLUS UNE FOIS dans l'ensemble de la réponse, et uniquement s'il figure dans la liste fournie.
Réponds {"merges": [], "forget": []} s'il n'y a rien à réorganiser.`

// scopeKey regroupe les souvenirs d'une même portée : la consolidation ne
// franchit JAMAIS une frontière de portée (PLAN.md §8.3) — fusionner un
// souvenir personnel avec un souvenir de groupe reviendrait à faire fuir de
// l'information d'un cloisonnement vers un autre.
type scopeKey struct {
	orgID   string
	scope   string
	scopeID string
}

// plan est la réponse attendue du LLM.
type plan struct {
	Merges []merge  `json:"merges"`
	Forget []string `json:"forget"`
}

type merge struct {
	IDs     []string `json:"ids"`
	Content string   `json:"content"`
}

// Consolidator exécute la réorganisation périodique. Mono-instance, comme
// tout le processus (docs/security-model.md §4).
type Consolidator struct {
	db          *persistence.DB
	store       memory.Store
	client      llm.ChatCompletionClient
	schedule    cron.Schedule
	minMemories int
	runs        *persistence.MaintenanceRunRepository
	logger      *slog.Logger
	metrics     *observability.Metrics
	now         func() time.Time
}

// New construit un Consolidator à partir de la configuration
// memory.consolidation (déjà validée par config.Validate ; l'expression
// cron est reparsée ici par défense en profondeur).
func New(db *persistence.DB, store memory.Store, client llm.ChatCompletionClient, cfg config.MemoryConsolidation, logger *slog.Logger, metrics *observability.Metrics) (*Consolidator, error) {
	expr := strings.TrimSpace(cfg.Cron)
	if expr == "" {
		expr = defaultCron
	}

	schedule, err := cron.ParseStandard(expr)
	if err != nil {
		return nil, fmt.Errorf("consolidation: expression cron %q invalide: %w", expr, err)
	}

	minMemories := cfg.MinMemories
	if minMemories <= 0 {
		minMemories = defaultMinMemories
	}

	if logger == nil {
		logger = slog.Default()
	}

	return &Consolidator{
		db:          db,
		store:       store,
		client:      client,
		schedule:    schedule,
		minMemories: minMemories,
		runs:        persistence.NewMaintenanceRunRepository(),
		logger:      logger,
		metrics:     metrics,
		now:         time.Now,
	}, nil
}

// WithClock remplace l'horloge (tests).
func (c *Consolidator) WithClock(now func() time.Time) *Consolidator {
	c.now = now
	return c
}

// Run vérifie l'échéance cron toutes les tickInterval jusqu'à l'annulation
// de ctx, et lance une consolidation quand elle est atteinte. Une erreur de
// tick est journalisée, jamais fatale : la prochaine échéance retentera.
func (c *Consolidator) Run(ctx context.Context) error {
	if err := c.Tick(ctx); err != nil && ctx.Err() == nil {
		c.logger.ErrorContext(ctx, "consolidation: échec du tick initial", "error", err)
	}

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := c.Tick(ctx); err != nil && ctx.Err() == nil {
				c.logger.ErrorContext(ctx, "consolidation: échec du tick", "error", err)
			}
		}
	}
}

// Tick lance une consolidation si l'échéance cron est dépassée depuis la
// dernière exécution enregistrée (table maintenance_runs). Au tout premier
// démarrage (aucune exécution enregistrée), l'horodatage est simplement
// initialisé à maintenant SANS consolider : la première consolidation aura
// lieu à la prochaine occurrence cron — sans cet ancrage, chaque nouveau
// déploiement consoliderait immédiatement au démarrage.
func (c *Consolidator) Tick(ctx context.Context) error {
	now := c.now()

	var (
		lastRun time.Time
		found   bool
	)
	err := c.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		lastRun, found, err = c.runs.GetLastRun(ctx, tx, taskName)
		return err
	})
	if err != nil {
		return fmt.Errorf("consolidation: lecture de la dernière exécution: %w", err)
	}

	if !found {
		return c.recordRun(ctx, now)
	}

	if c.schedule.Next(lastRun).After(now) {
		return nil
	}

	if err := c.Consolidate(ctx); err != nil {
		return err
	}

	return c.recordRun(ctx, now)
}

// recordRun enregistre at comme dernière exécution réussie.
func (c *Consolidator) recordRun(ctx context.Context, at time.Time) error {
	err := c.db.WithTx(ctx, func(tx *sql.Tx) error {
		return c.runs.SetLastRun(ctx, tx, taskName, at)
	})
	if err != nil {
		return fmt.Errorf("consolidation: enregistrement de l'exécution: %w", err)
	}
	return nil
}

// Consolidate exécute une passe complète de réorganisation, portée par
// portée. Une erreur sur une portée n'interrompt jamais les suivantes : la
// première erreur rencontrée est retournée à la fin, pour être journalisée
// par l'appelant.
func (c *Consolidator) Consolidate(ctx context.Context) error {
	all, err := c.store.List(ctx)
	if err != nil {
		return fmt.Errorf("consolidation: énumération des mémoires: %w", err)
	}

	groups := map[scopeKey][]memory.Memory{}
	for _, mem := range all {
		key := scopeKey{
			orgID:   mem.Metadata["org_id"],
			scope:   mem.Metadata["scope"],
			scopeID: mem.Metadata["scope_id"],
		}
		// Sans métadonnées de portée, impossible de garantir le
		// cloisonnement : le souvenir est laissé intact.
		if key.orgID == "" || key.scope == "" {
			continue
		}
		groups[key] = append(groups[key], mem)
	}

	var (
		firstErr           error
		total              int
		consolidatedScopes int
	)

	for key, group := range groups {
		if len(group) < c.minMemories {
			continue
		}

		removed, err := c.consolidateScope(ctx, key, group)
		if err != nil {
			// Jamais le contenu, seulement la portée et l'erreur.
			c.logger.ErrorContext(ctx, "consolidation: échec sur une portée",
				"scope", key.scope, "scope_id", key.scopeID, "error", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		consolidatedScopes++
		total += removed
	}

	c.metrics.AddMemoriesConsolidated(total)
	c.logger.InfoContext(ctx, "consolidation: passe terminée",
		"memories_total", len(all),
		"scopes", len(groups),
		"scopes_consolidated", consolidatedScopes,
		"memories_removed", total,
	)

	return firstErr
}

// consolidateScope demande un plan de réorganisation pour group et
// l'applique. Retourne le nombre de souvenirs supprimés (fusionnés ou
// oubliés).
func (c *Consolidator) consolidateScope(ctx context.Context, key scopeKey, group []memory.Memory) (int, error) {
	var b strings.Builder
	for _, mem := range group {
		fmt.Fprintf(&b, "- id: %s (créé le %s)\n  %s\n", mem.ID, mem.CreatedAt.Format("2006-01-02"), strings.ReplaceAll(mem.Content, "\n", "\n  "))
	}

	response, err := c.client.ChatCompletion(ctx,
		llm.WithMessages(
			llm.NewMessage(llm.RoleSystem, consolidationPrompt),
			llm.NewMessage(llm.RoleUser, b.String()),
		),
	)
	if err != nil {
		return 0, fmt.Errorf("appel du client llm: %w", err)
	}

	p, err := parsePlan(response.Message().Content())
	if err != nil {
		return 0, err
	}

	if err := validatePlan(p, group); err != nil {
		return 0, err
	}

	byID := make(map[string]memory.Memory, len(group))
	for _, mem := range group {
		byID[mem.ID] = mem
	}

	removed := 0

	for _, m := range p.Merges {
		content := strings.TrimSpace(m.Content)
		if runes := []rune(content); len(runes) > maxMergedChars {
			content = string(runes[:maxMergedChars])
		}

		_, err := c.store.Remember(ctx, memory.NewMemory{
			Content:          content,
			Scope:            model.Scope(key.scope),
			ScopeID:          model.ScopeID(key.scopeID),
			OrgID:            model.OrgID(key.orgID),
			OwnerPrincipalID: uniformOwner(m.IDs, byID),
			CreatedBy:        consolidatorPrincipal,
			Origin:           "consolidation",
		})
		if err != nil {
			// Le souvenir fusionné n'a pas pu être écrit : les originaux
			// sont conservés, aucune perte.
			return removed, fmt.Errorf("écriture d'un souvenir fusionné: %w", err)
		}

		for _, id := range m.IDs {
			if err := c.store.Forget(ctx, id); err != nil {
				// Doublon temporaire (l'original ET la fusion coexistent) :
				// préférable à une perte, et la prochaine passe le résorbera.
				c.logger.WarnContext(ctx, "consolidation: oubli d'un souvenir fusionné en échec",
					"scope", key.scope, "error", err)
				continue
			}
			removed++
		}
	}

	for _, id := range p.Forget {
		if err := c.store.Forget(ctx, id); err != nil {
			c.logger.WarnContext(ctx, "consolidation: oubli d'un souvenir en échec",
				"scope", key.scope, "error", err)
			continue
		}
		removed++
	}

	return removed, nil
}

// validatePlan applique les garde-fous : identifiants connus et uniques,
// fusions d'au moins deux souvenirs avec un contenu non vide, et oublis
// secs bornés au tiers de la portée — un LLM qui propose de vider la
// mémoire est un LLM qui se trompe, jamais un ordre à exécuter.
func validatePlan(p plan, group []memory.Memory) error {
	known := make(map[string]bool, len(group))
	for _, mem := range group {
		known[mem.ID] = true
	}

	seen := map[string]bool{}
	claim := func(id string) error {
		if !known[id] {
			return fmt.Errorf("plan de consolidation invalide: identifiant inconnu %q", id)
		}
		if seen[id] {
			return fmt.Errorf("plan de consolidation invalide: identifiant %q utilisé deux fois", id)
		}
		seen[id] = true
		return nil
	}

	for _, m := range p.Merges {
		if len(m.IDs) < 2 {
			return fmt.Errorf("plan de consolidation invalide: une fusion doit porter sur au moins deux souvenirs")
		}
		if strings.TrimSpace(m.Content) == "" {
			return fmt.Errorf("plan de consolidation invalide: fusion sans contenu")
		}
		for _, id := range m.IDs {
			if err := claim(id); err != nil {
				return err
			}
		}
	}

	maxForgets := max(1, len(group)/3)
	if len(p.Forget) > maxForgets {
		return fmt.Errorf("plan de consolidation refusé: %d oublis secs proposés, maximum %d pour %d souvenirs", len(p.Forget), maxForgets, len(group))
	}
	for _, id := range p.Forget {
		if err := claim(id); err != nil {
			return err
		}
	}

	return nil
}

// uniformOwner retourne le propriétaire commun des souvenirs fusionnés,
// ou une valeur vide s'ils appartiennent à des principaux différents : la
// fusion appartient alors à la portée elle-même, pas à un individu.
func uniformOwner(ids []string, byID map[string]memory.Memory) model.PrincipalID {
	owner := ""
	for i, id := range ids {
		current := byID[id].Metadata["owner_principal_id"]
		if i == 0 {
			owner = current
			continue
		}
		if current != owner {
			return ""
		}
	}
	return model.PrincipalID(owner)
}

// parsePlan décode la réponse du LLM, en tolérant un bloc de code Markdown
// autour du JSON.
func parsePlan(raw string) (plan, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	var p plan
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return plan{}, fmt.Errorf("plan de consolidation illisible: %w", err)
	}

	return p, nil
}
