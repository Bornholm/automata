-- Comptes de messagerie de l'instance, gérés depuis l'interface
-- d'administration (pilier 2 du SaaS). Ils remplacent la déclaration
-- statique de courier.providers : la migration des comptes YAML vers cette
-- table est automatique au démarrage (voir registry.migratePlatforms), et
-- conserve la configuration à l'identique — notamment le chemin de session
-- WhatsApp, sans quoi un ré-appairage serait nécessaire.
CREATE TABLE platforms (
    -- id est le nom du provider, celui qui identifie les origines et les
    -- canaux : le renommer romprait les liaisons existantes.
    id TEXT PRIMARY KEY,
    type TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',
    -- config porte la configuration du provider (YAML sérialisé en JSON),
    -- chiffrée au repos : elle contient des mots de passe et des jetons
    -- d'accès. Voir internal/web/secrets.go pour le schéma de chiffrement.
    config TEXT NOT NULL DEFAULT '',
    -- enabled permet d'arrêter un compte sans le supprimer (et donc sans
    -- perdre sa session).
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
