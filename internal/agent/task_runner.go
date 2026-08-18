package agent

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
)

// taskExecutionTimeout borne un tour d'agent déclenché par une tâche
// planifiée. Personne n'attend devant son téléphone : mieux vaut laisser du
// temps à une recherche web qu'abandonner un bulletin à moitié préparé.
const taskExecutionTimeout = 5 * time.Minute

// TaskRunner exécute la consigne d'une tâche planifiée (schedule_task) et
// retourne la réponse à délivrer. Il est appelé par le dispatcher de rappels
// (internal/reminder), qui ne connaît de lui que cette signature.
//
// Trois invariants tiennent ici, aucun n'étant laissé au modèle :
//
//   - l'identité d'exécution est celle du CRÉATEUR de la tâche, figée en
//     base à la création — jamais celle d'un autre principal, jamais une
//     identité reconstruite depuis le contenu de la consigne ;
//   - la portée (scope) vient de la configuration du canal, comme pour un
//     message réel de cette conversation ;
//   - les actions sensibles proposées pendant le tour sont IGNORÉES. Une
//     tâche s'exécute sans personne devant l'écran : rien ne doit pouvoir
//     écrire dehors sans confirmation humaine. C'est la même politique que
//     read_only côté schedules (PLAN.md §11.3).
type TaskRunner struct {
	cfg    *config.Config
	agents *Registry
	logger *slog.Logger
}

// NewTaskRunner construit un TaskRunner. logger nil retombe sur le logger
// par défaut.
func NewTaskRunner(cfg *config.Config, agents *Registry, logger *slog.Logger) *TaskRunner {
	if logger == nil {
		logger = slog.Default()
	}
	return &TaskRunner{cfg: cfg, agents: agents, logger: logger}
}

// RunTask exécute la consigne de task et retourne la réponse de l'agent.
//
// Ni la consigne ni la réponse ne sont journalisées : ce sont des contenus
// privés (AGENTS.md). Seuls les identifiants le sont.
func (r *TaskRunner) RunTask(ctx context.Context, task persistence.Reminder) (string, error) {
	logCtx := []any{
		"trigger", model.TriggerCron,
		"task_id", task.ID,
		"org_id", task.OrgID,
		"principal_id", task.PrincipalID,
		"agent_id", task.AgentID,
	}

	a, err := r.agents.Get(task.AgentID)
	if err != nil {
		return "", fmt.Errorf("agent %q de la tâche introuvable: %w", task.AgentID, err)
	}

	identity, conversation := r.buildIdentity(task)

	execCtx, cancel := context.WithTimeout(ctx, taskExecutionTimeout)
	defer cancel()

	result, err := a.Execute(execCtx, Request{
		Identity:     identity,
		Conversation: conversation,
		Input:        task.Message,
	})
	if err != nil {
		return "", fmt.Errorf("exécution de l'agent %q: %w", task.AgentID, err)
	}

	reply := result.Reply

	if len(result.ProposedActions) > 0 {
		// Lecture seule stricte : l'utilisateur voit ce qui a été écarté,
		// et peut le redemander de vive voix s'il le veut vraiment.
		r.logger.InfoContext(ctx, "task: actions proposées ignorées (lecture seule)", append(logCtx, "count", len(result.ProposedActions))...)
		reply = fmt.Sprintf("%s\n\n(%d action(s) proposée(s) ignorée(s) : une tâche programmée s'exécute en lecture seule)", reply, len(result.ProposedActions))
	}

	return reply, nil
}

// buildIdentity reconstruit l'identité d'exécution et la conversation d'une
// tâche depuis ce qui a été figé à sa création, complété par la portée du
// canal telle que déclarée en configuration.
//
// La portée n'est PAS stockée sur la tâche : la relire dans la configuration
// garantit qu'une tâche ancienne suit toujours la politique courante du
// canal — si un canal change de portée, ses tâches suivent, elles ne
// conservent pas un accès devenu illégitime.
func (r *TaskRunner) buildIdentity(task persistence.Reminder) (model.ExecutionIdentity, model.Conversation) {
	scope := model.ScopePersonal
	scopeID := model.ScopeID(task.PrincipalID)
	channelKind := model.ChannelPrivate

	for _, ch := range r.cfg.Channels {
		if ch.Provider != task.Provider || ch.ChannelID != task.ChannelID {
			continue
		}

		if ch.Kind == config.ChannelKindGroup {
			channelKind = model.ChannelGroup
		}
		if ch.Scope != "" {
			scope = model.Scope(ch.Scope)
		}
		if ch.ScopeID != "" {
			scopeID = model.ScopeID(ch.ScopeID)
		}
		break
	}

	identity := model.ExecutionIdentity{
		PrincipalID:    task.PrincipalID,
		OrgID:          task.OrgID,
		ConversationID: task.ConversationID,
		Provider:       task.Provider,
		ChannelID:      task.ChannelID,
		ChannelKind:    channelKind,
		Scope:          scope,
		ScopeID:        scopeID,
		Trigger:        model.TriggerCron,
	}

	conversation := model.Conversation{
		ID:      task.ConversationID,
		OrgID:   task.OrgID,
		Scope:   scope,
		ScopeID: scopeID,
	}

	return identity, conversation
}
