package model

import "context"

// La mention vocale : dans un groupe, un message vocal ne peut porter
// aucune mention — sur WhatsApp, un audio n'a pas de légende. La règle des
// groupes (« rien sans mention ») rendrait donc tout vocal inerte. Le
// pipeline laisse alors passer le vocal jusqu'au handler de conversation
// avec ce marqueur : la mention sera cherchée DANS la transcription, et le
// tour s'arrête là si le nom de l'assistant n'y figure pas.
//
// Le marqueur voyage par le contexte parce que les deux paquets concernés
// ne se connaissent pas : internal/ingress (transport) décide qu'une
// vérification est due, internal/conversation (qui détient la
// transcription) la réalise. Étendre l'interface Handler pour un seul
// booléen aurait couplé tous ses implémenteurs à ce cas particulier.

type voiceMentionKey struct{}

// ContextWithVoiceMentionRequired marque le tour : la transcription devra
// contenir name (comparaison insensible à la casse) pour que le message
// soit traité.
func ContextWithVoiceMentionRequired(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, voiceMentionKey{}, name)
}

// VoiceMentionRequired lit le marqueur. required vaut false pour un tour
// ordinaire.
func VoiceMentionRequired(ctx context.Context) (name string, required bool) {
	name, required = ctx.Value(voiceMentionKey{}).(string)
	return name, required
}
