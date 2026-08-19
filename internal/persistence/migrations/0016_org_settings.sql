-- Personnalisation par organisation (pilier 7) : ce qui distingue un
-- forfait d'un autre sans dupliquer la configuration de l'instance.
--
-- Trois réglages, tous facultatifs : une consigne ajoutée au prompt de
-- l'assistant, la liste des spécialistes dont l'organisation ne dispose
-- pas, et la limite d'appels d'outils par tour. Une ligne absente signifie
-- « comportement par défaut de l'instance », jamais « tout interdit ».
CREATE TABLE org_settings (
    org_id TEXT PRIMARY KEY REFERENCES organizations(id),
    -- prompt_extra est ajouté au prompt système de l'agent généraliste.
    -- Il ne remplace jamais les règles invariantes : une organisation ne
    -- peut pas se donner des droits qu'elle n'a pas.
    prompt_extra TEXT NOT NULL DEFAULT '',
    -- disabled_agents liste les spécialistes retirés, séparés par des
    -- virgules (« research,imagine »).
    disabled_agents TEXT NOT NULL DEFAULT '',
    -- max_tool_calls plafonne les appels d'outils par tour ; 0 = défaut de
    -- l'agent.
    max_tool_calls INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL
);
