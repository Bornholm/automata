package config

import "strings"

// ValidationErrors agrège plusieurs erreurs de validation. Elle implémente
// error et expose Unwrap pour rester compatible avec errors.Is/As.
type ValidationErrors []error

func (e ValidationErrors) Error() string {
	if len(e) == 0 {
		return ""
	}

	if len(e) == 1 {
		return e[0].Error()
	}

	messages := make([]string, 0, len(e))
	for _, err := range e {
		messages = append(messages, "- "+err.Error())
	}

	return strings.Join(messages, "\n")
}

// Unwrap permet à errors.Is/errors.As de parcourir les erreurs agrégées.
func (e ValidationErrors) Unwrap() []error {
	return e
}

// joinErrors construit une ValidationErrors à partir d'une liste d'erreurs,
// ou retourne nil si la liste est vide.
func joinErrors(errs []error) error {
	if len(errs) == 0 {
		return nil
	}

	return ValidationErrors(errs)
}
