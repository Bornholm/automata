package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/bornholm/automata/internal/secretbox"
)

// Missions : les dossiers au long cours (migration 0028, internal/mission).
//
// Le journal de bord est TOUT l'état d'une mission : c'est lui que l'agent
// relit à chaque réveil et complète avant de se rendormir. Il est borné —
// une mission n'accumule pas un procès-verbal, elle tient des notes — et
// chiffré au repos, comme l'objectif, dès que la clé de contenu existe.

// Statuts d'une mission.
const (
	// MissionStatusActive : suivie, un prochain réveil est programmé.
	MissionStatusActive = "active"
	// MissionStatusDone : l'objectif est atteint, close par l'agent.
	MissionStatusDone = "done"
	// MissionStatusAbandoned : close par la personne.
	MissionStatusAbandoned = "abandoned"
)

// MaxJournalChars borne le journal de bord. La troncature retire le DÉBUT :
// les notes récentes sont celles qui portent l'état courant du dossier.
const MaxJournalChars = 4000

// MaxActiveMissionsPerPrincipal borne les missions actives d'une personne :
// chaque mission coûte un tour de modèle par réveil.
const MaxActiveMissionsPerPrincipal = 10

// Mission est une ligne de la table missions.
type Mission struct {
	ID             string
	OrgID          string
	PrincipalID    string
	ConversationID string
	Provider       string
	ChannelID      string
	AgentID        string
	// Title reste en clair : il s'affiche dans des listes.
	Title string
	// Objective est le mandat, immuable après création.
	Objective string
	// Journal est le journal de bord, réécrit par l'agent.
	Journal string
	Status  string
	// NextCheckAt zéro = mission close.
	NextCheckAt time.Time
	// Attempts compte les réveils en échec consécutifs (backoff).
	Attempts  int
	LastRunAt time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// MissionRepository gère les missions. cipher peut être nil (pas de clé de
// chiffrement configurée) : les contenus sont alors en clair, comme les
// autres contenus personnels.
type MissionRepository struct {
	cipher *secretbox.Box
}

// NewMissionRepository crée un MissionRepository.
func NewMissionRepository(cipher *secretbox.Box) *MissionRepository {
	return &MissionRepository{cipher: cipher}
}

const missionColumns = `id, org_id, principal_id, conversation_id, provider, channel_id,
	agent_id, title, objective, journal, status, next_check_at, attempts,
	last_run_at, created_at, updated_at`

// Insert enregistre une mission.
func (r *MissionRepository) Insert(ctx context.Context, q Querier, m Mission) error {
	objective, err := sealContent(r.cipher, m.Objective)
	if err != nil {
		return err
	}
	journal, err := sealContent(r.cipher, m.Journal)
	if err != nil {
		return err
	}

	_, err = q.ExecContext(ctx, `INSERT INTO missions (`+missionColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.OrgID, m.PrincipalID, m.ConversationID, m.Provider, m.ChannelID,
		m.AgentID, m.Title, objective, journal, m.Status,
		formatTenantTime(m.NextCheckAt), m.Attempts,
		formatTenantTime(m.LastRunAt), formatTenantTime(m.CreatedAt), formatTenantTime(m.UpdatedAt))
	if err != nil {
		return fmt.Errorf("enregistrement de la mission %q: %w", m.ID, err)
	}

	return nil
}

// FindByID retourne une mission, ou found=false.
func (r *MissionRepository) FindByID(ctx context.Context, q Querier, id string) (Mission, bool, error) {
	row := q.QueryRowContext(ctx, `SELECT `+missionColumns+` FROM missions WHERE id = ?`, id)

	mission, err := r.scan(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Mission{}, false, nil
	}
	if err != nil {
		return Mission{}, false, fmt.Errorf("lecture de la mission %q: %w", id, err)
	}

	return mission, true, nil
}

// ListDue retourne les missions actives dont l'échéance est passée, la plus
// ancienne d'abord.
func (r *MissionRepository) ListDue(ctx context.Context, q Querier, now time.Time, limit int) ([]Mission, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+missionColumns+` FROM missions
		WHERE status = ? AND next_check_at != '' AND next_check_at <= ?
		ORDER BY next_check_at LIMIT ?`,
		MissionStatusActive, formatTenantTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("missions échues: %w", err)
	}
	defer rows.Close()

	return r.collect(rows)
}

// ListByConversation retourne les missions d'une conversation, actives
// d'abord — le cloisonnement des outils conversationnels, comme pour les
// tâches planifiées.
func (r *MissionRepository) ListByConversation(ctx context.Context, q Querier, conversationID string, limit int) ([]Mission, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+missionColumns+` FROM missions
		WHERE conversation_id = ?
		ORDER BY CASE status WHEN 'active' THEN 0 ELSE 1 END, updated_at DESC
		LIMIT ?`, conversationID, limit)
	if err != nil {
		return nil, fmt.Errorf("missions de la conversation: %w", err)
	}
	defer rows.Close()

	return r.collect(rows)
}

