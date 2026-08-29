package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// La sentinelle verrouille le modèle d'embeddings d'un index : accepté à
// l'identique, refusé s'il change — la recherche sémantique se dégraderait
// EN SILENCE avec des vecteurs incomparables, et ce refus de démarrer est
// le seul du catalogue.
func TestEmbeddingsSentinelLocksTheModel(t *testing.T) {
	indexPath := filepath.Join(t.TempDir(), "vector.sqlite")

	// Premier démarrage : la sentinelle se pose.
	if err := checkEmbeddingsSentinel(indexPath, "mistral", "mistral-embed"); err != nil {
		t.Fatalf("premier démarrage: %v", err)
	}
	if _, err := os.Stat(indexPath + ".embedding"); err != nil {
		t.Fatalf("sentinelle absente: %v", err)
	}

	// Même modèle : accepté.
	if err := checkEmbeddingsSentinel(indexPath, "mistral", "mistral-embed"); err != nil {
		t.Fatalf("redémarrage à l'identique: %v", err)
	}

	// Modèle différent : refusé, avec le geste de déverrouillage dans le
	// message.
	err := checkEmbeddingsSentinel(indexPath, "openai", "text-embedding-4")
	if err == nil {
		t.Fatal("changement de modèle accepté, refus attendu")
	}
	if !strings.Contains(err.Error(), "mistral/mistral-embed") || !strings.Contains(err.Error(), "supprimez l'index") {
		t.Errorf("message %q : il doit nommer l'ancien modèle et le geste de déverrouillage", err)
	}
}
