// plugin-echo is the test plugin of the host-side test suite: it echoes
// tool calls back and exposes a tiny UI, exercising every extension point
// without any external dependency.
package main

import (
	"context"
	"fmt"
	"net/http"

	"github.com/bornholm/automata/pkg/pluginsdk"
	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

type echoPlugin struct {
	proto.UnimplementedAutomataPluginServer
	host pluginsdk.HostClient
}

func (p *echoPlugin) SetHostClient(client pluginsdk.HostClient) {
	p.host = client
}

func (p *echoPlugin) Describe(context.Context, *proto.DescribeRequest) (*proto.PluginDescriptor, error) {
	return &proto.PluginDescriptor{
		Name:             "echo",
		Version:          "0.0.1",
		Description:      "Plugin d'essai : répète ce qu'on lui demande.",
		PermissionDomain: "echo",
		SubAgent: &proto.SubAgentDescriptor{
			SystemPrompt: "You are an echo assistant. Use the echo tools to answer.",
			Description:  "Echoes text back, for testing the plugin pipeline.",
		},
	}, nil
}

func (p *echoPlugin) ListTools(_ context.Context, in *proto.ListToolsInput) (*proto.ListToolsOutput, error) {
	return &proto.ListToolsOutput{Tools: []*proto.ToolDescriptor{
		{
			Name:            "echo_read",
			Description:     "Echo the given text back.",
			InputSchemaJson: `{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`,
			ReadOnly:        true,
		},
		{
			Name:            "echo_write",
			Description:     "Pretend to write the given text somewhere.",
			InputSchemaJson: `{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`,
		},
	}}, nil
}

func (p *echoPlugin) CallTool(_ context.Context, in *proto.CallToolInput) (*proto.CallToolOutput, error) {
	return &proto.CallToolOutput{
		ResultText: fmt.Sprintf("%s org=%s member=%s idem=%s args=%s",
			in.Name, in.Ctx.GetOrgId(), in.Ctx.GetMemberId(), in.Ctx.GetIdempotencyKey(), in.ArgumentsJson),
	}, nil
}

func uiHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "echo-ui org=%s member=%s base=%s view=%s",
			pluginsdk.OrgID(r), pluginsdk.MemberID(r), pluginsdk.BasePath(r), pluginsdk.View(r))
	})
	return mux
}

func main() {
	pluginsdk.ServeWithUI(&echoPlugin{}, "echo", uiHandler())
}
