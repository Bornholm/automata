package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Repositories du magasin d'objets des plugins (migration 0021). Les
// objets ne sont pas scellés au repos : le magasin porte du contenu
// destiné à être servi publiquement, jamais de secret.

// PluginObject est une ligne de plugin_objects.
type PluginObject struct {
	PluginName  string
	OrgID       string
	MemberID    string
	Collection  string
	Key         string
	ContentType string
	Size        int64
	Data        []byte
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// PluginObjectMeta décrit un objet sans son contenu.
type PluginObjectMeta struct {
	Key         string
	ContentType string
	Size        int64
	UpdatedAt   time.Time
}

// PluginObjectRepository gère les objets déposés par les plugins.
type PluginObjectRepository struct{}

// NewPluginObjectRepository crée un PluginObjectRepository.
func NewPluginObjectRepository() *PluginObjectRepository {
	return &PluginObjectRepository{}
}

// Upsert enregistre un objet, en préservant created_at si l'objet existe.
func (r *PluginObjectRepository) Upsert(ctx context.Context, q Querier, o PluginObject) error {
	_, err := q.ExecContext(ctx, `INSERT INTO plugin_objects
		(plugin_name, org_id, member_id, collection, key, content_type, size, data, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(plugin_name, org_id, member_id, collection, key) DO UPDATE SET
			content_type = excluded.content_type, size = excluded.size,
			data = excluded.data, updated_at = excluded.updated_at`,
		o.PluginName, o.OrgID, o.MemberID, o.Collection, o.Key,
		o.ContentType, o.Size, o.Data, formatTenantTime(o.CreatedAt), formatTenantTime(o.UpdatedAt))
	if err != nil {
		return fmt.Errorf("écriture de l'objet %q/%q du plugin %q: %w", o.Collection, o.Key, o.PluginName, err)
	}

	return nil
}

// Get retourne un objet avec son contenu, ou found=false.
func (r *PluginObjectRepository) Get(ctx context.Context, q Querier, pluginName, orgID, memberID, collection, key string) (PluginObject, bool, error) {
	row := q.QueryRowContext(ctx, `SELECT plugin_name, org_id, member_id, collection, key,
			content_type, size, data, created_at, updated_at
		FROM plugin_objects
		WHERE plugin_name = ? AND org_id = ? AND member_id = ? AND collection = ? AND key = ?`,
		pluginName, orgID, memberID, collection, key)

	var (
		o                    PluginObject
		createdAt, updatedAt string
	)
	if err := row.Scan(&o.PluginName, &o.OrgID, &o.MemberID, &o.Collection, &o.Key,
		&o.ContentType, &o.Size, &o.Data, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PluginObject{}, false, nil
		}
		return PluginObject{}, false, fmt.Errorf("lecture de l'objet %q/%q du plugin %q: %w", collection, key, pluginName, err)
	}

	var err error
	if o.CreatedAt, err = parseTenantTime(createdAt); err != nil {
		return PluginObject{}, false, err
	}
	if o.UpdatedAt, err = parseTenantTime(updatedAt); err != nil {
		return PluginObject{}, false, err
	}

	return o, true, nil
}

