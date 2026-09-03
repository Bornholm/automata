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
// injected through AUTOMATA_LOGGER_LEVEL.
//
// The key names are NOT slog's own. The host reads a plugin's stderr
// through go-plugin, which recognises one shape and one only: hclog's
// `@level`, `@message` and `@timestamp`. A line it cannot parse — slog's
// default `level`/`msg`/`time` included — is forwarded whole, as a Debug
// message, and a host running at INFO drops it. That is how a plugin that
// logged an error every single time ended up looking completely silent.
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
		Level:       slog.Level(level),
		ReplaceAttr: hclogKeys,
	})))
}

// hclogTimeFormat is the only timestamp layout go-plugin accepts; anything
// else makes the whole line unparseable.
const hclogTimeFormat = "2006-01-02T15:04:05.000000Z07:00"

// hclogKeys renames the three attributes go-plugin looks for. Everything
// else travels untouched, as key/value pairs the host logs alongside the
// message.
func hclogKeys(groups []string, attr slog.Attr) slog.Attr {
	if len(groups) > 0 {
		return attr
	}

	switch attr.Key {
	case slog.TimeKey:
		return slog.String("@timestamp", attr.Value.Time().Format(hclogTimeFormat))
	case slog.LevelKey:
		return slog.String("@level", hclogLevelName(attr.Value.Any()))
	case slog.MessageKey:
		return slog.String("@message", attr.Value.String())
	default:
		return attr
	}
}

// hclogLevelName maps a slog level onto the name hclog understands.
func hclogLevelName(value any) string {
	level, ok := value.(slog.Level)
	if !ok {
		return "info"
	}

	switch {
	case level < slog.LevelDebug:
		return "trace"
	case level < slog.LevelInfo:
		return "debug"
	case level < slog.LevelWarn:
		return "info"
	case level < slog.LevelError:
		return "warn"
	default:
		return "error"
	}
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
