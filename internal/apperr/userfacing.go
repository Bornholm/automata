package apperr

import "errors"

// UserFacing enveloppe une erreur dont la cause peut — et doit — être
// expliquée à la personne qui a écrit, parce qu'elle vient de ce qu'elle a
// envoyé et non d'une panne du système.
//
// La distinction compte à l'usage : « réessaie dans quelques instants » est
// un mauvais conseil devant une note vocale inaudible, où réessayer à
// l'identique redonnera le même résultat. Reply dit ce qui s'est passé et ce
// que la personne peut faire, sans jamais exposer d'erreur interne.
type UserFacing struct {
	// Reply est le message envoyé sur le canal, en français comme tout ce
	// qui s'adresse à l'utilisateur.
	Reply string
	// Err est la cause technique, destinée aux journaux.
	Err error
}

// Error implémente error.
func (e *UserFacing) Error() string {
	if e.Err == nil {
		return e.Reply
	}
	return e.Err.Error()
}

// Unwrap permet à errors.Is de continuer à reconnaître la cause enveloppée.
func (e *UserFacing) Unwrap() error {
	return e.Err
}

// Explain enveloppe err d'un message destiné à l'utilisateur. Retourne nil
// si err est nil, pour rester utilisable en fin de chaîne.
func Explain(err error, reply string) error {
	if err == nil {
		return nil
	}
	return &UserFacing{Reply: reply, Err: err}
}

// UserReply retourne le message à envoyer à l'utilisateur si err (ou l'une
// des erreurs qu'elle enveloppe) en porte un.
func UserReply(err error) (string, bool) {
	var userFacing *UserFacing
	if errors.As(err, &userFacing) && userFacing.Reply != "" {
		return userFacing.Reply, true
	}
	return "", false
}
