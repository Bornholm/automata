-- Résumé roulant d'une conversation (compaction de l'historique) : les
-- messages plus anciens que la fenêtre d'historique rejouée au modèle sont
-- condensés en un résumé unique, mis à jour au fil de l'eau par
-- internal/conversation.Compactor. Une ligne par conversation.
--
-- summary est du contenu privé (il condense des messages) : jamais
-- journalisé, comme les messages eux-mêmes.
--
-- last_message_rowid est le rowid du dernier message de la table messages
-- couvert par le résumé : la frontière exacte entre ce qui est résumé et ce
-- qui est encore rejoué verbatim. Le rowid plutôt que created_at, car deux
-- messages peuvent partager la même seconde.
CREATE TABLE conversation_summaries (
    conversation_id    TEXT PRIMARY KEY REFERENCES conversations (id),
    summary            TEXT NOT NULL,
    last_message_rowid INTEGER NOT NULL,
    messages_covered   INTEGER NOT NULL,
    updated_at         TEXT NOT NULL
);
