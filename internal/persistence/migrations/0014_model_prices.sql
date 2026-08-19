-- Grille tarifaire par modèle, en dollars par million de tokens. Elle sert
-- de repli lorsqu'un fournisseur ne rapporte pas le coût d'un appel : sans
-- elle, cet appel serait facturé zéro crédit et la consommation partirait
-- en fuite financière. Une estimation prudente vaut mieux qu'un zéro.
--
-- Le coût estimé est écrit dans usage_records.cost_amount au moment de
-- l'enregistrement, avec cost_reported à 0 : toutes les agrégations
-- existantes en tiennent compte, et la distinction entre mesuré et estimé
-- reste lisible partout.
CREATE TABLE model_prices (
    -- model est comparé d'abord à l'identique, puis comme préfixe : une
    -- ligne « deepseek/ » couvre toute une famille de modèles.
    model TEXT PRIMARY KEY,
    input_usd_per_million REAL NOT NULL,
    output_usd_per_million REAL NOT NULL,
    updated_at TEXT NOT NULL
);
