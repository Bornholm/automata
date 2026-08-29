package pluginsdk

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"

	"github.com/hashicorp/go-plugin"

	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// noopInitWrapper adds an Initialize returning port 0, so plugins without
// a UI never answer UNIMPLEMENTED.
//
// It still dials the host service: a plugin without a UI may perfectly
// well need the HostClient from its tools — to mint a download link, for
// instance. Only the embedded HTTP server is missing here.
type noopInitWrapper struct {
	proto.AutomataPluginServer
	broker *plugin.GRPCBroker // set by GRPCServer before any gRPC call
}

func (w *noopInitWrapper) setBroker(broker *plugin.GRPCBroker) {
	w.broker = broker
}

func (w *noopInitWrapper) Initialize(_ context.Context, req *proto.InitializeRequest) (*proto.InitializeResponse, error) {
	// A plugin that does not implement HostClientSetter needs nothing from
	// the broker: staying silent keeps it working even if the host ever
	// stops serving the host service.
	setter, wants := w.AutomataPluginServer.(HostClientSetter)
	if wants && w.broker != nil {
		conn, err := w.broker.Dial(req.HostServiceBrokerId)
		if err != nil {
			return nil, fmt.Errorf("dial AutomataHostService: %w", err)
		}
		setter.SetHostClient(newGRPCHostClient(conn))
	}

	return &proto.InitializeResponse{HttpUiPort: 0}, nil
}

// WrapWithNoopInit wraps impl with a no-op Initialize. Exported for tests.
func WrapWithNoopInit(impl proto.AutomataPluginServer) proto.AutomataPluginServer {
	return &noopInitWrapper{AutomataPluginServer: impl}
}

// configureSlogFromEnv installs a JSON handler at the level the host
// injected through AUTOMATA_LOGGER_LEVEL. JSON matters: go-plugin parses
// the level field of each stderr line and forwards it at the right hclog
// level instead of flattening everything to Debug.
func configureSlogFromEnv() {
	raw := os.Getenv("AUTOMATA_LOGGER_LEVEL")
	if raw == "" {
		return
	}
	level, err := strconv.Atoi(raw)
	if err != nil {
		return
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.Level(level),
	})))
}

// Serve starts the plugin gRPC server. Call it from the plugin's main().
func Serve(impl proto.AutomataPluginServer) {
	configureSlogFromEnv()
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: HandshakeConfig,
		Plugins: map[string]plugin.Plugin{
			PluginName: &AutomataPluginGRPC{Impl: WrapWithNoopInit(impl)},
		},
		GRPCServer: plugin.DefaultGRPCServer,
	})
}

// ServeWithUI starts the plugin gRPC server with an embedded HTTP UI.
// pluginName must match Describe().Name; uiHandler serves the plugin's
// configuration pages, reverse-proxied by the host into an iframe.
func ServeWithUI(impl proto.AutomataPluginServer, pluginName string, uiHandler http.Handler) {
	configureSlogFromEnv()
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: HandshakeConfig,
		Plugins: map[string]plugin.Plugin{
			PluginName: &AutomataPluginGRPC{Impl: newUIWrapper(impl, pluginName, uiHandler)},
		},
		GRPCServer: plugin.DefaultGRPCServer,
	})
}
