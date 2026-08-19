-- Liaisons de canaux créées dynamiquement par consommation d'un jeton
-- (internal/ingress) : les conversations rattachées en ligne, par
-- opposition aux canaux déclarés dans la configuration YAML. Les deux
-- sources coexistent le temps de la migration vers le SaaS ; la
-- configuration reste prioritaire à la résolution d'identité.
CREATE TABLE channel_bindings (
    provider TEXT NOT NULL,
    channel_id TEXT NOT NULL,
    org_id TEXT NOT NULL,
    -- kind : private | group ; scope : personal | group | org.
    kind TEXT NOT NULL,
    scope TEXT NOT NULL,
    scope_id TEXT NOT NULL,
    -- member_id n'est renseigné que pour un canal privé (le propriétaire).
    member_id TEXT NOT NULL DEFAULT '',
    display_name TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    PRIMARY KEY (provider, channel_id)
);
CREATE INDEX idx_channel_bindings_org ON channel_bindings(org_id);

-- Référence externe d'un mouvement de portefeuille (identifiant de session
-- Stripe, par exemple) : garantit qu'un même paiement n'est jamais crédité
-- deux fois, un webhook pouvant être rejoué. Vide pour les mouvements sans
-- origine externe (usage, gestes commerciaux, allocation).
ALTER TABLE wallet_entries ADD COLUMN external_ref TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_wallet_entries_external_ref
    ON wallet_entries(external_ref) WHERE external_ref != '';
