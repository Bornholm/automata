package secretbox_test

import (
	"strings"
	"testing"

	"github.com/bornholm/automata/internal/secretbox"
)

const testSecret = "un-secret-de-session-de-trente-deux-octets-au-moins"

func TestBox_RoundTrip(t *testing.T) {
	box, err := secretbox.New(testSecret)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const plaintext = `{"smtp":{"password":"tr3s-secret"}}`

	sealed, err := box.Seal(plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if strings.Contains(sealed, "secret") {
		t.Fatal("le texte chiffré ne doit rien laisser paraître du secret")
	}

	opened, err := box.Open(sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if opened != plaintext {
		t.Errorf("déchiffré %q, attendu %q", opened, plaintext)
	}
}

// Deux scellés du même texte diffèrent : sans cela, l'égalité de deux
// configurations serait lisible depuis la base.
func TestBox_SealIsNotDeterministic(t *testing.T) {
	box, _ := secretbox.New(testSecret)

	first, _ := box.Seal("même texte")
	second, _ := box.Seal("même texte")

	if first == second {
		t.Error("deux chiffrements du même texte ne doivent pas être identiques")
	}
}

// Un secret de session différent ne doit jamais pouvoir ouvrir les
// secrets d'une autre instance.
func TestBox_ForeignKeyCannotOpen(t *testing.T) {
	box, _ := secretbox.New(testSecret)
	other, _ := secretbox.New("un-autre-secret-de-session-de-trente-deux-octets")

	sealed, _ := box.Seal("configuration")

	if _, err := other.Open(sealed); err == nil {
		t.Fatal("une clé étrangère ne doit pas pouvoir déchiffrer")
	}
}

func TestBox_RejectsShortSecretAndBrokenValue(t *testing.T) {
	if _, err := secretbox.New("court"); err == nil {
		t.Error("un secret trop court doit être refusé")
	}

	box, _ := secretbox.New(testSecret)
	if _, err := box.Open("pas-du-base64-valide!!"); err == nil {
		t.Error("une valeur illisible doit être refusée")
	}
	if opened, err := box.Open(""); err != nil || opened != "" {
		t.Errorf("une valeur vide doit rester vide sans erreur (got %q, %v)", opened, err)
	}
}
