package core

import (
	"sync"
	"time"

	"github.com/bornholm/automata/internal/weblink"
)

// RevealStash conserve quelques secondes les secrets à afficher une seule
// fois (jeton fraîchement généré, lien de profil) entre le POST qui les
// crée et le GET qui les affiche (schéma POST-redirect-GET : sans ce
// détour, rafraîchir la page rejouerait la génération). Un secret est
// consommé à la première lecture ou expire au bout de 2 minutes ; tout
// reste en mémoire, rien n'est jamais écrit en clair.
type RevealStash struct {
	mu      sync.Mutex
	entries map[string]RevealEntry
}

type RevealEntry struct {
	value   RevealValue
	expires time.Time
}

// RevealValue porte les formes d'affichage d'un secret fraîchement créé.
type RevealValue struct {
	Clear   string
	Display string
}

func NewRevealStash() *RevealStash {
	return &RevealStash{entries: map[string]RevealEntry{}}
}

const revealTTL = 2 * time.Minute

func (s *RevealStash) Put(value RevealValue, now time.Time) (key string, err error) {
	key, err = weblink.RandomCrockford(16)
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for k, e := range s.entries {
		if now.After(e.expires) {
			delete(s.entries, k)
		}
	}
	s.entries[key] = RevealEntry{value: value, expires: now.Add(revealTTL)}

	return key, nil
}

func (s *RevealStash) Pop(key string, now time.Time) (RevealValue, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.entries[key]
	delete(s.entries, key)
	if !ok || now.After(entry.expires) {
		return RevealValue{}, false
	}

	return entry.value, true
}

// CodeStore conserve en mémoire les codes de vérification de courriel en
// attente (PRO-01) : un par membre, 10 minutes. Un redémarrage du worker
// les efface — la personne redemande simplement un code.
type CodeStore struct {
	mu      sync.Mutex
	entries map[string]CodeEntry
}

type CodeEntry struct {
	email   string
	code    string
	expires time.Time
}

func NewCodeStore() *CodeStore {
	return &CodeStore{entries: map[string]CodeEntry{}}
}

const codeTTL = 10 * time.Minute

func (s *CodeStore) Put(memberID, email, code string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[memberID] = CodeEntry{email: email, code: code, expires: now.Add(codeTTL)}
}

// pending retourne l'adresse en cours de vérification pour le membre.
func (s *CodeStore) Pending(memberID string, now time.Time) (email string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, found := s.entries[memberID]
	if !found || now.After(entry.expires) {
		return "", false
	}
	return entry.email, true
}

// verify consomme le code s'il correspond.
func (s *CodeStore) Verify(memberID, code string, now time.Time) (email string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, found := s.entries[memberID]
	if !found || now.After(entry.expires) || entry.code != code {
		return "", false
	}
	delete(s.entries, memberID)

	return entry.email, true
}
