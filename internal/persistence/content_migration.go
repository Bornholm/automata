package persistence

import (
	"context"
	"fmt"

	"github.com/bornholm/automata/internal/secretbox"
)

// EncryptionReport compte ce que la migration a chiffré et ce qui l'était
// déjà.
type EncryptionReport struct {
	Messages, MessagesSkipped       int
	Summaries, SummariesSkipped     int
	Reminders, RemindersSkipped     int
	Attachments, AttachmentsSkipped int
}

// EncryptExistingContent réécrit chiffrés les contenus encore en clair.
//
// La lecture se fait délibérément en SQL brut plutôt que par les
// repositories : ceux-ci déchiffrent à la lecture, ce qui empêcherait de
// distinguer une valeur déjà protégée d'une valeur en clair.
func EncryptExistingContent(ctx context.Context, q Querier, cipher *secretbox.Box) (EncryptionReport, error) {
	var report EncryptionReport

	textTables := []struct {
		table, idColumn, column string
		done, skipped           *int
	}{
		{"messages", "id", "content", &report.Messages, &report.MessagesSkipped},
		{"conversation_summaries", "conversation_id", "summary", &report.Summaries, &report.SummariesSkipped},
		{"reminders", "id", "message", &report.Reminders, &report.RemindersSkipped},
		{"message_attachments", "id", "caption", nil, nil},
	}

	for _, t := range textTables {
		done, skipped, err := encryptTextColumn(ctx, q, cipher, t.table, t.idColumn, t.column)
		if err != nil {
			return EncryptionReport{}, err
		}
		if t.done != nil {
			*t.done, *t.skipped = done, skipped
		}
	}

	done, skipped, err := encryptAttachmentData(ctx, q, cipher)
	if err != nil {
		return EncryptionReport{}, err
	}
	report.Attachments, report.AttachmentsSkipped = done, skipped

	return report, nil
}

// encryptTextColumn chiffre une colonne textuelle, ligne par ligne.
func encryptTextColumn(ctx context.Context, q Querier, cipher *secretbox.Box, table, idColumn, column string) (done, skipped int, err error) {
	// Les identifiants de table et de colonne viennent d'une liste figée
	// au-dessus, jamais d'une entrée extérieure.
	rows, err := q.QueryContext(ctx, fmt.Sprintf(`SELECT %s, %s FROM %s`, idColumn, column, table))
	if err != nil {
		return 0, 0, fmt.Errorf("lecture de %s.%s: %w", table, column, err)
	}

	type pending struct {
		id    string
		value string
	}

	var toSeal []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.value); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("lecture d'une ligne de %s: %w", table, err)
		}
		if secretbox.IsSealedText(p.value) || p.value == "" {
			skipped++
			continue
		}
		toSeal = append(toSeal, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, 0, fmt.Errorf("parcours de %s: %w", table, err)
	}
	rows.Close()

	for _, p := range toSeal {
		sealed, err := cipher.SealText(p.value)
		if err != nil {
			return 0, 0, fmt.Errorf("chiffrement de %s.%s (%s): %w", table, column, p.id, err)
		}
		if _, err := q.ExecContext(ctx,
			fmt.Sprintf(`UPDATE %s SET %s = ? WHERE %s = ?`, table, column, idColumn),
			sealed, p.id); err != nil {
			return 0, 0, fmt.Errorf("écriture de %s.%s (%s): %w", table, column, p.id, err)
		}
		done++
	}

	return done, skipped, nil
}

// encryptAttachmentData chiffre les octets des pièces jointes.
func encryptAttachmentData(ctx context.Context, q Querier, cipher *secretbox.Box) (done, skipped int, err error) {
	rows, err := q.QueryContext(ctx, `SELECT id, data FROM message_attachments`)
	if err != nil {
		return 0, 0, fmt.Errorf("lecture des pièces jointes: %w", err)
	}

	type pending struct {
		id   string
		data []byte
	}

	var toSeal []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.data); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("lecture d'une pièce jointe: %w", err)
		}
		if secretbox.IsSealed(p.data) || len(p.data) == 0 {
			skipped++
			continue
		}
		toSeal = append(toSeal, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, 0, fmt.Errorf("parcours des pièces jointes: %w", err)
	}
	rows.Close()

	for _, p := range toSeal {
		sealed, err := cipher.SealBytes(p.data)
		if err != nil {
			return 0, 0, fmt.Errorf("chiffrement de la pièce jointe %q: %w", p.id, err)
		}
		if _, err := q.ExecContext(ctx, `UPDATE message_attachments SET data = ? WHERE id = ?`, sealed, p.id); err != nil {
			return 0, 0, fmt.Errorf("écriture de la pièce jointe %q: %w", p.id, err)
		}
		done++
	}

	return done, skipped, nil
}
