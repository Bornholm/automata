package registry

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/bornholm/automata/internal/action"
	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/llmclients"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/plugin"
)

// pluginSpecialistProvider adapte le gestionnaire de plugins au contrat
// agent.PluginSpecialistProvider : par tour, il relit l'activation de
// l'organisation, interroge ListTools avec l'identité (le plugin taille
// ses outils selon les réglages du membre) et fabrique les sous-agents.
// Les modèles (sous-agent et view_file) sont résolus À CHAQUE tour par le
// catalogue, sous les rôles « plugins » et « plugins.vision » : rien n'est
// construit au démarrage.
type pluginSpecialistProvider struct {
	manager *plugin.Manager
	db      *persistence.DB
	// maxFileBytes borne un fichier échangé avec un plugin, dans les deux
	// sens (attachments.max_tool_size).
	maxFileBytes int64
	// skills est la bibliothèque de compétences. Le nom d'agent vu par
	// elle est celui du plugin : une compétence ciblée
	// `agents: [workspace]` n'apparaît qu'au sous-agent du plugin
	// workspace.
	skills agent.SkillsProvider
	// clientResolver fournit les modèles des sous-agents et de view_file.
	clientResolver agent.ClientResolver
	logger         *slog.Logger
}

// newPluginSpecialistProvider construit le provider ; nil si le système de
// plugins est désactivé.
func newPluginSpecialistProvider(cfg *config.Config, manager *plugin.Manager, db *persistence.DB, skills agent.SkillsProvider, clientResolver agent.ClientResolver, logger *slog.Logger) (agent.PluginSpecialistProvider, error) {
	if manager == nil {
		return nil, nil
	}

	return &pluginSpecialistProvider{
		manager:        manager,
		db:             db,
		maxFileBytes:   int64(cfg.Attachments.MaxToolSize.Bytes()),
		skills:         skills,
		clientResolver: clientResolver,
		logger:         logger,
	}, nil
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
			SupportsFiles:    spec.SupportsFiles,
		}
		for _, t := range spec.Tools {
			agentSpec.Tools = append(agentSpec.Tools, agent.PluginToolSpec{
				Name:           t.Name,
				Description:    t.Description,
				SchemaJSON:     t.SchemaJSON,
				ReadOnly:       t.ReadOnly,
				TimeoutSeconds: t.TimeoutSeconds,
			})
		}

		subAgent := agent.NewPluginSubAgent(agentSpec, nil, pluginToolCaller{p.manager}, 0, p.logger).
			WithSkills(p.skills).
			WithClientResolver(p.clientResolver, llmclients.RolePlugins, llmclients.RolePluginsVision, p.logger)
		if agentSpec.SupportsFiles {
			// La borne des fichiers échangés est celle des pièces jointes
			// « outillage seulement » : c'est la même taille qui entre par
			// la messagerie et qui doit pouvoir en ressortir. view_file, lui,
			// se monte au tour, quand le rôle plugins.vision résout.
			subAgent = subAgent.WithFiles(pluginFileTransfer{p.manager}, p.maxFileBytes)
		}

		specialists[spec.PluginName] = subAgent
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
func (c pluginToolCaller) CallPluginTool(ctx context.Context, pluginName, toolName string, callCtx agent.PluginCallContext, argsJSON string, timeoutSeconds int) (string, bool, error) {
	return c.manager.CallTool(ctx, pluginName, toolName, plugin.CallContext{
		OrgID:          callCtx.OrgID,
		MemberID:       callCtx.MemberID,
		Scope:          callCtx.Scope,
		ScopeID:        callCtx.ScopeID,
		IdempotencyKey: callCtx.IdempotencyKey,
	}, argsJSON, timeoutSeconds)
}

// pluginFileTransfer adapte les transferts de fichiers du gestionnaire au
// contrat agent.PluginFileTransfer.
type pluginFileTransfer struct {
	manager *plugin.Manager
}

// PutPluginFile implémente agent.PluginFileTransfer.
func (t pluginFileTransfer) PutPluginFile(ctx context.Context, pluginName string, callCtx agent.PluginCallContext, meta agent.PluginFileMeta, r io.Reader) (string, bool, string, error) {
	return t.manager.PutFile(ctx, pluginName, toPluginCallContext(callCtx), plugin.FileMeta{
		Filename: meta.Filename,
		MimeType: meta.MimeType,
		Size:     meta.Size,
	}, r)
}

// OpenPluginFile implémente agent.PluginFileTransfer.
func (t pluginFileTransfer) OpenPluginFile(ctx context.Context, pluginName string, callCtx agent.PluginCallContext, path string) (agent.PluginFileMeta, io.ReadCloser, error) {
	meta, body, err := t.manager.OpenFile(ctx, pluginName, toPluginCallContext(callCtx), path)
	if err != nil {
		return agent.PluginFileMeta{}, nil, err
	}

	return agent.PluginFileMeta{
		Filename: meta.Filename,
		MimeType: meta.MimeType,
		Size:     meta.Size,
	}, body, nil
}

func toPluginCallContext(callCtx agent.PluginCallContext) plugin.CallContext {
	return plugin.CallContext{
		OrgID:          callCtx.OrgID,
		MemberID:       callCtx.MemberID,
		Scope:          callCtx.Scope,
		ScopeID:        callCtx.ScopeID,
		IdempotencyKey: callCtx.IdempotencyKey,
	}
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
