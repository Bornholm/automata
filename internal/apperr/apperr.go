// Package apperr regroupe le catalogue central des erreurs classifiées
// d'Automata. Le reste du code doit envelopper ces erreurs sentinelles avec
// fmt.Errorf("...: %w", ...) pour ajouter du contexte sans perdre la
// possibilité de les comparer avec errors.Is.
package apperr

import "errors"

var (
	ErrUnauthorized       = errors.New("non autorisé")
	ErrUnknownOrigin      = errors.New("origine inconnue")
	ErrUnknownChannel     = errors.New("canal inconnu")
	ErrMentionRequired    = errors.New("mention requise")
	ErrConfirmationNeeded = errors.New("confirmation requise")
	ErrActionExpired      = errors.New("action expirée")
	ErrDuplicateMessage   = errors.New("message déjà traité")
)
