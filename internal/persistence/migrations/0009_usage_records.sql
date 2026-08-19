-- Comptabilité d'usage de l'inférence LLM (internal/usage) : une ligne par
-- appel réussi (complétion, transcription, génération d'image), avec son
-- attribution (organisation, principal, conversation, composant, agent),
-- ses volumes de tokens et, quand le provider le rapporte (OpenRouter), le
-- coût réellement facturé. Aucun contenu n'est jamais stocké ici : la table
-- ne porte que des identifiants, des comptes et des montants, dans
-- l'optique d'une refacturation de l'accès par organisation/utilisateur.
--
-- Les champs d'attribution sont non NULL avec défaut '' : un appel non
-- attribuable (contexte sans attribution) est enregistré orphelin plutôt
-- qu'ignoré — pour une comptabilité, un montant orphelin vaut mieux qu'un
-- montant manquant. principal_id vide = tâche de fond facturée à
-- l'organisation (compaction, consolidation).
CREATE TABLE usage_records (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at TEXT NOT NULL,
    org_id TEXT NOT NULL DEFAULT '',
    principal_id TEXT NOT NULL DEFAULT '',
    conversation_id TEXT NOT NULL DEFAULT '',
    component TEXT NOT NULL DEFAULT '',
    agent TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    prompt_tokens INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens INTEGER NOT NULL DEFAULT 0,
    cached_tokens INTEGER NOT NULL DEFAULT 0,
    cost_amount REAL NOT NULL DEFAULT 0,
    cost_currency TEXT NOT NULL DEFAULT '',
    -- 1 si cost_amount est le montant rapporté par le provider, 0 si aucun
    -- coût n'a été rapporté (seuls les tokens font alors foi).
    cost_reported INTEGER NOT NULL DEFAULT 0
);

-- Les agrégations de refacturation filtrent toujours par période, et le plus
-- souvent par organisation.
CREATE INDEX idx_usage_records_created_at ON usage_records(created_at);
CREATE INDEX idx_usage_records_org_created_at ON usage_records(org_id, created_at);
