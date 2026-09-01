-- Suggestions d'amélioration : ce que l'introspection hebdomadaire propose
-- à chaque membre (internal/introspection).
--
-- Le cycle de vie est la raison d'être de la table : une suggestion émise
-- (proposed), poussée en conversation (delivered), suivie d'effet
-- (accepted) ou écartée (dismissed). Sans statut, impossible de ne pas se
-- répéter — et un assistant qui repropose ce qu'on a déjà refusé lasse plus
-- vite qu'un assistant qui ne propose rien.
CREATE TABLE suggestions (
    id TEXT PRIMARY KEY,
    org_id TEXT NOT NULL,
    member_id TEXT NOT NULL,
    -- kind : automation (geste répété à programmer), activation (capacité
    -- existante inutilisée), fix (friction à corriger), habit (confort
    -- observé dans les habitudes).
    kind TEXT NOT NULL,
    title TEXT NOT NULL,
    body TEXT NOT NULL,
    -- status : proposed | delivered | accepted | dismissed.
    status TEXT NOT NULL DEFAULT 'proposed',
    created_at TEXT NOT NULL,
    -- delivered_at vide = jamais poussée en conversation (visible
    -- seulement sur la page de profil).
    delivered_at TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_suggestions_member ON suggestions (org_id, member_id, created_at);

-- Opt-out : la personne qui demande qu'on ne lui propose plus rien est
-- crue, définitivement — collecte comprise, pas seulement l'envoi.
ALTER TABLE members ADD COLUMN suggestions_muted INTEGER NOT NULL DEFAULT 0;
