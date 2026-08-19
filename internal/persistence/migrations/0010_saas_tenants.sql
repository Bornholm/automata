-- Socle SaaS, lot A (maquettes P1) : les tenants passent progressivement de
-- la configuration YAML à la base. Ces tables sont pour l'instant pilotées
-- exclusivement par l'interface d'administration web (internal/web) et la
-- sous-commande "automata web bootstrap" ; la résolution d'identité de
-- l'ingress continue de lire la configuration (lot B). La table principals
-- de la migration 0001, inutilisée en production, reste intacte.

-- Une organisation cliente : une famille ou une équipe, frontière étanche.
-- offered = organisation « offerte par la maison » : pas d'achat, une
-- allocation mensuelle de crédits non cumulative (monthly_allowance),
-- remise à niveau le 1er de chaque mois.
CREATE TABLE organizations (
    id TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    offered INTEGER NOT NULL DEFAULT 0,
    monthly_allowance INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- Un membre pré-créé par l'administrateur, rattaché à une organisation.
-- provider/external_user_id/linked_at sont renseignés à la liaison par
-- jeton (lot B) ou par le bootstrap depuis la configuration. Les champs
-- courriel servent à la récupération de compte (PRO-01) : email_verified_at
-- vide = adresse non vérifiée.
CREATE TABLE members (
    id TEXT PRIMARY KEY,
    org_id TEXT NOT NULL REFERENCES organizations(id),
    display_name TEXT NOT NULL,
    role TEXT NOT NULL DEFAULT 'member',
    email TEXT NOT NULL DEFAULT '',
    email_verified_at TEXT NOT NULL DEFAULT '',
    provider TEXT NOT NULL DEFAULT '',
    external_user_id TEXT NOT NULL DEFAULT '',
    linked_at TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX idx_members_org ON members(org_id);

-- Jetons de liaison signés à usage unique (ADM-04, ADM-05). Seul le
-- SHA-256 du jeton est stocké : le clair n'est affiché qu'une fois, à la
-- création. kind = personal (lie une personne, member_id renseigné) ou
-- group (lie un canal de groupe à l'organisation). L'état « expiré » se
-- calcule depuis expires_at, il n'est jamais écrit.
CREATE TABLE link_tokens (
    id TEXT PRIMARY KEY,
    kind TEXT NOT NULL,
    member_id TEXT NOT NULL DEFAULT '',
    org_id TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    status TEXT NOT NULL DEFAULT 'pending',
    expires_at TEXT NOT NULL,
    used_at TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);
CREATE INDEX idx_link_tokens_org ON link_tokens(org_id);
CREATE INDEX idx_link_tokens_member ON link_tokens(member_id);

-- Livre de comptes du portefeuille de crédits : immuable, une ligne par
-- mouvement, montant signé (+ apport, − usage), solde = SUM(amount).
-- kind : purchase (achat Stripe), grant (geste commercial), welcome
-- (crédits de bienvenue), allowance (allocation mensuelle des
-- organisations offertes), usage (débit de consommation, lot C),
-- adjustment (correction).
CREATE TABLE wallet_entries (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    label TEXT NOT NULL DEFAULT '',
    amount INTEGER NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX idx_wallet_entries_org ON wallet_entries(org_id, created_at);

-- Liens de profil temporaires (PRO-*) : ouverts par lien unique envoyé
-- dans la conversation. Usage unique à l'ouverture (status pending →
-- opened) ; la suite de la visite tient sur un cookie de session courte.
-- Même politique de hachage que link_tokens.
CREATE TABLE profile_links (
    id TEXT PRIMARY KEY,
    member_id TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    status TEXT NOT NULL DEFAULT 'pending',
    expires_at TEXT NOT NULL,
    opened_at TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);
CREATE INDEX idx_profile_links_member ON profile_links(member_id);
