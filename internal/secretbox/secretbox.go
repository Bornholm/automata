// Package secretbox chiffre les secrets stockés en base — aujourd'hui la
// configuration des comptes de messagerie, qui porte des mots de passe et
// des jetons d'accès.
//
// La clé dérive du secret de session du serveur web (web.session_secret)
// par HKDF-SHA256 : une seule valeur à protéger dans l'environnement
// plutôt que deux, et un contexte de dérivation distinct garantit qu'une
// clé de chiffrement ne peut jamais signer un cookie, ni l'inverse.
//
// Conséquence à connaître : changer web.session_secret rend illisibles les
// secrets déjà chiffrés. Les comptes concernés doivent alors être
// reconfigurés — c'est le prix d'une clé unique, et la seule opération
// destructrice de ce paquet.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// derivationInfo isole la dérivation de cette clé de toute autre usage du
// même secret.
const derivationInfo = "automata/secretbox/v1"

// Box chiffre et déchiffre des secrets avec AES-256-GCM.
type Box struct {
	aead cipher.AEAD
}

// New dérive une Box du secret fourni (web.session_secret).
func New(secret string) (*Box, error) {
	return newBox(secret, derivationInfo)
}

// NewPlugins dérive la Box des secrets et configurations de plugins :
// contexte HKDF distinct, un secret de plateforme ne peut jamais
// déchiffrer un secret de plugin, ni l'inverse.
func NewPlugins(secret string) (*Box, error) {
	return newBox(secret, "automata/plugins/v1")
}

// newBox dérive une Box du secret fourni, pour l'usage nommé par info :
// deux usages ne partagent jamais la même clé, même issus du même secret.
func newBox(secret, info string) (*Box, error) {
	if len(secret) < 32 {
		return nil, fmt.Errorf("secretbox: secret trop court (au moins 32 octets)")
	}

	key := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, []byte(secret), nil, []byte(info)), key); err != nil {
		return nil, fmt.Errorf("secretbox: dérivation de la clé: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secretbox: initialisation du chiffrement: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secretbox: initialisation du mode GCM: %w", err)
	}

	return &Box{aead: aead}, nil
}

// Seal chiffre plaintext et retourne une chaîne base64 (nonce + scellé),
// stockable telle quelle en base.
func (b *Box) Seal(plaintext string) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("secretbox: lecture d'aléa: %w", err)
	}

	sealed := b.aead.Seal(nonce, nonce, []byte(plaintext), nil)

	return base64.RawStdEncoding.EncodeToString(sealed), nil
}

// Open déchiffre une valeur produite par Seal. Une valeur vide reste vide :
// une configuration sans secret n'a pas besoin d'être scellée.
func (b *Box) Open(sealed string) (string, error) {
	if sealed == "" {
		return "", nil
	}

	raw, err := base64.RawStdEncoding.DecodeString(sealed)
	if err != nil {
		return "", fmt.Errorf("secretbox: valeur illisible: %w", err)
	}

	nonceSize := b.aead.NonceSize()
	if len(raw) < nonceSize {
		return "", fmt.Errorf("secretbox: valeur tronquée")
	}

	plaintext, err := b.aead.Open(nil, raw[:nonceSize], raw[nonceSize:], nil)
	if err != nil {
		// Cause la plus fréquente : le secret de session a changé.
		return "", fmt.Errorf("secretbox: déchiffrement impossible (le secret de session a-t-il changé ?): %w", err)
	}

	return string(plaintext), nil
}
