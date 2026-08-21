-- Bibliothèque de compétences : modes opératoires en markdown que les
-- agents chargent à la demande (outil load_skill). Semée au démarrage
-- depuis les fichiers embarqués, uniquement pour les noms absents — une
-- compétence éditée dans l'administration n'est jamais écrasée par un
-- redéploiement.
--
-- Pas de chiffrement : le contenu n'est pas personnel (le scellement
-- secretbox est réservé aux contenus privés), et il part au modèle.
CREATE TABLE skills (
    name        TEXT PRIMARY KEY,
    description TEXT NOT NULL,
    -- Corps markdown, en anglais : il part au modèle.
    content     TEXT NOT NULL,
    -- Ciblage : liste JSON de noms d'agents ("[]" = tous).
    agents      TEXT NOT NULL DEFAULT '[]',
    enabled     INTEGER NOT NULL DEFAULT 1,
    -- Fournie par le projet : autorise « restaurer la version d'origine ».
    builtin     INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);
