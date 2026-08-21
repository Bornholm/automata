package registry

import (
	"path/filepath"
	"strings"
	"testing"
)

// Une seconde instance sur les mêmes données doit être refusée
// immédiatement et par son nom : sans ce refus, elle attendrait sans fin
// le verrou bolt de l'index bleve, et le démarrage n'échouerait jamais
// vraiment.
func TestLockDataDirRefusesSecondInstance(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "app.sqlite")

	first, err := lockDataDir(dbPath)
	if err != nil {
		t.Fatalf("première instance refusée: %v", err)
	}
	defer first.release()

	_, err = lockDataDir(dbPath)
	if err == nil {
		t.Fatal("la seconde instance aurait dû être refusée")
	}
	if !strings.Contains(err.Error(), "autre instance") {
		t.Errorf("message peu explicite: %v", err)
	}

	// Le verrou rendu, la place est libre : un redémarrage après arrêt
	// propre ne doit pas buter sur les restes du précédent.
	first.release()
	second, err := lockDataDir(dbPath)
	if err != nil {
		t.Fatalf("après libération, l'instance suivante devrait passer: %v", err)
	}
	second.release()
}
