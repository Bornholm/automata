// Package platform tient le cycle de vie des comptes de messagerie de
// l'instance : construction des fournisseurs Courier depuis la base,
// démarrage et arrêt des pipelines d'ingestion à chaud, et suivi de leur
// état pour l'interface d'administration.
//
// L'ancienne organisation démarrait un pipeline par fournisseur déclaré en
// configuration, une fois pour toutes, au lancement du processus. Ajouter
// un compte imposait donc un redémarrage — ce que le SaaS ne peut pas
// demander à un exploitant qui gère des clients en ligne.
package platform

import (
	"context"
	"sync"
	"time"
)

// State décrit l'état d'un compte de messagerie tel que l'interface
// d'administration l'affiche.
type State string

const (
	// StateStopped : compte connu mais désactivé, ou jamais démarré.
	StateStopped State = "stopped"
	// StateStarting : le pipeline démarre (connexion en cours).
	StateStarting State = "starting"
	// StateRunning : le pipeline écoute les messages.
	StateRunning State = "running"
	// StatePairing : le compte attend d'être appairé (QR à scanner).
	StatePairing State = "pairing"
	// StateFailed : le pipeline s'est arrêté sur une erreur.
	StateFailed State = "failed"
)

// Status est l'état observable d'un compte, publié par le gestionnaire.
type Status struct {
	ID    string
	Type  string
	State State
	// Since date le dernier changement d'état.
	Since time.Time
	// Err porte le message d'erreur du dernier échec (StateFailed).
	Err string
	// PairingCode est le contenu du QR à afficher (StatePairing) ; il est
	// renouvelé toutes les vingt secondes par le fournisseur, et effacé dès
	// que l'appairage aboutit.
	PairingCode string
}

// Pairing indique si le compte attend un scan.
func (s Status) Pairing() bool { return s.State == StatePairing && s.PairingCode != "" }

// registry conserve l'état courant de chaque compte, sous mutex : il est
// écrit depuis les goroutines des pipelines et lu depuis les requêtes HTTP
// de l'administration.
type registry struct {
	mu       sync.RWMutex
	statuses map[string]Status
}

func newRegistry() *registry {
	return &registry{statuses: map[string]Status{}}
}

// set remplace l'état d'un compte.
func (r *registry) set(id string, mutate func(*Status)) {
	r.mu.Lock()
	defer r.mu.Unlock()

	status := r.statuses[id]
	status.ID = id
	mutate(&status)
	status.Since = time.Now()
	r.statuses[id] = status
}

// get retourne l'état d'un compte.
func (r *registry) get(id string) (Status, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	status, ok := r.statuses[id]
	return status, ok
}

// all retourne l'état de tous les comptes connus.
func (r *registry) all() map[string]Status {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make(map[string]Status, len(r.statuses))
	for id, status := range r.statuses {
		out[id] = status
	}

	return out
}

// remove oublie un compte supprimé.
func (r *registry) remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.statuses, id)
}

// running décrit un pipeline en cours d'exécution, et de quoi l'arrêter.
type running struct {
	cancel context.CancelFunc
	done   chan struct{}
}
