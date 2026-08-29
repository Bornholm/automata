-- Portée « instance » des choix de modèles : org_id vide = le défaut de
-- l'instance pour ce rôle. C'est ce qui achève la migration du câblage des
-- rôles hors du YAML (agents.<nom>.client et consorts disparaissent) : le
-- CONTENU des clients était en base depuis la 0022, leur AFFECTATION y
-- entre à son tour.
--
-- La table est recréée sans la contrainte REFERENCES organizations(id) :
-- une ligne d'instance n'appartient à aucune organisation, et les clés
-- étrangères sont actives en production (storage.pragmas.foreign_keys).
-- Même modèle que plugin_configs (migration 0017), qui accepte déjà un
-- member_id vide pour la même raison.
--
-- Les rôles connus : chaque nom d'agent déclaré, plus les rôles système
-- 'plugins', 'plugins.vision', 'compaction', 'transcription',
-- 'consolidation', 'retrieval', 'image:<agent>' et 'embeddings:<index>'.
-- La liste vit dans internal/llmclients (Roles), jamais en base : un rôle
-- orphelin (agent renommé) est simplement ignoré au chargement.
CREATE TABLE org_agent_clients_v2 (
    org_id TEXT NOT NULL DEFAULT '',
    role TEXT NOT NULL,
    client_name TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (org_id, role)
);

INSERT INTO org_agent_clients_v2 (org_id, role, client_name, updated_at)
    SELECT org_id, role, client_name, updated_at FROM org_agent_clients;

DROP TABLE org_agent_clients;

ALTER TABLE org_agent_clients_v2 RENAME TO org_agent_clients;
