package persistence

import (
	"context"
	"fmt"
)

// MessageAttachmentRepository donne accès à la table message_attachments
// (pièces jointes conservées pour être rejouées dans l'historique).
type MessageAttachmentRepository struct{}

// NewMessageAttachmentRepository crée un MessageAttachmentRepository.
func NewMessageAttachmentRepository() *MessageAttachmentRepository {
	return &MessageAttachmentRepository{}
}

// Insert insère une pièce jointe de message.
func (r *MessageAttachmentRepository) Insert(ctx context.Context, q Querier, a MessageAttachment) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO message_attachments (
			id, message_id, position, kind, mime_type, filename, caption, data, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, a.ID, a.MessageID, a.Position, a.Kind, a.MimeType, a.Filename, a.Caption, a.Data, a.CreatedAt)
	if err != nil {
		return fmt.Errorf("insertion de la pièce jointe %q du message %q: %w", a.ID, a.MessageID, err)
	}

	return nil
}

// ListByMessageIDs retourne les pièces jointes des messages désignés,
// regroupées par identifiant de message et ordonnées par position.
//
// Une seule requête couvre tout l'historique d'un tour, plutôt qu'une par
// message : l'historique est relu à CHAQUE message reçu, et le coût d'une
// requête par message y serait payé en permanence.
//
// limit borne le nombre total de pièces jointes retournées, en conservant les
// plus récentes (PLAN.md §9.4 dans son esprit : sans borne, une conversation
// riche en images ferait croître indéfiniment la taille et le coût de chaque
// requête au modèle). limit <= 0 ne retourne rien.
func (r *MessageAttachmentRepository) ListByMessageIDs(ctx context.Context, q Querier, messageIDs []string, limit int) (map[string][]MessageAttachment, error) {
	if len(messageIDs) == 0 || limit <= 0 {
		return nil, nil
	}

	// Les identifiants proviennent d'une lecture de la base, jamais d'une
	// entrée utilisateur ; ils sont malgré tout passés en paramètres liés.
	placeholders := make([]byte, 0, len(messageIDs)*2)
	args := make([]any, 0, len(messageIDs)+1)

	for i, id := range messageIDs {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
		args = append(args, id)
	}

	args = append(args, limit)

	// rowid décroissant puis inversion : on garde les pièces jointes les plus
	// récentes, tout en les restituant dans leur ordre d'origine.
	query := `
		SELECT id, message_id, position, kind, mime_type, filename, caption, data, created_at
		FROM (
			SELECT rowid, id, message_id, position, kind, mime_type, filename, caption, data, created_at
			FROM message_attachments
			WHERE message_id IN (` + string(placeholders) + `)
			ORDER BY rowid DESC
			LIMIT ?
		)
		ORDER BY rowid ASC
	`

	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("lecture des pièces jointes des messages: %w", err)
	}
	defer rows.Close()

	byMessage := make(map[string][]MessageAttachment, len(messageIDs))

	for rows.Next() {
		var a MessageAttachment
		if err := rows.Scan(&a.ID, &a.MessageID, &a.Position, &a.Kind, &a.MimeType, &a.Filename, &a.Caption, &a.Data, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("lecture des pièces jointes des messages: %w", err)
		}

		byMessage[a.MessageID] = append(byMessage[a.MessageID], a)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lecture des pièces jointes des messages: %w", err)
	}

	return byMessage, nil
}
