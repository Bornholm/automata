package registry

import (
	"context"
	"database/sql"
	"fmt"
	"io"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
)

// StorageEncrypt chiffre les contenus déjà écrits en clair : activer
// storage.encryption_key ne protège que ce qui est écrit ensuite, et une
// base contient d'ordinaire un historique entier qui, lui, resterait
// lisible.
//
// L'opération est reprenable : chaque valeur déjà marquée comme chiffrée
// est laissée telle quelle, et une interruption ne fait que laisser du
// travail pour la prochaine fois.
func StorageEncrypt(ctx context.Context, cfg *config.Config, out io.Writer) error {
	if cfg.Storage.EncryptionKey == "" {
		return fmt.Errorf("registry: storage.encryption_key n'est pas configurée : rien à chiffrer")
	}

	db, err := persistence.OpenWithEncryption(ctx, cfg.Storage.Application, cfg.Storage.EncryptionKey)
	if err != nil {
		return fmt.Errorf("registry: ouverture de la persistance: %w", err)
	}
	defer db.Close()

	var report persistence.EncryptionReport
	if err := db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		report, err = persistence.EncryptExistingContent(ctx, tx, db.Cipher())
		return err
	}); err != nil {
		return fmt.Errorf("registry: chiffrement des contenus existants: %w", err)
	}

	fmt.Fprintln(out, "Chiffrement des contenus existants :")
	fmt.Fprintf(out, "  messages            : %d chiffré(s), %d déjà protégé(s)\n", report.Messages, report.MessagesSkipped)
	fmt.Fprintf(out, "  résumés             : %d chiffré(s), %d déjà protégé(s)\n", report.Summaries, report.SummariesSkipped)
	fmt.Fprintf(out, "  rappels             : %d chiffré(s), %d déjà protégé(s)\n", report.Reminders, report.RemindersSkipped)
	fmt.Fprintf(out, "  pièces jointes      : %d chiffrée(s), %d déjà protégée(s)\n", report.Attachments, report.AttachmentsSkipped)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "La clé se sauvegarde à part : sans elle, ces contenus sont définitivement illisibles.")

	return nil
}
