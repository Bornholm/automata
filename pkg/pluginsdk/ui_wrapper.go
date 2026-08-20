package pluginsdk

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"

	"github.com/hashicorp/go-plugin"

	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// Header names of the identity contract injected by the host's reverse
// proxy. They are trustworthy: the proxy derives them from the
// authenticated session and the URL, never from the browser.
const (
	HeaderOrgID    = "X-Automata-Org-Id"
	HeaderMemberID = "X-Automata-Member-Id"
	HeaderBasePath = "X-Automata-Plugin-Base-Path"
	HeaderView     = "X-Automata-View"
	HeaderUIToken  = "X-Automata-UI-Token"
	HeaderBaseURL  = "X-Automata-Base-Url"
)

// OrgID returns the organization the UI request acts for.
func OrgID(r *http.Request) string { return r.Header.Get(HeaderOrgID) }

// MemberID returns the member the UI request acts for; empty in the admin
// view when no member is selected.
func MemberID(r *http.Request) string { return r.Header.Get(HeaderMemberID) }

// BasePath returns the absolute prefix under which the plugin UI is
// mounted; use it to build URLs that survive the reverse proxy.
func BasePath(r *http.Request) string { return r.Header.Get(HeaderBasePath) }

// View returns "admin", "member", or "public" for the OAuth callback
// route, so one UI can adapt to each surface.
func View(r *http.Request) string { return r.Header.Get(HeaderView) }

// BaseURL returns the public base URL of the instance. Plugins use it to
// build an OAuth redirect URI that a provider can be configured with:
// BaseURL(r) + "/plugins/<name>/oauth/callback".
func BaseURL(r *http.Request) string { return r.Header.Get(HeaderBaseURL) }

// HostClientSetter is implemented by plugins that need the HostClient from
// their gRPC methods (ListTools/CallTool/WatchTriggers), not only from
// their UI. ServeWithUI calls SetHostClient once the broker connection is
// established.
type HostClientSetter interface {
	SetHostClient(HostClient)
}

// uiWrapper handles Initialize: dial the host service through the broker,
// start the embedded HTTP server, return its port and auth token.
type uiWrapper struct {
	proto.AutomataPluginServer
	pluginName string
	uiHandler  http.Handler
	broker     *plugin.GRPCBroker // set by GRPCServer before any gRPC call
}

func newUIWrapper(impl proto.AutomataPluginServer, pluginName string, uiHandler http.Handler) *uiWrapper {
	return &uiWrapper{
		AutomataPluginServer: impl,
		pluginName:           pluginName,
		uiHandler:            uiHandler,
	}
}

func (w *uiWrapper) setBroker(broker *plugin.GRPCBroker) {
	w.broker = broker
}

func (w *uiWrapper) Initialize(_ context.Context, req *proto.InitializeRequest) (*proto.InitializeResponse, error) {
	if w.broker == nil {
		return nil, fmt.Errorf("broker not set: GRPCServer must run before Initialize")
	}

	conn, err := w.broker.Dial(req.HostServiceBrokerId)
	if err != nil {
		return nil, fmt.Errorf("dial AutomataHostService: %w", err)
	}

	hostClient := newGRPCHostClient(conn)

	if setter, ok := w.AutomataPluginServer.(HostClientSetter); ok {
		setter.SetHostClient(hostClient)
	}

	// The UI listens on the loopback with an OS-assigned port. That alone
	// does not close it to other local processes, so every request must
	// carry the token minted here — only the host's reverse proxy knows it.
	rawToken := make([]byte, 32)
	if _, err := rand.Read(rawToken); err != nil {
		return nil, fmt.Errorf("mint ui token: %w", err)
	}
	token := hex.EncodeToString(rawToken)

	wrapped := http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Header.Get(HeaderUIToken) != token {
			http.Error(rw, "forbidden", http.StatusForbidden)
			return
		}
		ctx := contextWithHostClient(r.Context(), hostClient)
		ctx = contextWithPluginName(ctx, w.pluginName)
		w.uiHandler.ServeHTTP(rw, r.WithContext(ctx))
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	srv := &http.Server{Handler: wrapped}
	go func() {
		// The server lives and dies with the plugin process.
		_ = srv.Serve(ln)
	}()

	port := uint32(ln.Addr().(*net.TCPAddr).Port)

	return &proto.InitializeResponse{HttpUiPort: port, UiAuthToken: token}, nil
}
