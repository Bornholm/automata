-- Économie de la monnaie virtuelle, pilotée depuis l'administration
-- (écran ADM-08). Jusqu'ici les packs et le taux de conversion vivaient
-- dans la configuration YAML : les ajuster imposait un redémarrage, et
-- surtout rien ne permettait de comparer ce qui est vendu à ce qui est
-- réellement dépensé.

-- Offres d'achat proposées sur la page de crédits du profil.
CREATE TABLE credit_packs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    credits INTEGER NOT NULL,
    price_eur REAL NOT NULL,
    -- featured met le pack en avant (« Le plus choisi »).
    featured INTEGER NOT NULL DEFAULT 0,
    position INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);

-- Réglages simples de l'instance, sous forme clé/valeur : taux de
-- conversion, crédits de bienvenue, allocation par défaut. Une table
-- générique plutôt qu'une colonne par réglage — ils sont peu nombreux,
-- lus ensemble, et d'autres suivront avec la configuration en ligne.
CREATE TABLE settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- Prix effectivement payé pour un achat de crédits, en euros. Sans lui, le
-- portefeuille dit combien de crédits ont été vendus mais pas ce qu'ils ont
-- rapporté : la marge serait incalculable. Zéro pour tout mouvement sans
-- contrepartie monétaire (usage, allocation, geste commercial).
ALTER TABLE wallet_entries ADD COLUMN price_eur REAL NOT NULL DEFAULT 0;
