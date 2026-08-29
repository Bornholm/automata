-- Catalogue des clients de modèles, administrable depuis l'interface web.
--
-- Depuis cette migration, la base fait autorité : les sections llm_clients
-- et image_clients du YAML ne servent plus qu'au SEMIS initial (jalon
-- « llm-clients-seed » dans maintenance_runs), après quoi elles ne sont
-- plus relues. Ce qui reste au YAML : quel client sert quel rôle par
-- défaut, et la définition des agents eux-mêmes.
--
-- api_key est SCELLÉE au repos (secretbox, contexte
-- « automata/llm_clients/v1 ») et n'est jamais relue par l'interface : un
-- écran ne peut que la remplacer, comme pour plugin_secrets.
CREATE TABLE llm_clients (
    name TEXT PRIMARY KEY,
    -- kind vaut 'llm' (conversation, compaction, transcription…) ou
    -- 'image' (génération d'images) : deux familles de fournisseurs aux
    -- champs proches mais aux constructeurs distincts.
    kind TEXT NOT NULL DEFAULT 'llm',
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    base_url TEXT NOT NULL DEFAULT '',
    api_key TEXT NOT NULL DEFAULT '',
    reasoning_effort TEXT NOT NULL DEFAULT '',
    -- vision à 0 : le modèle refuse les images en entrée, et les agents qui
    -- l'utilisent ne lui en envoient JAMAIS (une seule pièce jointe ferait
    -- échouer la requête entière).
    vision INTEGER NOT NULL DEFAULT 1,
    -- extra_fields est un objet JSON ajouté tel quel au corps de chaque
    -- requête (« usage: {include: true} » chez OpenRouter). Rien n'est
    -- validé : le contenu part au fournisseur.
    extra_fields TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- Quel client sert quel rôle, POUR UNE ORGANISATION. Une ligne absente
-- signifie « le client par défaut de l'instance », jamais « aucun modèle ».
--
-- Table dédiée plutôt qu'un champ d'org_settings : la personnalisation
-- d'organisation ne peut que RESTREINDRE (migration 0016), or choisir un
-- modèle n'est ni une restriction ni un octroi de capacité — c'est un
-- routage. Même raisonnement que plugin_activations (migration 0017).
--
-- role vaut un nom d'agent déclaré, ou l'un des rôles système : 'plugins',
-- 'plugins.vision', 'compaction'.
CREATE TABLE org_agent_clients (
    org_id TEXT NOT NULL REFERENCES organizations (id),
    role TEXT NOT NULL,
    client_name TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (org_id, role)
);

-- Mode « gratuit sans limite », distinct de offered : offered accorde une
-- allocation mensuelle plafonnée, qui finit par s'épuiser et met le service
-- en pause. Une organisation unlimited n'est jamais débitée et n'est jamais
-- mise en pause ; sa consommation reste mesurée dans usage_records, donc
-- son coût réel demeure visible dans les écrans d'usage et de marge.
ALTER TABLE organizations ADD COLUMN unlimited INTEGER NOT NULL DEFAULT 0;
