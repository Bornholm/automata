package pluginsdk

import "strings"

// TriggerSilent est la réponse par laquelle un sous-agent dit qu'il n'y a
// rien à signaler : l'hôte n'envoie alors AUCUN message.
//
// Un déclencheur observe un flux d'événements dont tous ne méritent pas
// d'interrompre quelqu'un — un courriel publicitaire, une notification
// automatique, un accusé de réception. Sans façon de se taire, le sous-agent
// résume consciencieusement le pourriel, et la personne finit par ignorer
// toutes les notifications, y compris celles qui comptaient.
//
// Un marqueur explicite plutôt qu'une réponse vide : un modèle de langue
// répond presque toujours quelque chose, et une consigne « ne réponds rien »
// produit en pratique « D'accord, je ne dis rien ». Nommer le silence le
// rend atteignable.
const TriggerSilent = "NOTHING_TO_REPORT"

// IsTriggerSilent reconnaît le marqueur de silence dans la réponse d'un
// sous-agent.
//
// La comparaison tolère la casse et la ponctuation dont un modèle habille
// volontiers un mot isolé, mais RIEN de plus : un marqueur noyé dans une
// phrase reste une réponse, sans quoi un résumé mentionnant le marqueur —
// « ce courriel n'est pas un NOTHING_TO_REPORT » — disparaîtrait en silence.
func IsTriggerSilent(reply string) bool {
	trimmed := strings.TrimSpace(reply)
	trimmed = strings.Trim(trimmed, ".!\"'`*_ \t\n")

	return strings.EqualFold(trimmed, TriggerSilent)
}
