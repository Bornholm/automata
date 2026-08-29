// plugin-echo is the test plugin of the host-side test suite: it echoes
// tool calls back and exposes a tiny UI, exercising every extension point
// without any external dependency.
package main

import (
	"bytes"
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
		{
			Name:            "echo_store",
			Description:     "Exercise the host object store end to end.",
			InputSchemaJson: `{"type":"object","properties":{}}`,
			ReadOnly:        true,
		},
	}}, nil
}

func (p *echoPlugin) CallTool(ctx context.Context, in *proto.CallToolInput) (*proto.CallToolOutput, error) {
	if in.Name == "echo_store" {
		return p.exerciseStore(ctx, in)
	}

	return &proto.CallToolOutput{
		ResultText: fmt.Sprintf("%s org=%s member=%s idem=%s args=%s",
			in.Name, in.Ctx.GetOrgId(), in.Ctx.GetMemberId(), in.Ctx.GetIdempotencyKey(), in.ArgumentsJson),
	}, nil
}

// exerciseStore fait l'aller-retour complet du magasin d'objets à travers
// le vrai broker : c'est le seul endroit où le streaming du client SDK
// (découpage en tranches, EOF) rencontre le serveur hôte réel.
func (p *echoPlugin) exerciseStore(ctx context.Context, in *proto.CallToolInput) (*proto.CallToolOutput, error) {
	org, member := in.Ctx.GetOrgId(), in.Ctx.GetMemberId()

	// Plus d'une tranche (1 Mio) pour éprouver le découpage.
	payload := bytes.Repeat([]byte("automata!"), 150_000)
	if err := p.host.PutObject(ctx, org, member, "spaces/e2e/draft", "big.bin", "application/octet-stream", payload); err != nil {
		return fail("PutObject", err)
	}

	back, _, found, err := p.host.GetObject(ctx, org, member, "spaces/e2e/draft", "big.bin")
	if err != nil || !found || !bytes.Equal(back, payload) {
		return fail(fmt.Sprintf("GetObject (found=%v, equal=%v)", found, bytes.Equal(back, payload)), err)
	}

	if err := p.host.PutObject(ctx, org, member, "spaces/e2e/draft", "index.html", "text/html", []byte("<html>e2e</html>")); err != nil {
		return fail("PutObject index", err)
	}
	if _, err := p.host.CopyCollection(ctx, org, member, "spaces/e2e/draft", "spaces/e2e/live"); err != nil {
		return fail("CopyCollection", err)
	}
	slug, url, err := p.host.PublishCollection(ctx, org, member, "spaces/e2e/live")
	if err != nil || slug == "" || url == "" {
		return fail("PublishCollection", err)
	}
	publications, err := p.host.ListPublications(ctx, org, member)
	if err != nil || len(publications) != 1 {
		return fail(fmt.Sprintf("ListPublications (%d)", len(publications)), err)
	}
	if _, err := p.host.DeleteCollection(ctx, org, member, "spaces/e2e/live"); err != nil {
		return fail("DeleteCollection", err)
	}
	if _, err := p.host.DeleteCollection(ctx, org, member, "spaces/e2e/draft"); err != nil {
		return fail("DeleteCollection draft", err)
	}

	return &proto.CallToolOutput{ResultText: "store ok slug=" + slug + " url=" + url}, nil
}

func fail(step string, err error) (*proto.CallToolOutput, error) {
	return &proto.CallToolOutput{ResultText: fmt.Sprintf("%s failed: %v", step, err), IsError: true}, nil
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
