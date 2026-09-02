-- Missions : les dossiers au long cours (internal/mission).
--
-- Une mission n'est ni un rappel ni une tâche cron : c'est un objectif
-- suivi pendant des semaines, avec un JOURNAL DE BORD que l'agent relit et
-- complète à chaque réveil. Le journal est toute la continuité — sans lui,
-- chaque réveil serait amnésique, comme le sont les tâches planifiées.
--
-- objective et journal sont chiffrés au repos quand storage.encryption_key
-- est posée (db.Cipher, comme reminders.message). title reste en clair :
-- il s'affiche dans des listes et ne doit porter aucun contenu sensible —
-- l'outil de création le dit au modèle.
CREATE TABLE missions (
    id TEXT PRIMARY KEY,
    org_id TEXT NOT NULL,
    -- principal_id est le créateur, figé : la mission agit en son nom.
    principal_id TEXT NOT NULL,
    -- conversation_id est celle d'origine : c'est là que les plans
    -- d'actions proposés par un réveil attendent leur « confirmer ».
    conversation_id TEXT NOT NULL,
    provider TEXT NOT NULL,
    channel_id TEXT NOT NULL,
    agent_id TEXT NOT NULL,
    title TEXT NOT NULL,
    objective TEXT NOT NULL,
    journal TEXT NOT NULL DEFAULT '',
    -- status : active | done | abandoned.
    status TEXT NOT NULL,
    -- next_check_at vide = mission close.
    next_check_at TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    last_run_at TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX idx_missions_due ON missions (status, next_check_at);
CREATE INDEX idx_missions_member ON missions (org_id, principal_id);
