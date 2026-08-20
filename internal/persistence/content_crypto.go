package persistence

import (
	"fmt"

	"github.com/bornholm/automata/internal/secretbox"
)

// Le chiffrement des contenus se fait ici, au plus près de la base : le
// reste de l'application manipule du clair sans savoir ce qui est
// protégé. Le faire au niveau des appelants reviendrait à répéter
// l'opération sur chaque site d'écriture, et un oubli passerait
// inaperçu — une conversation en clair au milieu des autres ne se voit
// pas.
//
// Un chiffrement de fichier (SQLCipher, VFS chiffré) serait plus simple
// mais adhérerait au moteur : celui-ci suivra tel quel vers PostgreSQL.

// sealContent chiffre une valeur textuelle si un chiffrement est
// configuré, et la rend telle quelle sinon.
func sealContent(cipher *secretbox.Box, plaintext string) (string, error) {
	if cipher == nil {
		return plaintext, nil
	}

	sealed, err := cipher.SealText(plaintext)
	if err != nil {
		return "", fmt.Errorf("chiffrement du contenu: %w", err)
	}

	return sealed, nil
}

// openContent déchiffre une valeur lue en base. Une valeur non marquée
// est rendue telle quelle : les données antérieures à l'activation du
// chiffrement restent lisibles, et la migration peut se faire sans
// interruption de service.
func openContent(cipher *secretbox.Box, value string) (string, error) {
	if cipher == nil {
		if secretbox.IsSealedText(value) {
			// La clé a disparu de la configuration alors que la base
			// contient du chiffré : mieux vaut le dire que rendre un
			// charabia base64 à l'utilisateur.
			return "", fmt.Errorf("contenu chiffré lu sans clé de chiffrement configurée (storage.encryption_key)")
		}
		return value, nil
	}

	plaintext, err := cipher.OpenText(value)
	if err != nil {
		return "", fmt.Errorf("déchiffrement du contenu: %w", err)
	}

	return plaintext, nil
}

// sealBytes et openBytes sont les variantes binaires, pour les pièces
// jointes.
func sealBytes(cipher *secretbox.Box, plaintext []byte) ([]byte, error) {
	if cipher == nil {
		return plaintext, nil
	}

	sealed, err := cipher.SealBytes(plaintext)
	if err != nil {
		return nil, fmt.Errorf("chiffrement de la pièce jointe: %w", err)
	}

	return sealed, nil
}

func openBytes(cipher *secretbox.Box, value []byte) ([]byte, error) {
	if cipher == nil {
		if secretbox.IsSealed(value) {
			return nil, fmt.Errorf("pièce jointe chiffrée lue sans clé de chiffrement configurée (storage.encryption_key)")
		}
		return value, nil
	}

	plaintext, err := cipher.OpenBytes(value)
	if err != nil {
		return nil, fmt.Errorf("déchiffrement de la pièce jointe: %w", err)
	}

	return plaintext, nil
}