// ListByMember retourne les missions d'un membre — l'onglet du profil.
func (r *MissionRepository) ListByMember(ctx context.Context, q Querier, orgID, principalID string, limit int) ([]Mission, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+missionColumns+` FROM missions
		WHERE org_id = ? AND principal_id = ?
		ORDER BY CASE status WHEN 'active' THEN 0 ELSE 1 END, updated_at DESC
		LIMIT ?`, orgID, principalID, limit)
	if err != nil {
		return nil, fmt.Errorf("missions du membre %q: %w", principalID, err)
	}
	defer rows.Close()

	return r.collect(rows)
}

// CountActiveByPrincipal compte les missions actives d'une personne, pour
// la borne de création.
func (r *MissionRepository) CountActiveByPrincipal(ctx context.Context, q Querier, orgID, principalID string) (int, error) {
	var count int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM missions
		WHERE org_id = ? AND principal_id = ? AND status = ?`,
		orgID, principalID, MissionStatusActive).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("comptage des missions de %q: %w", principalID, err)
	}

	return count, nil
}

// UpdateJournal écrit en une fois ce qu'un réveil laisse derrière lui :
// journal complet, statut, prochaine échéance, compteur d'échecs remis à
// zéro ou incrémenté, horodatage d'exécution.
func (r *MissionRepository) UpdateJournal(ctx context.Context, q Querier, id, journal, status string, nextCheckAt time.Time, attempts int, now time.Time) error {
	sealed, err := sealContent(r.cipher, journal)
	if err != nil {
		return err
	}

	result, err := q.ExecContext(ctx, `UPDATE missions
		SET journal = ?, status = ?, next_check_at = ?, attempts = ?,
			last_run_at = ?, updated_at = ?
		WHERE id = ?`,
		sealed, status, formatTenantTime(nextCheckAt), attempts,
		formatTenantTime(now), formatTenantTime(now), id)
	if err != nil {
		return fmt.Errorf("journal de la mission %q: %w", id, err)
	}
	if n, err := result.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("mission %q introuvable", id)
	}

	return nil
}

// UpdateStatus clôt ou rouvre une mission D'UN MEMBRE PRÉCIS — le filtre
// (org, principal) est le cloisonnement de l'onglet du profil. Retourne
// false si rien ne correspondait.
func (r *MissionRepository) UpdateStatus(ctx context.Context, q Querier, orgID, principalID, id, status string, now time.Time) (bool, error) {
	nextCheck := ""
	if status == MissionStatusActive {
		nextCheck = formatTenantTime(now)
	}

	result, err := q.ExecContext(ctx, `UPDATE missions
		SET status = ?, next_check_at = ?, updated_at = ?
		WHERE id = ? AND org_id = ? AND principal_id = ?`,
		status, nextCheck, formatTenantTime(now), id, orgID, principalID)
	if err != nil {
		return false, fmt.Errorf("statut de la mission %q: %w", id, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("statut de la mission %q: %w", id, err)
	}

	return affected > 0, nil
}

func (r *MissionRepository) collect(rows *sql.Rows) ([]Mission, error) {
	var missions []Mission
	for rows.Next() {
		mission, err := r.scan(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("lecture d'une mission: %w", err)
		}
		missions = append(missions, mission)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des missions: %w", err)
	}

	return missions, nil
}

func (r *MissionRepository) scan(scan func(...any) error) (Mission, error) {
	var (
		m                                            Mission
		nextCheckAt, lastRunAt, createdAt, updatedAt string
	)
	if err := scan(&m.ID, &m.OrgID, &m.PrincipalID, &m.ConversationID, &m.Provider,
		&m.ChannelID, &m.AgentID, &m.Title, &m.Objective, &m.Journal, &m.Status,
		&nextCheckAt, &m.Attempts, &lastRunAt, &createdAt, &updatedAt); err != nil {
		return Mission{}, err
	}

	var err error
	if m.Objective, err = openContent(r.cipher, m.Objective); err != nil {
		return Mission{}, err
	}
	if m.Journal, err = openContent(r.cipher, m.Journal); err != nil {
		return Mission{}, err
	}
	if m.NextCheckAt, err = parseTenantTime(nextCheckAt); err != nil {
		return Mission{}, err
	}
	if m.LastRunAt, err = parseTenantTime(lastRunAt); err != nil {
		return Mission{}, err
	}
	if m.CreatedAt, err = parseTenantTime(createdAt); err != nil {
		return Mission{}, err
	}
	if m.UpdatedAt, err = parseTenantTime(updatedAt); err != nil {
		return Mission{}, err
	}

	return m, nil
}
