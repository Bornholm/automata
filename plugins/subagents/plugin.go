package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/bornholm/automata/pkg/pluginsdk"
	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// Plugin monte, pour chaque membre, les sous-agents qu'il a activés dans
// le catalogue de l'exploitant. Chaque entrée a son prompt, sa description
// et ses outils, tirés des serveurs MCP qu'elle déclare.
type Plugin struct {
	proto.UnimplementedAutomataPluginServer

	catalog catalog
	pool    *pool

	mu   sync.Mutex
	host pluginsdk.HostClient
}

func newPlugin(cat catalog) *Plugin {
	return &Plugin{catalog: cat, pool: newPool()}
}

// SetHostClient implémente pluginsdk.HostClientSetter.
func (p *Plugin) SetHostClient(client pluginsdk.HostClient) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.host = client
}

func (p *Plugin) hostClient() pluginsdk.HostClient {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.host
}

// Describe implémente proto.AutomataPluginServer.
func (p *Plugin) Describe(context.Context, *proto.DescribeRequest) (*proto.PluginDescriptor, error) {
	return &proto.PluginDescriptor{
		Name:        "subagents",
		Version:     "0.1.0",
		Description: "Sous-agents spécialisés à activer depuis son profil, adossés à des serveurs MCP déclarés par l'exploitant.",
		// Domaine commun à toutes les entrées du catalogue : l'autorisation
		// de l'hôte ne connaît que le domaine du plugin. Une écriture, quelle
		// que soit l'entrée, exige subagents.<portée>.write.
		PermissionDomain:  "subagents",
		ProvidesSubAgents: true,
	}, nil
}

// ListTools implémente proto.AutomataPluginServer. Les outils voyagent
// avec leur sous-agent (ListSubAgents) : l'hôte n'appelle pas cette RPC
// pour un plugin à catalogue, et un appel égaré ne doit rien exposer.
func (p *Plugin) ListTools(context.Context, *proto.ListToolsInput) (*proto.ListToolsOutput, error) {
	return &proto.ListToolsOutput{}, nil
}

// ListSubAgents implémente proto.AutomataPluginServer : les entrées que CE
// membre a activées, et dont tous les identifiants requis sont renseignés.
//
// Appelée à chaque tour. La découverte des outils est mise en cache par le
// pool dès la première connexion : le coût est payé une fois par (membre,
// serveur) et par vie du plugin, pas à chaque message.
func (p *Plugin) ListSubAgents(ctx context.Context, in *proto.ListSubAgentsInput) (*proto.ListSubAgentsOutput, error) {
	host := p.hostClient()
	if host == nil || in.GetCtx() == nil || in.GetCtx().GetMemberId() == "" {
		return &proto.ListSubAgentsOutput{}, nil
	}
	orgID, memberID := in.Ctx.OrgId, in.Ctx.MemberId

	raw, found, err := host.GetConfig(ctx, orgID, memberID)
	if err != nil || !found {
		return &proto.ListSubAgentsOutput{}, nil
	}
	cfg := parseConfig(raw)

	out := &proto.ListSubAgentsOutput{}
	for _, agent := range p.catalog.Agents {
		if !cfg.enabled(agent.Name) {
			continue
		}

		values, missing := p.credentials(ctx, orgID, memberID, agent)
		if len(missing) > 0 {
			// L'entrée n'est pas montée tant qu'il manque un identifiant :
			// un sous-agent dont chaque outil échoue est pire qu'un
			// sous-agent absent. La page de profil, elle, nomme ce qui
			// manque.
			continue
		}

		tools := p.toolsFor(ctx, agent, orgID, memberID, values)
		if len(tools) == 0 {
			continue
		}

		out.SubAgents = append(out.SubAgents, &proto.NamedSubAgent{
			Name: agent.Name,
			SubAgent: &proto.SubAgentDescriptor{
				SystemPrompt:           agent.SystemPrompt,
				Description:            agent.Description,
				MaxSequentialToolCalls: int32(agent.MaxToolCalls),
			},
			Tools: tools,
		})
	}

	return out, nil
}

// toolsFor rassemble les outils de tous les serveurs d'une entrée. Un
// serveur injoignable ne fait pas tomber les autres : l'entrée est montée
// avec ce qui répond.
func (p *Plugin) toolsFor(ctx context.Context, agent catalogAgent, orgID, memberID string, values map[string]string) []*proto.ToolDescriptor {
	var tools []*proto.ToolDescriptor

	for _, server := range agent.Servers {
		descriptors, err := p.pool.tools(ctx, connection{
			agent: agent, server: server, orgID: orgID, memberID: memberID, values: values,
		})
		if err != nil {
			continue
		}

		for _, descriptor := range descriptors {
			tools = append(tools, &proto.ToolDescriptor{
				Name:            descriptor.Name,
				Description:     descriptor.Description,
				InputSchemaJson: descriptor.SchemaJSON,
				ReadOnly:        descriptor.ReadOnly,
			})
		}
	}

	return tools
}

