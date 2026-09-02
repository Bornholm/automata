-- Rappels ponctuels créés conversationnellement ("rappelle-moi demain à
-- 9h..."), par opposition aux schedules récurrents déclarés dans la
-- configuration (plan de conception, §11) : un rappel est créé par un outil de l'agent,
-- vit en base, se déclenche une seule fois puis devient inerte.
--
-- message est du contenu privé : il est délivré tel quel sur le canal
-- d'origine, mais ne doit JAMAIS apparaître dans les journaux (AGENTS.md,
-- "ne pas journaliser les contenus privés").
--
-- provider/channel_id sont figés à la création : le rappel est délivré sur
-- le canal où il a été demandé, jamais ailleurs — le modèle ne choisit
-- jamais la destination.
CREATE TABLE reminders (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL,
    principal_id    TEXT NOT NULL,
    conversation_id TEXT NOT NULL,
    provider        TEXT NOT NULL,
    channel_id      TEXT NOT NULL,
    message         TEXT NOT NULL,
    fire_at         TEXT NOT NULL, -- RFC3339 UTC
    status          TEXT NOT NULL, -- pending | sent | cancelled | failed
    created_at      TEXT NOT NULL, -- RFC3339 UTC
    sent_at         TEXT           -- RFC3339 UTC, renseigné au statut sent
);

-- Le dispatcher liste les rappels dus à chaque tick : (status, fire_at)
-- évite un balayage complet quand la table accumule des rappels passés.
CREATE INDEX idx_reminders_due ON reminders(status, fire_at);

-- list_reminders filtre par conversation.
CREATE INDEX idx_reminders_conversation ON reminders(conversation_id, status);
