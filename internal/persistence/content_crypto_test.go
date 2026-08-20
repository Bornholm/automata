package persistence_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
)

const testEncryptionKey = "une-cle-de-test-de-plus-de-32-octets"

func openEncrypted(t *testing.T, path string) *persistence.DB {
	t.Helper()

	db, err := persistence.OpenWithEncryption(context.Background(),
		config.StorageApplication{Driver: "sqlite3", Path: path}, testEncryptionKey)
	if err != nil {
		t.Fatalf("OpenWithEncryption: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return db
}

func seedConversation(t *testing.T, db *persistence.DB, id model.ConversationID) {
	t.Helper()

	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return persistence.NewConversationRepository().Insert(context.Background(), tx, persistence.Conversation{
			ID: id, OrgID: "atelier", Provider: "rest", ExternalChannelID: "c1",
			Kind: model.ChannelPrivate, Scope: model.ScopePersonal, ScopeID: "will",
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
		})
	})
	if err != nil {
		t.Fatalf("insertion de la conversation: %v", err)
	}
}

// Le chiffrement n'a d'intérêt que si le contenu est illisible dans le
// fichier : c'est la seule chose que constatera qui met la main sur une
// sauvegarde.
func TestContentEncryption_MessageUnreadableOnDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.sqlite")
	db := openEncrypted(t, path)
	seedConversation(t, db, "conv-1")

	const secret = "RENDEZ-VOUS CHEZ LE NOTAIRE MARDI"

	repo := persistence.NewMessageRepository(db.Cipher())
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return repo.Insert(context.Background(), tx, persistence.Message{
			ID: "m1", ConversationID: "conv-1", ExternalMessageID: "x1",
			PrincipalID: "will", Role: "user", Content: secret, ContentKind: "text",
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
		})
	}); err != nil {
		t.Fatalf("insertion du message: %v", err)
	}

	// Relecture par l'application : le contenu revient en clair.
	var got persistence.Message
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		var found bool
		var err error
		got, found, err = repo.FindByID(context.Background(), tx, "m1")
		if !found {
			t.Fatal("message introuvable")
		}
		return err
	}); err != nil {
		t.Fatalf("lecture du message: %v", err)
	}
	if got.Content != secret {
		t.Errorf("contenu relu = %q, attendu %q", got.Content, secret)
	}

	// Lecture du fichier : rien de lisible.
	if err := db.Close(); err != nil {
		t.Fatalf("fermeture: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("lecture du fichier: %v", err)
	}
	if strings.Contains(string(raw), "NOTAIRE") {
		t.Error("le contenu du message apparaît en clair dans le fichier de base")
	}
}

// Une base écrite avant l'activation du chiffrement doit rester lisible :
// sans cette tolérance, activer le réglage rendrait l'historique muet.
func TestContentEncryption_ReadsPlaintextWrittenBefore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.sqlite")

	plain, err := persistence.Open(context.Background(),
		config.StorageApplication{Driver: "sqlite3", Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	seedConversation(t, plain, "conv-1")

	clearRepo := persistence.NewMessageRepository(nil)
	if err := plain.WithTx(context.Background(), func(tx *sql.Tx) error {
		return clearRepo.Insert(context.Background(), tx, persistence.Message{
			ID: "m1", ConversationID: "conv-1", ExternalMessageID: "x1",
			PrincipalID: "will", Role: "user", Content: "message d'avant", ContentKind: "text",
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
		})
	}); err != nil {
		t.Fatalf("insertion en clair: %v", err)
	}
	if err := plain.Close(); err != nil {
		t.Fatalf("fermeture: %v", err)
	}

	db := openEncrypted(t, path)
	repo := persistence.NewMessageRepository(db.Cipher())

	var got persistence.Message
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		var found bool
		var err error
		got, found, err = repo.FindByID(context.Background(), tx, "m1")
		if !found {
			t.Fatal("message introuvable")
		}
		return err
	}); err != nil {
		t.Fatalf("lecture: %v", err)
	}
	if got.Content != "message d'avant" {
		t.Errorf("contenu relu = %q, attendu le clair d'origine", got.Content)
	}

	// La migration chiffre ce qui restait en clair, et ne repasse pas sur
	// ce qui l'est déjà.
	var report persistence.EncryptionReport
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		report, err = persistence.EncryptExistingContent(context.Background(), tx, db.Cipher())
		return err
	}); err != nil {
		t.Fatalf("migration: %v", err)
	}
	if report.Messages != 1 {
		t.Errorf("%d message(s) chiffré(s), attendu 1", report.Messages)
	}

	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		report, err = persistence.EncryptExistingContent(context.Background(), tx, db.Cipher())
		return err
	}); err != nil {
		t.Fatalf("seconde migration: %v", err)
	}
	if report.Messages != 0 || report.MessagesSkipped != 1 {
		t.Errorf("la migration a rechiffré du contenu déjà protégé: %+v", report)
	}
}

// Perdre la clé ne doit pas rendre du charabia : mieux vaut une erreur
// nette qu'un message affiché en base64 à un utilisateur.
func TestContentEncryption_FailsLoudlyWithoutKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.sqlite")
	db := openEncrypted(t, path)
	seedConversation(t, db, "conv-1")

	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return persistence.NewMessageRepository(db.Cipher()).Insert(context.Background(), tx, persistence.Message{
			ID: "m1", ConversationID: "conv-1", ExternalMessageID: "x1",
			PrincipalID: "will", Role: "user", Content: "secret", ContentKind: "text",
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
		})
	}); err != nil {
		t.Fatalf("insertion: %v", err)
	}

	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		_, _, err := persistence.NewMessageRepository(nil).FindByID(context.Background(), tx, "m1")
		return err
	})
	if err == nil {
		t.Fatal("la lecture sans clé a réussi, une erreur explicite était attendue")
	}
	if !strings.Contains(err.Error(), "encryption_key") {
		t.Errorf("erreur peu parlante: %v", err)
	}
}
