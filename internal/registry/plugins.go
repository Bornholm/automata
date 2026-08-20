package registry

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/action"
	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/plugin"
)

// pluginSpecialistProvider adapte le gestionnaire de plugins au contrat
// agent.PluginSpecialistProvider : par tour, il relit l'activation de
// l'organisation, interroge ListTools avec l'identité (le plugin taille
// ses outils selon les réglages du membre) et fabrique les sous-agents.
// Le client LLM est construit UNE fois — c'est un client de l'instance,
// décoré par la comptabilité d'usage comme tous les autres.
type pluginSpecialistProvider struct {
	manager *plugin.Manager
	db      *persistence.DB
	client  llm.ChatCompletionClient
	logger  *slog.Logger
}

// newPluginSpecialistProvider construit le provider ; nil si le système de
// plugins est désactivé.
func newPluginSpecialistProvider(ctx context.Context, cfg *config.Config, manager *plugin.Manager, db *persistence.DB, logger *slog.Logger) (agent.PluginSpecialistProvider, error) {
	if manager == nil {
		return nil, nil
	}

	client, err := agent.BuildLLMClient(ctx, cfg.LLMClients[cfg.Plugins.Client])
	if err != nil {
		return nil, err
	}

	return &pluginSpecialistProvider{manager: manager, db: db, client: client, logger: logger}, nil
}

// SpecialistsFor implémente agent.PluginSpecialistProvider.
func (p *pluginSpecialistProvider) SpecialistsFor(ctx context.Context, identity model.ExecutionIdentity) (map[string]delegation.Specialist, map[string]string) {
	specs := p.manager.ActiveSubAgents(ctx, p.db, plugin.CallContext{
		OrgID:    string(identity.OrgID),
		MemberID: string(identity.PrincipalID),
		Scope:    string(identity.Scope),
		ScopeID:  string(identity.ScopeID),
	})
	if len(specs) == 0 {
		return nil, nil
	}

	specialists := make(map[string]delegation.Specialist, len(specs))
	descriptions := make(map[string]string, len(specs))

	for _, spec := range specs {
		agentSpec := agent.PluginSubAgentSpec{
			PluginName:       spec.PluginName,
			SystemPrompt:     spec.SystemPrompt,
			Description:      spec.Description,
			PermissionDomain: spec.PermissionDomain,
			MaxToolCalls:     spec.MaxToolCalls,
		}
		for _, t := range spec.Tools {
			agentSpec.Tools = append(agentSpec.Tools, agent.PluginToolSpec{
				Name:        t.Name,
				Description: t.Description,
				SchemaJSON:  t.SchemaJSON,
				ReadOnly:    t.ReadOnly,
			})
		}

		specialists[spec.PluginName] = agent.NewPluginSubAgent(agentSpec, p.client, pluginToolCaller{p.manager}, 0, p.logger)
		descriptions[spec.PluginName] = spec.Description
	}

	return specialists, descriptions
}

// pluginToolCaller adapte plugin.Manager.CallTool au contrat du paquet
// agent.
type pluginToolCaller struct {
	manager *plugin.Manager
}

// CallPluginTool implémente agent.PluginToolCaller.
func (c pluginToolCaller) CallPluginTool(ctx context.Context, pluginName, toolName string, callCtx agent.PluginCallContext, argsJSON string) (string, bool, error) {
	return c.manager.CallTool(ctx, pluginName, toolName, plugin.CallContext{
		OrgID:          callCtx.OrgID,
		MemberID:       callCtx.MemberID,
		Scope:          callCtx.Scope,
		ScopeID:        callCtx.ScopeID,
		IdempotencyKey: callCtx.IdempotencyKey,
	}, argsJSON)
}

// pluginDomainSource adapte le gestionnaire au contrat
// authorization.PluginDomainSource.
type pluginDomainSource struct {
	manager *plugin.Manager
	db      *persistence.DB
}

// ActiveDomains implémente authorization.PluginDomainSource.
func (s pluginDomainSource) ActiveDomains(ctx context.Context, orgID string) []string {
	return s.manager.ActiveDomains(ctx, s.db, orgID)
}

// pluginTriggerRunner exécute le sous-agent d'un plugin pour un événement
// extérieur, puis transforme ses actions proposées en plan confirmable —
// même chemin que la conversation : « email entrant → proposer une
// réponse » est le cas nominal, pas une exception.
type pluginTriggerRunner struct {
	provider agent.PluginSpecialistProvider
	actions  *action.Engine
	logger   *slog.Logger
}

// RunTrigger implémente plugin.TriggerRunner.
func (r *pluginTriggerRunner) RunTrigger(ctx context.Context, pluginName string, identity model.ExecutionIdentity, conversation model.Conversation, input string) (string, error) {
	specialists, _ := r.provider.SpecialistsFor(ctx, identity)
	specialist, ok := specialists[pluginName]
	if !ok {
		return "", fmt.Errorf("sous-agent du plugin %q indisponible pour cette organisation", pluginName)
	}

	result, err := specialist.Execute(ctx, delegation.Request{
		AgentID:  pluginName,
		Goal:     input,
		Identity: identity,
	})
	if err != nil {
		return "", err
	}

	reply := strings.TrimSpace(result.Summary)

	if r.actions != nil && len(result.ProposedActions) > 0 {
		_, planText, err := r.actions.CreatePlan(ctx, identity, result.ProposedActions)
		if err != nil {
			return "", fmt.Errorf("création du plan d'actions: %w", err)
		}
		reply = strings.TrimSpace(reply + "\n\n" + planText)
	}

	return reply, nil
}
