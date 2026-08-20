package secretbox

import "strings"

// Le chiffrement des contenus est applicatif, pas moteur : les valeurs
// partent chiffrées vers la base et en reviennent telles quelles. Un
// changement de moteur — SQLite aujourd'hui, PostgreSQL demain — ne
// demande donc rien de particulier, là où un chiffrement de fichier
// SQLite serait à refaire entièrement.

// contentDerivationInfo isole la clé des contenus de celle des secrets de
// configuration : deux usages, deux clés, même si le même secret les
// engendrait.
const contentDerivationInfo = "automata/content/v1"

// contentPrefix marque une valeur chiffrée. Il rend la lecture
// transparente : une base pas encore migrée, ou dont le chiffrement vient
// d'être activé, contient les deux formes côte à côte, et l'application
// continue de fonctionner pendant la migration.
const contentPrefix = "enc1:"

// NewContentBox dérive une Box pour le chiffrement des contenus
// (conversations, résumés, rappels, pièces jointes).
func NewContentBox(key string) (*Box, error) {
	return newBox(key, contentDerivationInfo)
}

// SealText chiffre une valeur et la marque comme chiffrée. Une chaîne vide
// reste vide : rien à protéger, et un marqueur ferait grossir la base sans
// rien cacher.
func (b *Box) SealText(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}

	sealed, err := b.Seal(plaintext)
	if err != nil {
		return "", err
	}

	return contentPrefix + sealed, nil
}

// OpenText déchiffre une valeur produite par SealText. Une valeur sans
// marqueur est rendue telle quelle : elle date d'avant l'activation du
// chiffrement.
func (b *Box) OpenText(value string) (string, error) {
	rest, sealed := strings.CutPrefix(value, contentPrefix)
	if !sealed {
		return value, nil
	}

	return b.Open(rest)
}

// SealBytes chiffre un contenu binaire (les pièces jointes). Le marqueur
// précède les octets scellés, pour la même raison que SealText.
func (b *Box) SealBytes(plaintext []byte) ([]byte, error) {
	if len(plaintext) == 0 {
		return plaintext, nil
	}

	sealed, err := b.Seal(string(plaintext))
	if err != nil {
		return nil, err
	}

	return append([]byte(contentPrefix), sealed...), nil
}

// OpenBytes déchiffre un contenu binaire produit par SealBytes, et rend
// tel quel ce qui n'est pas marqué.
func (b *Box) OpenBytes(value []byte) ([]byte, error) {
	if !IsSealed(value) {
		return value, nil
	}

	plaintext, err := b.Open(string(value[len(contentPrefix):]))
	if err != nil {
		return nil, err
	}

	return []byte(plaintext), nil
}

// IsSealed indique si une valeur porte le marqueur de chiffrement. La
// migration s'en sert pour ne pas chiffrer deux fois ce qui l'est déjà.
func IsSealed(value []byte) bool {
	return strings.HasPrefix(string(value), contentPrefix)
}

// IsSealedText est la variante texte de IsSealed.
func IsSealedText(value string) bool {
	return strings.HasPrefix(value, contentPrefix)
}
