package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Skill est une ligne de la table skills (migration 0019) : un mode
// opératoire en markdown que les agents chargent à la demande.
//
// Content et Description partent au modèle : ils s'écrivent en anglais.
// Agents porte le ciblage — vide signifie « visible de tous les agents ».
type Skill struct {
	Name        string
	Description string
	Content     string
	Agents      []string
	Enabled     bool
	// Builtin marque une compétence fournie par le projet (semée depuis
	// les fichiers embarqués) : elle seule peut être restaurée.
	Builtin bool
	// Edited marque une compétence modifiée par un administrateur. Une
	// compétence fournie et jamais éditée suit les mises à jour du dépôt ;
	// dès qu'elle est éditée, le travail de l'administrateur prime et le
	// semis ne la touche plus.
	Edited    bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// SkillRepository donne accès à la table skills. Sans état, comme les
// autres repositories du paquet.
type SkillRepository struct{}

// NewSkillRepository crée un SkillRepository.
func NewSkillRepository() *SkillRepository {
	return &SkillRepository{}
}

// encodeAgents sérialise le ciblage. Une liste vide s'écrit "[]" : la
// colonne est NOT NULL et le format doit rester lisible en SQL.
func encodeAgents(agents []string) (string, error) {
	if len(agents) == 0 {
		return "[]", nil
	}
	raw, err := json.Marshal(agents)
	if err != nil {
		return "", fmt.Errorf("sérialisation du ciblage: %w", err)
	}
	return string(raw), nil
}

// decodeAgents est l'inverse d'encodeAgents. Une valeur illisible est
// traitée comme un ciblage vide : une compétence visible de tous vaut
// mieux qu'une lecture en échec de toute la bibliothèque.
func decodeAgents(raw string) []string {
	if raw == "" || raw == "[]" {
		return nil
	}
	var agents []string
	if err := json.Unmarshal([]byte(raw), &agents); err != nil {
		return nil
	}
	return agents
}

// Upsert enregistre une compétence, en écrasant celle qui porte le même
// nom. Le nom est la clé : renommer, c'est créer puis supprimer.
func (r *SkillRepository) Upsert(ctx context.Context, q Querier, s Skill) error {
	agents, err := encodeAgents(s.Agents)
	if err != nil {
		return fmt.Errorf("compétence %q: %w", s.Name, err)
	}

	_, err = q.ExecContext(ctx, `INSERT INTO skills
		(name, description, content, agents, enabled, builtin, edited, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			description = excluded.description,
			content = excluded.content,
			agents = excluded.agents,
			enabled = excluded.enabled,
			builtin = excluded.builtin,
			edited = excluded.edited,
			updated_at = excluded.updated_at`,
		s.Name, s.Description, s.Content, agents, boolToInt(s.Enabled), boolToInt(s.Builtin), boolToInt(s.Edited),
		formatTenantTime(s.CreatedAt), formatTenantTime(s.UpdatedAt))
	if err != nil {
		return fmt.Errorf("enregistrement de la compétence %q: %w", s.Name, err)
	}

	return nil
}

// InsertIfAbsent insère la compétence uniquement si son nom est libre, et
// indique si l'insertion a eu lieu. C'est le geste du semis : une
// compétence éditée dans l'administration ne doit JAMAIS être écrasée par
// un redéploiement.
func (r *SkillRepository) InsertIfAbsent(ctx context.Context, q Querier, s Skill) (bool, error) {
	agents, err := encodeAgents(s.Agents)
	if err != nil {
		return false, fmt.Errorf("compétence %q: %w", s.Name, err)
	}

	res, err := q.ExecContext(ctx, `INSERT INTO skills
		(name, description, content, agents, enabled, builtin, edited, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO NOTHING`,
		s.Name, s.Description, s.Content, agents, boolToInt(s.Enabled), boolToInt(s.Builtin), boolToInt(s.Edited),
		formatTenantTime(s.CreatedAt), formatTenantTime(s.UpdatedAt))
	if err != nil {
		return false, fmt.Errorf("semis de la compétence %q: %w", s.Name, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("semis de la compétence %q: %w", s.Name, err)
	}

	return affected > 0, nil
}

// Get retourne une compétence par son nom. found faux si elle n'existe
// pas.
func (r *SkillRepository) Get(ctx context.Context, q Querier, name string) (Skill, bool, error) {
	row := q.QueryRowContext(ctx, `SELECT name, description, content, agents, enabled, builtin, edited, created_at, updated_at
		FROM skills WHERE name = ?`, name)

	skill, err := scanSkill(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Skill{}, false, nil
		}
		return Skill{}, false, fmt.Errorf("lecture de la compétence %q: %w", name, err)
	}

	return skill, true, nil
}

// List retourne toutes les compétences, triées par nom.
func (r *SkillRepository) List(ctx context.Context, q Querier) ([]Skill, error) {
	rows, err := q.QueryContext(ctx, `SELECT name, description, content, agents, enabled, builtin, edited, created_at, updated_at
		FROM skills ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("liste des compétences: %w", err)
	}
	defer rows.Close()

	var skills []Skill
	for rows.Next() {
		skill, err := scanSkill(rows)
		if err != nil {
			return nil, fmt.Errorf("lecture d'une compétence: %w", err)
		}
		skills = append(skills, skill)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des compétences: %w", err)
	}

	return skills, nil
}

// ListEnabledForAgent retourne les compétences actives visibles de
// l'agent nommé, triées par nom. Le filtre de ciblage se fait en Go : la
// bibliothèque est petite, et interroger du JSON en SQL coûterait plus
// cher en complexité qu'il ne rapporte.
func (r *SkillRepository) ListEnabledForAgent(ctx context.Context, q Querier, agentName string) ([]Skill, error) {
	rows, err := q.QueryContext(ctx, `SELECT name, description, content, agents, enabled, builtin, edited, created_at, updated_at
		FROM skills WHERE enabled = 1 ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("compétences actives: %w", err)
	}
	defer rows.Close()

	var skills []Skill
	for rows.Next() {
		skill, err := scanSkill(rows)
		if err != nil {
			return nil, fmt.Errorf("lecture d'une compétence: %w", err)
		}
		if !skill.TargetsAgent(agentName) {
			continue
		}
		skills = append(skills, skill)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des compétences: %w", err)
	}

	return skills, nil
}

// TargetsAgent indique si la compétence est visible de l'agent nommé. Un
// ciblage vide vaut pour tous les agents.
func (s Skill) TargetsAgent(agentName string) bool {
	if len(s.Agents) == 0 {
		return true
	}
	for _, target := range s.Agents {
		if target == agentName {
			return true
		}
	}
	return false
}

// Delete supprime une compétence. Une compétence fournie par le projet
// sera re-semée au prochain démarrage.
func (r *SkillRepository) Delete(ctx context.Context, q Querier, name string) error {
	if _, err := q.ExecContext(ctx, `DELETE FROM skills WHERE name = ?`, name); err != nil {
		return fmt.Errorf("suppression de la compétence %q: %w", name, err)
	}
	return nil
}

// rowScanner couvre *sql.Row et *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanSkill(row rowScanner) (Skill, error) {
	var (
		skill                    Skill
		agents                   string
		enabled, builtin, edited int
		createdAt, updatedAt     string
	)

	if err := row.Scan(&skill.Name, &skill.Description, &skill.Content, &agents,
		&enabled, &builtin, &edited, &createdAt, &updatedAt); err != nil {
		return Skill{}, err
	}

	skill.Agents = decodeAgents(agents)
	skill.Enabled = enabled != 0
	skill.Builtin = builtin != 0
	skill.Edited = edited != 0

	var err error
	if skill.CreatedAt, err = parseTenantTime(createdAt); err != nil {
		return Skill{}, err
	}
	if skill.UpdatedAt, err = parseTenantTime(updatedAt); err != nil {
		return Skill{}, err
	}

	return skill, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
