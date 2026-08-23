package consolidation

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/memory"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/usage"
)

// La réflexion épisodique est la seconde phase de la passe nocturne : là où
// la consolidation réorganise des faits déjà extraits, la réflexion relit
// les épisodes VERBATIM récents (internal/memory/episode_store.go), portée
// par portée, pour en dégager des motifs récurrents que personne n'a jamais
// énoncés — une habitude installée, une préférence implicite. C'est le seul
// mécanisme capable de les voir : l'extraction de faits à la compaction ne
// voit qu'un fragment à la fois, et les insights de consolidation ne voient
// que les faits qui ont survécu à ce premier filtre.
//
// Les épisodes sont lus, JAMAIS modifiés : l'invariant « la consolidation
// ne réécrit pas de verbatim » survit tel quel. La seule suppression est la
// purge de rétention, et elle ne touche jamais un épisode qu'aucune
// réflexion réussie n'a couvert — consolider avant d'oublier.
//
// Comme pour la consolidation, le contenu des épisodes transite vers le LLM
// mais n'apparaît JAMAIS dans les journaux (AGENTS.md).

// reflectionTaskPrefix préfixe les entrées de maintenance_runs de la
// réflexion. Contrairement à la consolidation (une seule entrée), l'ancrage
// est PAR PORTÉE : une portée qui n'a pas encore accumulé assez d'épisodes
// ne doit pas voir sa fenêtre avancer, sinon ses épisodes ne seraient
// jamais réfléchis — un motif se construit précisément en accumulant des
// occurrences à travers plusieurs fenêtres.
const reflectionTaskPrefix = "memory_reflection:"

// defaultMinEpisodes est le seuil de réflexion d'une portée quand
// memory.consolidation.reflection.min_episodes vaut 0 : en dessous, la
// portée attend d'accumuler davantage de matière, sans appel LLM.
const defaultMinEpisodes = 5

// maxReflectionEpisodes borne le nombre d'épisodes soumis au LLM par portée
// et par passe : du verbatim, la matière la plus chère en tokens de toute
// la maintenance. Les plus ANCIENS d'abord — l'ancrage n'avance que
// jusqu'au dernier épisode réellement soumis, le retard se résorbe donc
// passe après passe sans jamais sauter d'épisode.
const maxReflectionEpisodes = 20

// maxReflectionPatterns borne le nombre de motifs qu'une passe peut
// mémoriser par portée : même parcimonie que maxInsights, pour la même
// raison — la réflexion est spéculative par nature, elle doit rester
// marginale face aux faits.
const maxReflectionPatterns = 2

// maxPatternChars borne la taille d'un motif mémorisé, alignée sur la borne
// des faits extraits à la compaction (maxFactChars) : un motif est un fait
// comportemental, pas un résumé.
const maxPatternChars = 500

// reflectionPrompt encadre l'observation des motifs. Comme
// consolidationPrompt, seuls des contenus déjà confiés au LLM en
// conversation transitent ici.
const reflectionPrompt = `Tu observes des fragments verbatim de conversations entre un assistant personnel et ses utilisateurs, dans une même portée, pour y repérer des habitudes ou préférences récurrentes que personne n'a jamais énoncées explicitement.

Règles strictes :
- Ne retiens un motif que s'il se manifeste dans AU MOINS TROIS fragments distincts.
- Formule chaque motif prudemment, au conditionnel ou avec « semble » (« semble préférer des réponses courtes », « aurait l'habitude de planifier ses journées le dimanche soir »), à la troisième personne, autonome et compréhensible sans aucun contexte.
- N'invente rien : chaque motif doit être directement observable dans les fragments fournis.
- Ignore les demandes ponctuelles, les états passagers, le simple fil des échanges, et tout ce qui est déjà énoncé explicitement comme un fait — ces faits sont extraits par un autre mécanisme.
- Jamais de généralisation à partir d'un fait isolé : au moindre doute, aucun motif.

Réponds UNIQUEMENT par un tableau JSON de chaînes de caractères (au plus 2 motifs), ou [] si aucun motif ne se dégage. Aucun commentaire, aucun balisage.`