// Delete supprime un objet et indique s'il existait.
func (r *PluginObjectRepository) Delete(ctx context.Context, q Querier, pluginName, orgID, memberID, collection, key string) (bool, error) {
	result, err := q.ExecContext(ctx, `DELETE FROM plugin_objects
		WHERE plugin_name = ? AND org_id = ? AND member_id = ? AND collection = ? AND key = ?`,
		pluginName, orgID, memberID, collection, key)
	if err != nil {
		return false, fmt.Errorf("suppression de l'objet %q/%q du plugin %q: %w", collection, key, pluginName, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("suppression de l'objet %q/%q du plugin %q: %w", collection, key, pluginName, err)
	}

	return affected > 0, nil
}

// DeleteCollection supprime tous les objets d'une collection.
func (r *PluginObjectRepository) DeleteCollection(ctx context.Context, q Querier, pluginName, orgID, memberID, collection string) (int64, error) {
	result, err := q.ExecContext(ctx, `DELETE FROM plugin_objects
		WHERE plugin_name = ? AND org_id = ? AND member_id = ? AND collection = ?`,
		pluginName, orgID, memberID, collection)
	if err != nil {
		return 0, fmt.Errorf("suppression de la collection %q du plugin %q: %w", collection, pluginName, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("suppression de la collection %q du plugin %q: %w", collection, pluginName, err)
	}

	return affected, nil
}

// List retourne les métadonnées des objets d'une collection, sans leur
// contenu, triées par clé.
func (r *PluginObjectRepository) List(ctx context.Context, q Querier, pluginName, orgID, memberID, collection string) ([]PluginObjectMeta, error) {
	rows, err := q.QueryContext(ctx, `SELECT key, content_type, size, updated_at
		FROM plugin_objects
		WHERE plugin_name = ? AND org_id = ? AND member_id = ? AND collection = ?
		ORDER BY key`, pluginName, orgID, memberID, collection)
	if err != nil {
		return nil, fmt.Errorf("objets de la collection %q du plugin %q: %w", collection, pluginName, err)
	}
	defer rows.Close()

	var metas []PluginObjectMeta
	for rows.Next() {
		var (
			m         PluginObjectMeta
			updatedAt string
		)
		if err := rows.Scan(&m.Key, &m.ContentType, &m.Size, &updatedAt); err != nil {
			return nil, fmt.Errorf("lecture d'un objet: %w", err)
		}
		if m.UpdatedAt, err = parseTenantTime(updatedAt); err != nil {
			return nil, err
		}
		metas = append(metas, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des objets: %w", err)
	}

	return metas, nil
}

// ListCollections retourne les collections non vides commençant par le
// préfixe donné (préfixe vide = toutes), triées.
func (r *PluginObjectRepository) ListCollections(ctx context.Context, q Querier, pluginName, orgID, memberID, prefix string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT collection FROM plugin_objects
		WHERE plugin_name = ? AND org_id = ? AND member_id = ?
			AND collection LIKE ? ESCAPE '\'
		ORDER BY collection`, pluginName, orgID, memberID, escapeLike(prefix)+"%")
	if err != nil {
		return nil, fmt.Errorf("collections du plugin %q: %w", pluginName, err)
	}
	defer rows.Close()

	var collections []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, fmt.Errorf("lecture d'une collection: %w", err)
		}
		collections = append(collections, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des collections: %w", err)
	}

	return collections, nil
}

// ReplaceCollection remplace le contenu de la collection cible par une
// copie de la source. À appeler dans une transaction : la cible est vidée
// puis remplie, l'un sans l'autre laisserait un état incohérent.
func (r *PluginObjectRepository) ReplaceCollection(ctx context.Context, q Querier, pluginName, orgID, memberID, from, to string) (int64, error) {
	if _, err := q.ExecContext(ctx, `DELETE FROM plugin_objects
		WHERE plugin_name = ? AND org_id = ? AND member_id = ? AND collection = ?`,
		pluginName, orgID, memberID, to); err != nil {
		return 0, fmt.Errorf("remplacement de la collection %q du plugin %q: %w", to, pluginName, err)
	}

	now := formatTenantTime(time.Now().UTC())
	result, err := q.ExecContext(ctx, `INSERT INTO plugin_objects
		(plugin_name, org_id, member_id, collection, key, content_type, size, data, created_at, updated_at)
		SELECT plugin_name, org_id, member_id, ?, key, content_type, size, data, ?, ?
		FROM plugin_objects
		WHERE plugin_name = ? AND org_id = ? AND member_id = ? AND collection = ?`,
		to, now, now, pluginName, orgID, memberID, from)
	if err != nil {
		return 0, fmt.Errorf("copie de la collection %q vers %q du plugin %q: %w", from, to, pluginName, err)
	}

	copied, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("copie de la collection %q vers %q du plugin %q: %w", from, to, pluginName, err)
	}

	return copied, nil
}

// Usage retourne le volume total et le nombre d'objets du périmètre
// (plugin, org, membre) — l'assiette des quotas du service hôte.
func (r *PluginObjectRepository) Usage(ctx context.Context, q Querier, pluginName, orgID, memberID string) (bytes int64, count int64, err error) {
	row := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(size), 0), COUNT(*)
		FROM plugin_objects
		WHERE plugin_name = ? AND org_id = ? AND member_id = ?`,
		pluginName, orgID, memberID)

	if err := row.Scan(&bytes, &count); err != nil {
		return 0, 0, fmt.Errorf("usage du magasin du plugin %q: %w", pluginName, err)
	}

	return bytes, count, nil
}

// escapeLike protège les métacaractères LIKE d'un préfixe littéral.
func escapeLike(s string) string {
	replaced := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '%', '_', '\\':
			replaced = append(replaced, '\\')
		}
		replaced = append(replaced, s[i])
	}
	return string(replaced)
}

// PluginPublicSite est une ligne de plugin_public_sites.
type PluginPublicSite struct {
	Slug        string
	PluginName  string
	OrgID       string
	MemberID    string
	Collection  string
	PublishedAt time.Time
}

// PluginPublicSiteRepository gère les publications web des plugins.
type PluginPublicSiteRepository struct{}

// NewPluginPublicSiteRepository crée un PluginPublicSiteRepository.
func NewPluginPublicSiteRepository() *PluginPublicSiteRepository {
	return &PluginPublicSiteRepository{}
}

