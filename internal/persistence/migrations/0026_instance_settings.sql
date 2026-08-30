-- Réglages d'instance : ce qui ne dépend d'aucune organisation.
--
-- org_settings ne pouvait pas les accueillir : sa clé primaire référence
-- organizations(id), et les clés étrangères sont actives en production. Une
-- table clé/valeur convient mieux ici — ces réglages sont peu nombreux, sans
-- rapport les uns avec les autres, et chacun arrive avec sa fonctionnalité.
--
-- Première clé : operator_member_id, le membre qui reçoit les alertes
-- d'exploitation (internal/alerting).
CREATE TABLE instance_settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- Alertes émises, pour l'écran d'administration et pour la déduplication au
-- redémarrage. La table est bornée par la purge de internal/alerting : c'est
-- un journal de courtoisie, pas un historique d'audit.
--
-- delivered_at vide = alerte jamais remise (messagerie en panne, exploitant
-- non désigné) ; elle est alors rejouée à la prochaine occasion.
CREATE TABLE alerts (
    id TEXT PRIMARY KEY,
    -- kind groupe les alertes de même nature (« platform_down ») ; subject
    -- désigne ce qui est concerné (le nom du compte). Ensemble ils forment
    -- la clé de déduplication.
    kind TEXT NOT NULL,
    subject TEXT NOT NULL,
    message TEXT NOT NULL,
    created_at TEXT NOT NULL,
    delivered_at TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_alerts_dedup ON alerts (kind, subject, created_at);
