package pluginsdk

import (
	"context"

	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// brokerSetter is implemented by plugin server wrappers that need the
// broker (e.g. uiWrapper, to dial the host service during Initialize).
type brokerSetter interface {
	setBroker(broker *plugin.GRPCBroker)
}

// PluginClientBundle is returned by GRPCClient: it exposes the plugin
// client and the broker, so the host can serve AutomataHostService on a
// broker channel and pass its id through Initialize.
type PluginClientBundle struct {
	proto.AutomataPluginClient
	Broker *plugin.GRPCBroker
}

// AutomataPluginGRPC implements plugin.GRPCPlugin for go-plugin.
type AutomataPluginGRPC struct {
	plugin.Plugin
	// Impl is set on the plugin binary side only.
	Impl proto.AutomataPluginServer
}

func (p *AutomataPluginGRPC) GRPCServer(broker *plugin.GRPCBroker, s *grpc.Server) error {
	// GRPCServer runs before any gRPC method, so the broker is always set
	// before Initialize can be called.
	if setter, ok := p.Impl.(brokerSetter); ok {
		setter.setBroker(broker)
	}
	proto.RegisterAutomataPluginServer(s, p.Impl)
	return nil
}

func (p *AutomataPluginGRPC) GRPCClient(_ context.Context, broker *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return &PluginClientBundle{
		AutomataPluginClient: proto.NewAutomataPluginClient(c),
		Broker:               broker,
	}, nil
}

// PluginMap registers the plugin on both sides of the handshake.
var PluginMap = map[string]plugin.Plugin{
	PluginName: &AutomataPluginGRPC{},
}
