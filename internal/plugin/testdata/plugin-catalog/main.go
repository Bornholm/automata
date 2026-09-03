// plugin-catalog is the test plugin for sub-agent catalogues: it declares
// several named sub-agents through ListSubAgents instead of the single
// sub-agent of the descriptor, and echoes back which one a tool call was
// routed to.
package main

import (
	"context"
	"fmt"

	"github.com/bornholm/automata/pkg/pluginsdk"
	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

type catalogPlugin struct {
	proto.UnimplementedAutomataPluginServer
}

func (p *catalogPlugin) Describe(context.Context, *proto.DescribeRequest) (*proto.PluginDescriptor, error) {
	return &proto.PluginDescriptor{
		Name:              "catalog",
		Version:           "0.0.1",
		Description:       "Plugin d'essai : fournit un catalogue de sous-agents.",
		PermissionDomain:  "catalog",
		ProvidesSubAgents: true,
	}, nil
}

// ListSubAgents returns two usable entries plus one named after a
// configured agent: the host must skip that one and keep the other two.
func (p *catalogPlugin) ListSubAgents(_ context.Context, in *proto.ListSubAgentsInput) (*proto.ListSubAgentsOutput, error) {
	member := in.GetCtx().GetMemberId()

	return &proto.ListSubAgentsOutput{SubAgents: []*proto.NamedSubAgent{
		{
			Name: "alpha",
			SubAgent: &proto.SubAgentDescriptor{
				SystemPrompt:           "You are alpha, member " + member + ".",
				Description:            "First catalogue entry.",
				MaxSequentialToolCalls: 3,
			},
			Tools: []*proto.ToolDescriptor{{
				Name:            "probe",
				Description:     "Probe something, alpha style.",
				InputSchemaJson: `{"type":"object"}`,
				ReadOnly:        true,
			}},
		},
		{
			Name: "beta",
			SubAgent: &proto.SubAgentDescriptor{
				SystemPrompt: "You are beta.",
				Description:  "Second catalogue entry.",
			},
			// Same tool name as alpha on purpose: CallTool must tell them
			// apart through sub_agent.
			Tools: []*proto.ToolDescriptor{{
				Name:            "probe",
				Description:     "Probe something, beta style.",
				InputSchemaJson: `{"type":"object"}`,
			}},
		},
		{
			Name:     "main",
			SubAgent: &proto.SubAgentDescriptor{SystemPrompt: "x", Description: "Collides with a configured agent."},
		},
	}}, nil
}

func (p *catalogPlugin) CallTool(_ context.Context, in *proto.CallToolInput) (*proto.CallToolOutput, error) {
	return &proto.CallToolOutput{
		ResultText: fmt.Sprintf("%s sub_agent=%s idem=%s", in.Name, in.SubAgent, in.Ctx.GetIdempotencyKey()),
	}, nil
}

func main() {
	pluginsdk.Serve(&catalogPlugin{})
}
