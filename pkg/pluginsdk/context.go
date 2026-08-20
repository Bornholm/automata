package pluginsdk

import "context"

type contextKey int

const (
	hostClientKey contextKey = iota
	pluginNameKey
)

// HostClientFromContext returns the HostClient injected into UI request
// contexts by ServeWithUI. Nil means a wiring bug.
func HostClientFromContext(ctx context.Context) HostClient {
	v, _ := ctx.Value(hostClientKey).(HostClient)
	return v
}

// PluginNameFromContext returns the plugin name injected by ServeWithUI.
func PluginNameFromContext(ctx context.Context) string {
	v, _ := ctx.Value(pluginNameKey).(string)
	return v
}

func contextWithHostClient(ctx context.Context, client HostClient) context.Context {
	return context.WithValue(ctx, hostClientKey, client)
}

func contextWithPluginName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, pluginNameKey, name)
}

// ContextWithHostClientForTest injects a HostClient; for plugin tests only.
func ContextWithHostClientForTest(ctx context.Context, client HostClient) context.Context {
	return contextWithHostClient(ctx, client)
}

// ContextWithPluginNameForTest injects a plugin name; for plugin tests only.
func ContextWithPluginNameForTest(ctx context.Context, name string) context.Context {
	return contextWithPluginName(ctx, name)
}
