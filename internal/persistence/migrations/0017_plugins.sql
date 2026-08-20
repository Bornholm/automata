-- Socle du système de plugins.

-- Activation des plugins PAR ORGANISATION. Table dédiée, jamais
-- org_settings : activer un plugin ACCORDE une capacité, or l'invariant de
-- la personnalisation d'organisation est de ne pouvoir que restreindre.
CREATE TABLE plugin_activations (
    plugin_name TEXT NOT NULL,
    org_id TEXT NOT NULL REFERENCES organizations (id),
    enabled INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (plugin_name, org_id)
);

-- Configurations des plugins. member_id vide = configuration de niveau
-- organisation (une clé primaire SQLite n'accepte pas NULL). La colonne
-- config est scellée au repos (secretbox, contexte "automata/plugins/v1") :
-- une configuration de plugin peut décrire des comptes externes.
CREATE TABLE plugin_configs (
    plugin_name TEXT NOT NULL,
    org_id TEXT NOT NULL,
    member_id TEXT NOT NULL DEFAULT '',
    config TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL,
    PRIMARY KEY (plugin_name, org_id, member_id)
);

-- Secrets des plugins (mots de passe IMAP, jetons d'API…), valeur scellée.
-- L'interface web ne relit JAMAIS une valeur : seul le plugin, via le
-- service hôte gRPC, y accède — et le service vérifie l'appartenance du
-- membre à l'organisation avant de servir.
CREATE TABLE plugin_secrets (
    plugin_name TEXT NOT NULL,
    org_id TEXT NOT NULL,
    member_id TEXT NOT NULL DEFAULT '',
    key TEXT NOT NULL,
    value TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (plugin_name, org_id, member_id, key)
);