// CallTool implémente proto.AutomataPluginServer : l'appel est routé vers
// le serveur de l'entrée qui expose cet outil. Deux entrées peuvent
// exposer le même nom d'outil — c'est in.SubAgent qui les distingue.
func (p *Plugin) CallTool(ctx context.Context, in *proto.CallToolInput) (*proto.CallToolOutput, error) {
	if in.GetCtx() == nil || in.Ctx.MemberId == "" {
		return toolError("this tool needs a member context"), nil
	}

	agent, ok := p.catalog.find(in.SubAgent)
	if !ok {
		return toolError("unknown sub-agent"), nil
	}

	orgID, memberID := in.Ctx.OrgId, in.Ctx.MemberId
	if !p.isEnabled(ctx, orgID, memberID, agent.Name) {
		return toolError("this sub-agent is not enabled for this user"), nil
	}

	values, missing := p.credentials(ctx, orgID, memberID, agent)
	if len(missing) > 0 {
		return toolError("missing credentials for this sub-agent: " + strings.Join(missing, ", ")), nil
	}

	var args map[string]any
	if in.ArgumentsJson != "" {
		if err := json.Unmarshal([]byte(in.ArgumentsJson), &args); err != nil {
			return toolError("unreadable arguments"), nil
		}
	}

	server, ok := p.serverOf(ctx, agent, orgID, memberID, values, in.Name)
	if !ok {
		return toolError(fmt.Sprintf("unknown tool %q for this sub-agent", in.Name)), nil
	}

	result, err := p.pool.call(ctx, connection{
		agent: agent, server: server, orgID: orgID, memberID: memberID, values: values,
	}, in.Name, args)
	if err != nil {
		// Le message d'erreur part au modèle : il ne nomme que le serveur,
		// jamais la configuration résolue, qui porte des identifiants.
		return toolError(err.Error()), nil
	}

	return &proto.CallToolOutput{ResultText: result}, nil
}

// serverOf retrouve le serveur d'une entrée qui expose l'outil nommé.
func (p *Plugin) serverOf(ctx context.Context, agent catalogAgent, orgID, memberID string, values map[string]string, toolName string) (serverSpec, bool) {
	for _, server := range agent.Servers {
		descriptors, err := p.pool.tools(ctx, connection{
			agent: agent, server: server, orgID: orgID, memberID: memberID, values: values,
		})
		if err != nil {
			continue
		}
		for _, descriptor := range descriptors {
			if descriptor.Name == toolName {
				return server, true
			}
		}
	}
	return serverSpec{}, false
}

// isEnabled re-vérifie l'activation au moment de l'appel : une entrée
// désactivée entre la proposition d'une écriture et sa confirmation
// n'exécute rien.
func (p *Plugin) isEnabled(ctx context.Context, orgID, memberID, agentName string) bool {
	host := p.hostClient()
	if host == nil {
		return false
	}
	raw, found, err := host.GetConfig(ctx, orgID, memberID)
	if err != nil || !found {
		return false
	}
	return parseConfig(raw).enabled(agentName)
}

// credentials lit les identifiants du membre pour une entrée. Retourne les
// valeurs à injecter dans les patrons, et les clés REQUISES manquantes.
// Les valeurs ne sortent jamais d'ici : ni journal, ni modèle, ni
// interface.
func (p *Plugin) credentials(ctx context.Context, orgID, memberID string, agent catalogAgent) (map[string]string, []string) {
	host := p.hostClient()
	// Valeurs fournies par le plugin, disponibles sans que le membre ait
	// rien à saisir. Les clés d'identifiants ne peuvent pas les masquer :
	// le catalogue refuse une clé réservée.
	values := hostValues()
	present := map[string]bool{}

	for _, cred := range agent.Credentials {
		value, found, err := host.GetSecret(ctx, orgID, memberID, secretKey(agent.Name, cred.Key))
		if err != nil || !found || value == "" {
			continue
		}
		values[cred.Key] = value
		present[cred.Key] = true
	}

	return values, agent.missingCredentials(present)
}

func toolError(text string) *proto.CallToolOutput {
	return &proto.CallToolOutput{ResultText: text, IsError: true}
}
