-- Magasin d'objets des plugins et publications web.

-- Objets binaires déposés par les plugins via le service hôte, scopés
-- (plugin, org, membre, collection). member_id vide = portée organisation,
-- même convention que plugin_configs. La colonne data n'est PAS scellée au
-- repos, au contraire de plugin_configs/plugin_secrets : ce magasin porte
-- du contenu destiné à être servi tel quel (pages web publiques) — il ne
-- doit JAMAIS recevoir de secret.
CREATE TABLE plugin_objects (
    plugin_name TEXT NOT NULL,
    org_id TEXT NOT NULL,
    member_id TEXT NOT NULL DEFAULT '',
    collection TEXT NOT NULL,
    key TEXT NOT NULL,
    content_type TEXT NOT NULL DEFAULT '',
    size INTEGER NOT NULL,
    data BLOB NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (plugin_name, org_id, member_id, collection, key)
);

CREATE INDEX idx_plugin_objects_scope ON plugin_objects (plugin_name, org_id, member_id);

-- Publications : une collection d'objets exposée publiquement sous
-- /s/<slug>. Le slug est généré par l'hôte (Crockford, non devinable) et
-- reste stable tant que la publication existe ; supprimer la ligne rend le
-- lien mort immédiatement. Une collection ne peut être publiée que sous un
-- seul slug à la fois.
CREATE TABLE plugin_public_sites (
    slug TEXT PRIMARY KEY,
    plugin_name TEXT NOT NULL,
    org_id TEXT NOT NULL,
    member_id TEXT NOT NULL DEFAULT '',
    collection TEXT NOT NULL,
    published_at TEXT NOT NULL,
    UNIQUE (plugin_name, org_id, member_id, collection)
);