// Insert enregistre une nouvelle publication. Une violation d'unicité sur
// le slug remonte telle quelle : c'est à l'appelant de retenter avec un
// autre slug.
func (r *PluginPublicSiteRepository) Insert(ctx context.Context, q Querier, s PluginPublicSite) error {
	_, err := q.ExecContext(ctx, `INSERT INTO plugin_public_sites
		(slug, plugin_name, org_id, member_id, collection, published_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		s.Slug, s.PluginName, s.OrgID, s.MemberID, s.Collection, formatTenantTime(s.PublishedAt))
	if err != nil {
		return fmt.Errorf("publication de la collection %q du plugin %q: %w", s.Collection, s.PluginName, err)
	}

	return nil
}

// Touch rafraîchit la date de publication d'un slug existant.
func (r *PluginPublicSiteRepository) Touch(ctx context.Context, q Querier, slug string, at time.Time) error {
	if _, err := q.ExecContext(ctx, `UPDATE plugin_public_sites
		SET published_at = ? WHERE slug = ?`, formatTenantTime(at), slug); err != nil {
		return fmt.Errorf("rafraîchissement de la publication %q: %w", slug, err)
	}

	return nil
}

// FindBySlug retourne la publication du slug, ou found=false.
func (r *PluginPublicSiteRepository) FindBySlug(ctx context.Context, q Querier, slug string) (PluginPublicSite, bool, error) {
	row := q.QueryRowContext(ctx, `SELECT slug, plugin_name, org_id, member_id, collection, published_at
		FROM plugin_public_sites WHERE slug = ?`, slug)

	return scanPluginPublicSite(row, slug)
}

// FindByCollection retourne la publication d'une collection, ou found=false.
func (r *PluginPublicSiteRepository) FindByCollection(ctx context.Context, q Querier, pluginName, orgID, memberID, collection string) (PluginPublicSite, bool, error) {
	row := q.QueryRowContext(ctx, `SELECT slug, plugin_name, org_id, member_id, collection, published_at
		FROM plugin_public_sites
		WHERE plugin_name = ? AND org_id = ? AND member_id = ? AND collection = ?`,
		pluginName, orgID, memberID, collection)

	return scanPluginPublicSite(row, collection)
}

// DeleteByCollection dépublie une collection et indique si elle l'était.
func (r *PluginPublicSiteRepository) DeleteByCollection(ctx context.Context, q Querier, pluginName, orgID, memberID, collection string) (bool, error) {
	result, err := q.ExecContext(ctx, `DELETE FROM plugin_public_sites
		WHERE plugin_name = ? AND org_id = ? AND member_id = ? AND collection = ?`,
		pluginName, orgID, memberID, collection)
	if err != nil {
		return false, fmt.Errorf("dépublication de la collection %q du plugin %q: %w", collection, pluginName, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("dépublication de la collection %q du plugin %q: %w", collection, pluginName, err)
	}

	return affected > 0, nil
}

// ListByMember retourne les publications d'un membre, triées par collection.
func (r *PluginPublicSiteRepository) ListByMember(ctx context.Context, q Querier, pluginName, orgID, memberID string) ([]PluginPublicSite, error) {
	rows, err := q.QueryContext(ctx, `SELECT slug, plugin_name, org_id, member_id, collection, published_at
		FROM plugin_public_sites
		WHERE plugin_name = ? AND org_id = ? AND member_id = ?
		ORDER BY collection`, pluginName, orgID, memberID)
	if err != nil {
		return nil, fmt.Errorf("publications du plugin %q: %w", pluginName, err)
	}
	defer rows.Close()

	var sites []PluginPublicSite
	for rows.Next() {
		var (
			s           PluginPublicSite
			publishedAt string
		)
		if err := rows.Scan(&s.Slug, &s.PluginName, &s.OrgID, &s.MemberID, &s.Collection, &publishedAt); err != nil {
			return nil, fmt.Errorf("lecture d'une publication: %w", err)
		}
		if s.PublishedAt, err = parseTenantTime(publishedAt); err != nil {
			return nil, err
		}
		sites = append(sites, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des publications: %w", err)
	}

	return sites, nil
}

func scanPluginPublicSite(row *sql.Row, what string) (PluginPublicSite, bool, error) {
	var (
		s           PluginPublicSite
		publishedAt string
	)
	if err := row.Scan(&s.Slug, &s.PluginName, &s.OrgID, &s.MemberID, &s.Collection, &publishedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PluginPublicSite{}, false, nil
		}
		return PluginPublicSite{}, false, fmt.Errorf("lecture de la publication %q: %w", what, err)
	}

	var err error
	if s.PublishedAt, err = parseTenantTime(publishedAt); err != nil {
		return PluginPublicSite{}, false, err
	}

	return s, true, nil
}
