-- Marque les pièces jointes retenues POUR LES OUTILS seulement (une vidéo
-- reçue par messagerie, par exemple), qui ne doivent jamais être rejouées
-- vers le modèle : un fournisseur refuse la requête ENTIÈRE lorsqu'une pièce
-- jointe ne lui convient pas.
--
-- Le marqueur est persisté plutôt que recalculé depuis attachments.tool_types
-- à chaque rejeu : retirer un type de la configuration ferait autrement
-- basculer d'un coup tout l'historique déjà stocké vers le modèle, et
-- casserait les conversations concernées.
ALTER TABLE message_attachments
    ADD COLUMN tool_only INTEGER NOT NULL DEFAULT 0;