// Reflect exécute une passe de réflexion épisodique, portée par portée.
// Même contrat d'erreur que Consolidate : une erreur sur une portée
// n'interrompt jamais les suivantes, la première rencontrée est retournée à
// la fin. No-op si la réflexion n'est pas câblée (WithEpisodes absent).
func (c *Consolidator) Reflect(ctx context.Context) error {
	if c.episodes == nil {
		return nil
	}

	all, err := c.episodes.ListEpisodes(ctx)
	if err != nil {
		return fmt.Errorf("réflexion: énumération des épisodes: %w", err)
	}

	groups := map[scopeKey][]memory.Episode{}
	for _, ep := range all {
		// La mémoire organisationnelle n'est jamais alimentée par un
		// mécanisme automatique (même règle que l'extraction de faits) : la
		// réflexion ne regarde que les portées personnelles et de groupe.
		if ep.Scope != model.ScopePersonal && ep.Scope != model.ScopeGroup {
			continue
		}
		key := scopeKey{orgID: string(ep.OrgID), scope: string(ep.Scope), scopeID: string(ep.ScopeID)}
		// Sans portée complète, impossible de garantir le cloisonnement :
		// l'épisode est ignoré (et jamais purgé, faute d'ancrage).
		if key.orgID == "" || key.scope == "" {
			continue
		}
		groups[key] = append(groups[key], ep)
	}

	var (
		firstErr        error
		reflectedScopes int
		patternsTotal   int
		purgedTotal     int
	)

	for key, group := range groups {
		anchor, found, err := c.reflectionAnchor(ctx, key)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		sort.Slice(group, func(i, j int) bool {
			return group[i].CreatedAt.Before(group[j].CreatedAt)
		})

		// Fenêtre de la passe : les épisodes strictement postérieurs au
		// dernier ancrage, les plus anciens d'abord, bornés en nombre.
		var window []memory.Episode
		for _, ep := range group {
			if found && !ep.CreatedAt.After(anchor) {
				continue
			}
			window = append(window, ep)
			if len(window) == maxReflectionEpisodes {
				break
			}
		}

		if len(window) >= c.minEpisodes {
			patterns, err := c.reflectScope(ctx, key, window)
			if err != nil {
				// Jamais le contenu, seulement la portée et l'erreur.
				c.logger.ErrorContext(ctx, "réflexion: échec sur une portée",
					"scope", key.scope, "scope_id", key.scopeID, "error", err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}

			// L'ancrage n'avance que jusqu'au dernier épisode réellement
			// soumis : ce qui n'a pas été vu reste dans la prochaine
			// fenêtre.
			anchor = window[len(window)-1].CreatedAt
			if err := c.recordReflectionAnchor(ctx, key, anchor); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			found = true

			reflectedScopes++
			patternsTotal += patterns
		}

		// Purge de rétention : uniquement des épisodes déjà couverts par
		// une réflexion réussie (jamais au-delà de l'ancrage), et plus
		// vieux que la rétention configurée.
		if c.retention > 0 && found {
			purgedTotal += c.purgeScope(ctx, key, group, anchor)
		}
	}

	c.metrics.AddEpisodePatterns(patternsTotal)
	c.metrics.AddEpisodesPurged(purgedTotal)
	c.logger.InfoContext(ctx, "réflexion: passe terminée",
		"episodes_total", len(all),
		"scopes", len(groups),
		"scopes_reflected", reflectedScopes,
		"patterns", patternsTotal,
		"episodes_purged", purgedTotal,
	)

	return firstErr
}

// reflectScope soumet la fenêtre d'épisodes de key au LLM et mémorise les
// motifs retenus. Retourne le nombre de motifs écrits.
func (c *Consolidator) reflectScope(ctx context.Context, key scopeKey, window []memory.Episode) (int, error) {
	// Comptabilité d'usage : comme la consolidation, la réflexion nocturne
	// est facturée à l'organisation de la portée traitée.
	ctx = usage.ContextWithAttribution(ctx, usage.Attribution{
		OrgID:     key.orgID,
		Component: usage.ComponentReflection,
	})

	var b strings.Builder
	for i, ep := range window {
		fmt.Fprintf(&b, "--- Fragment %d (du %s au %s) ---\n%s\n\n",
			i+1, ep.From.Format("2006-01-02"), ep.To.Format("2006-01-02"), ep.Content)
	}

	response, err := c.client.ChatCompletion(ctx,
		llm.WithMessages(
			llm.NewMessage(llm.RoleSystem, reflectionPrompt),
			llm.NewMessage(llm.RoleUser, b.String()),
		),
	)
	if err != nil {
		return 0, fmt.Errorf("appel du client llm: %w", err)
	}

	patterns, err := parsePatterns(response.Message().Content())
	if err != nil {
		return 0, err
	}

	if len(patterns) > maxReflectionPatterns {
		return 0, fmt.Errorf("réflexion refusée: %d motifs proposés, maximum %d", len(patterns), maxReflectionPatterns)
	}

	written := 0
	for _, pattern := range patterns {
		content := strings.TrimSpace(pattern)
		if content == "" {
			return written, fmt.Errorf("réflexion invalide: motif sans contenu")
		}
		if runes := []rune(content); len(runes) > maxPatternChars {
			content = string(runes[:maxPatternChars])
		}

		_, err := c.store.Remember(ctx, memory.NewMemory{
			Content: content,
			Scope:   model.Scope(key.scope),
			ScopeID: model.ScopeID(key.scopeID),
			OrgID:   model.OrgID(key.orgID),
			// Un motif appartient à la portée observée, pas à un individu.
			CreatedBy: consolidatorPrincipal,
			Origin:    "episode_reflection",
		})
		if err != nil {
			// Les épisodes sources sont intacts : un motif perdu n'est
			// qu'une occasion manquée, pas une perte.
			c.logger.WarnContext(ctx, "réflexion: écriture d'un motif en échec",
				"scope", key.scope, "error", err)
			continue
		}
		written++
	}

	return written, nil
}

// purgeScope supprime les épisodes de group plus vieux que la rétention ET
// couverts par l'ancrage (jamais un épisode qu'aucune réflexion réussie n'a
// vu). Retourne le nombre d'épisodes supprimés ; un échec de suppression
// isolé est journalisé sans interrompre la purge.
func (c *Consolidator) purgeScope(ctx context.Context, key scopeKey, group []memory.Episode, anchor time.Time) int {
	cutoff := c.now().Add(-time.Duration(c.retention) * 24 * time.Hour)

	purged := 0
	for _, ep := range group {
		if ep.CreatedAt.IsZero() || ep.CreatedAt.After(anchor) || !ep.CreatedAt.Before(cutoff) {
			continue
		}
		if err := c.episodes.ForgetEpisode(ctx, ep.ID); err != nil {
			c.logger.WarnContext(ctx, "réflexion: purge d'un épisode en échec",
				"scope", key.scope, "error", err)
			continue
		}
		purged++
	}

	return purged
}

// reflectionAnchor lit l'ancrage de réflexion de la portée key : l'horodatage
// de création du dernier épisode couvert par une réflexion réussie.
func (c *Consolidator) reflectionAnchor(ctx context.Context, key scopeKey) (time.Time, bool, error) {
	var (
		anchor time.Time
		found  bool
	)
	err := c.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		anchor, found, err = c.runs.GetLastRun(ctx, tx, reflectionTask(key))
		return err
	})
	if err != nil {
		return time.Time{}, false, fmt.Errorf("réflexion: lecture de l'ancrage de %s/%s: %w", key.scope, key.scopeID, err)
	}
	return anchor, found, nil
}

// recordReflectionAnchor enregistre at comme nouvel ancrage de la portée key.
func (c *Consolidator) recordReflectionAnchor(ctx context.Context, key scopeKey, at time.Time) error {
	err := c.db.WithTx(ctx, func(tx *sql.Tx) error {
		return c.runs.SetLastRun(ctx, tx, reflectionTask(key), at)
	})
	if err != nil {
		return fmt.Errorf("réflexion: enregistrement de l'ancrage de %s/%s: %w", key.scope, key.scopeID, err)
	}
	return nil
}

// reflectionTask construit le nom de tâche maintenance_runs de la portée key.
func reflectionTask(key scopeKey) string {
	return reflectionTaskPrefix + key.orgID + "/" + key.scope + "/" + key.scopeID
}

// parsePatterns décode la réponse du LLM en liste de motifs, en tolérant un
// bloc de code Markdown autour du JSON.
func parsePatterns(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	var patterns []string
	if err := json.Unmarshal([]byte(raw), &patterns); err != nil {
		return nil, fmt.Errorf("réflexion illisible: %w", err)
	}

	return patterns, nil
}
