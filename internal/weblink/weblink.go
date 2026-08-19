// Package weblink porte la cryptographie légère des jetons de liaison et
// des liens de profil du socle SaaS : génération aléatoire, hachage,
// formes d'affichage. Séparé du serveur web (internal/web) parce que
// l'ingress (consommation des jetons) et le registry (génération de liens
// par l'agent) en ont besoin sans dépendre du serveur.
package weblink

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// crockford est l'alphabet base32 de Crockford (sans I, L, O, U) : les
// jetons se transmettent à voix haute ou se recopient sans ambiguïté.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// LinkTokenPrefix identifie un jeton de liaison Automata dans un message
// (« atm_… ») — le préfixe recherché par l'ingress.
const LinkTokenPrefix = "atm_"

// RandomCrockford retourne n caractères aléatoires de l'alphabet.
func RandomCrockford(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("weblink: lecture d'aléa: %w", err)
	}

	var b strings.Builder
	for _, c := range raw {
		b.WriteByte(crockford[int(c)%len(crockford)])
	}

	return b.String(), nil
}

// NewLinkToken produit un jeton de liaison : le clair (« atm_ » + 16
// caractères, ~80 bits), son hachage SHA-256 hexadécimal (seule forme
// stockée) et sa forme d'affichage en quatre blocs de quatre
// (« atm_XXXX · XXXX · XXXX · XXXX », annotation ADM-04).
func NewLinkToken() (clear, hash, display string, err error) {
	body, err := RandomCrockford(16)
	if err != nil {
		return "", "", "", err
	}

	clear = LinkTokenPrefix + body
	hash = HashToken(clear)
	display = LinkTokenPrefix + body[0:4] + " · " + body[4:8] + " · " + body[8:12] + " · " + body[12:16]

	return clear, hash, display, nil
}

// HashToken retourne le SHA-256 hexadécimal d'un secret.
func HashToken(clear string) string {
	sum := sha256.Sum256([]byte(clear))
	return hex.EncodeToString(sum[:])
}

// NewProfileLink produit un lien de profil : l'identifiant public (clé de
// la ligne profile_links), le secret dont seul le hachage est stocké, et
// le segment d'URL « <id>.<secret> » à composer avec web.base_url.
func NewProfileLink() (id, secretHash, urlPath string, err error) {
	id, err = RandomCrockford(6)
	if err != nil {
		return "", "", "", err
	}
	secret, err := RandomCrockford(20)
	if err != nil {
		return "", "", "", err
	}

	id = strings.ToLower(id)
	secret = strings.ToLower(secret)

	return id, HashToken(secret), id + "." + secret, nil
}

// SplitProfileLink sépare un segment « <id>.<secret> » ; ok est faux si la
// forme n'est pas celle attendue.
func SplitProfileLink(segment string) (id, secret string, ok bool) {
	id, secret, found := strings.Cut(segment, ".")
	if !found || id == "" || secret == "" {
		return "", "", false
	}
	return id, secret, true
}

// TokenPrefix retourne le préfixe d'affichage d'un jeton dont on ne
// connaît plus que le hachage — l'identifiant de ligne sert alors de
// repère court (« atm_9K2P… » dans les maquettes ne peut pas être
// reconstruit depuis le hachage : on affiche l'identifiant du jeton).
func TokenPrefix(id string) string {
	if len(id) > 8 {
		id = id[:8]
	}
	return id + "…"
}
