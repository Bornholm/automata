package registry

import (
	"fmt"
	"os"
	"path/filepath"
)

// Une instance d'Automata possède ses données en exclusivité : la base
// applicative, la base mémoire et l'index bleve ne supportent pas deux
// écrivains. L'index bleve le fait respecter par un verrou bolt
// BLOQUANT — un second processus y attend indéfiniment, sans un mot,
// et le seul symptôme visible est un démarrage qui ne finit jamais.
//
// Le verrou posé ici transforme cette attente muette en refus immédiat
// et nommé. Il vaut aussi pour les déploiements où l'orchestrateur
// démarre le nouveau conteneur avant d'arrêter l'ancien : les deux
// montent le même volume, et sans ce garde-fou le déploiement échoue
// sur un healthcheck qui n'explique rien.
type instanceLock struct {
	file *os.File
}

// lockDataDir prend le verrou exclusif de l'instance sur le répertoire
// des données. Le verrou est un flock non bloquant : il tombe de
// lui-même à la mort du processus, y compris si celui-ci est tué.
func lockDataDir(dbPath string) (*instanceLock, error) {
	dir := filepath.Dir(dbPath)
	if dir == "" || dir == "." {
		return nil, nil
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("registry: création du répertoire de données %q: %w", dir, err)
	}

	path := filepath.Join(dir, ".automata.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("registry: ouverture du verrou d'instance %q: %w", path, err)
	}

	if err := tryLockFile(file); err != nil {
		file.Close()
		return nil, fmt.Errorf(
			"registry: une autre instance d'Automata utilise déjà %q (verrou %s). "+
				"Deux instances ne peuvent pas partager ces données : arrêtez la précédente avant de démarrer celle-ci",
			dir, path)
	}

	return &instanceLock{file: file}, nil
}

// release rend le verrou. Le fichier reste en place : c'est le verrou
// qui compte, pas son existence.
func (l *instanceLock) release() {
	if l == nil || l.file == nil {
		return
	}
	unlockFile(l.file)
	l.file.Close()
}
