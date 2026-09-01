package introspection

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/bornholm/automata/internal/memory"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
)

// La collecte : composer, pour un membre, le dossier que la passe soumettra
// au modèle. Tout vient de tables déjà tenues par ailleurs — l'introspection
// n'instrumente rien, elle relit.
//
// RÈGLE DE CONFIDENTIALITÉ, la plus importante du paquet : le dossier ne
// porte JAMAIS de verbatim de conversation. Les frictions y entrent en
// métadonnées (outil, permission, statut, date) — le résumé d'un plan
// d'actions, écrit par un modèle à partir de la conversation, reste dehors.
// Les seuls textes libres sont les motifs comportementaux, eux-mêmes
// produits par la réflexion épisodique sous ses propres règles de prudence,
// et les suggestions déjà émises, écrites par cette passe.

// collectWindow est la fenêtre de relecture des frictions.
const collectWindow = 30 * 24 * time.Hour

// historyWindow est la fenêtre de non-répétition : les suggestions émises
// dans cet intervalle sont montrées au modèle avec interdiction d'en
// reproduire une proche. Alignée sur la rétention de la table.
const historyWindow = 90 * 24 * time.Hour

// maxDossierItems borne chaque rubrique du dossier : au-delà, le volume
// n'apporte plus d'information, seulement des tokens.
const maxDossierItems = 20

// friction est une trace exploitable, réduite à ses métadonnées.
type friction struct {
	// Kind : plan_expired | plan_failed | reminder_failed | task_failed.
	Kind string
	// Detail nomme l'outil, la permission ou le code d'erreur — jamais un
	// contenu.
	Detail string
	At     time.Time
}

// dossier est la matière soumise au modèle pour un membre.
type dossier struct {
	Frictions []friction
	// Patterns : motifs comportementaux déjà en mémoire (Origin
	// reflection/episode_reflection), du texte produit par un LLM.
	Patterns []string
	// Previous : titres et statuts des suggestions déjà émises, pour
	// interdire la répétition.
	Previous []string
}

// empty indique qu'il n'y a aucune matière : ni friction, ni motif. Un
// dossier vide ne part jamais au modèle.
func (d dossier) empty() bool {
	return len(d.Frictions) == 0 && len(d.Patterns) == 0
}

// collect compose le dossier d'un membre.
func (i *Introspector) collect(ctx context.Context, member persistence.Member) (dossier, error) {
	var d dossier
	now := i.now().UTC()
	since := now.Add(-collectWindow)

	err := i.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		if d.Frictions, err = collectFrictions(ctx, tx, member, since); err != nil {
			return err
		}
		d.Previous, err = collectPrevious(ctx, tx, i.suggestions, member, now.Add(-historyWindow))
		return err
	})
	if err != nil {
		return dossier{}, err
	}

	// Les motifs vivent dans la mémoire, hors base applicative.
	if i.memory != nil {
		memories, err := i.memory.ListByScope(ctx, model.OrgID(member.OrgID),
			model.ScopePersonal, model.ScopeID(member.ID))
		if err != nil {
			// Une mémoire indisponible ne condamne pas la passe : les
			// frictions suffisent souvent.
			i.logger.WarnContext(ctx, "introspection: motifs illisibles",
				"org_id", member.OrgID, "member_id", member.ID, "error", err)
		}
		for _, m := range memories {
			origin := m.Metadata["origin"]
			if origin != "reflection" && origin != "episode_reflection" {
				continue
			}
			d.Patterns = append(d.Patterns, m.Content)
			if len(d.Patterns) >= maxDossierItems {
				break
			}
		}
	}

	return d, nil
}

// collectFrictions relit les trois traces persistées.
//
// Les requêtes sont directes plutôt que des méthodes de repository : chacune
// ne sert que ce paquet, et n'extrait volontairement AUCUNE colonne de
// contenu — le SELECT est la frontière de confidentialité, autant qu'elle
// se voie ici.
func collectFrictions(ctx context.Context, tx *sql.Tx, member persistence.Member, since time.Time) ([]friction, error) {
	var frictions []friction
	sinceStr := since.Format(time.RFC3339)

	// Plans d'actions proposés par le membre et jamais aboutis. tool_name
	// et required_permission décrivent le geste sans révéler le contenu.
	rows, err := tx.QueryContext(ctx, `
		SELECT p.status, a.tool_name, a.required_permission, p.created_at
		FROM action_plans p
		JOIN actions a ON a.plan_id = p.id AND a.position = 0
		WHERE p.org_id = ? AND p.created_by = ? AND p.created_at >= ?
			AND p.status IN ('expired', 'failed')
		ORDER BY p.created_at DESC LIMIT ?`,
		member.OrgID, member.ID, sinceStr, maxDossierItems)
	if err != nil {
		return nil, fmt.Errorf("plans en friction: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var status, tool, permission, createdAt string
		if err := rows.Scan(&status, &tool, &permission, &createdAt); err != nil {
			return nil, fmt.Errorf("lecture d'un plan: %w", err)
		}
		at, _ := time.Parse(time.RFC3339, createdAt)
		frictions = append(frictions, friction{
			Kind:   "plan_" + status,
			Detail: tool + " (" + permission + ")",
			At:     at,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des plans: %w", err)
	}

	// Rappels et tâches du membre en échec. kind distingue les deux ; le
	// message, contenu privé, n'est pas lu.
	remRows, err := tx.QueryContext(ctx, `
		SELECT kind, recurrence, created_at FROM reminders
		WHERE org_id = ? AND principal_id = ? AND status = 'failed' AND created_at >= ?
		ORDER BY created_at DESC LIMIT ?`,
		member.OrgID, member.ID, sinceStr, maxDossierItems)
	if err != nil {
		return nil, fmt.Errorf("rappels en échec: %w", err)
	}
	defer remRows.Close()
	for remRows.Next() {
		var kind, createdAt string
		var recurrence sql.NullString
		if err := remRows.Scan(&kind, &recurrence, &createdAt); err != nil {
			return nil, fmt.Errorf("lecture d'un rappel: %w", err)
		}
		at, _ := time.Parse(time.RFC3339, createdAt)
		detail := "one-shot"
		if recurrence.Valid && recurrence.String != "" {
			detail = "recurring"
		}
		frictions = append(frictions, friction{Kind: kind + "_failed", Detail: detail, At: at})
	}
	if err := remRows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des rappels: %w", err)
	}

	return frictions, nil
}

// collectPrevious liste les suggestions déjà émises, avec leur sort : un
// refus est une information aussi précieuse qu'une acceptation.
func collectPrevious(ctx context.Context, tx *sql.Tx, repo *persistence.SuggestionRepository, member persistence.Member, since time.Time) ([]string, error) {
	suggestions, err := repo.ListByMember(ctx, tx, member.OrgID, member.ID, maxDossierItems)
	if err != nil {
		return nil, err
	}

	var previous []string
	for _, s := range suggestions {
		if s.CreatedAt.Before(since) {
			continue
		}
		previous = append(previous, s.Title+" — "+s.Status)
	}

	return previous, nil
}

// memoryReader est la vue de la mémoire dont la collecte a besoin.
type memoryReader interface {
	ListByScope(ctx context.Context, orgID model.OrgID, scope model.Scope, scopeID model.ScopeID) ([]memory.Memory, error)
}
