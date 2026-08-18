package agent

import (
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
)

// ExportedBuildTaskIdentity expose buildIdentity aux tests du paquet
// agent_test : la construction de l'identité d'une tâche planifiée est une
// propriété de sécurité, elle mérite d'être vérifiée directement plutôt qu'à
// travers un tour d'agent.
func ExportedBuildTaskIdentity(r *TaskRunner, task persistence.Reminder) (model.ExecutionIdentity, model.Conversation) {
	return r.buildIdentity(task)
}
