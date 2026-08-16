-- Pièces jointes des messages (images, documents), conservées pour être
-- rejouées dans l'historique remis au modèle aux tours suivants.
--
-- Les notes vocales et leurs transcriptions ne sont JAMAIS stockées ici :
-- PLAN.md §3.4 impose de ne conserver ni l'audio ni sa transcription, et
-- internal/audio les traite sans jamais les persister.
--
-- data porte les octets bruts. La taille de chaque pièce est bornée à la
-- réception (attachments.max_size), mais cette table fait croître la base
-- bien plus vite que les messages textuels : voir docs/operations.md pour la
-- surveillance et la sauvegarde.
CREATE TABLE message_attachments (
    id           TEXT PRIMARY KEY,
    message_id   TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    position     INTEGER NOT NULL,
    kind         TEXT NOT NULL,
    mime_type    TEXT NOT NULL,
    filename     TEXT NOT NULL,
    caption      TEXT NOT NULL,
    data         BLOB NOT NULL,
    created_at   TEXT NOT NULL,
    UNIQUE(message_id, position)
);

-- Le rejeu de l'historique lit les pièces jointes des N derniers messages
-- d'une conversation : cet index évite un balayage complet de la table à
-- chaque tour.
CREATE INDEX idx_message_attachments_message_id
    ON message_attachments(message_id);
