# Automata plugin SDK

This directory is a standalone Go module,
`github.com/bornholm/automata/pkg/pluginsdk`. It contains everything a
plugin needs to talk to Automata: the gRPC contract
(`proto/plugin.proto` and its generated code), the go-plugin handshake,
and the serving helpers. A plugin author depends on this module only,
never on the Automata server module.

## Writing a plugin

Plugins are separate binaries started by Automata through
[hashicorp/go-plugin](https://github.com/hashicorp/go-plugin). A minimal
plugin implements the `AutomataPlugin` gRPC service and calls
`pluginsdk.Serve`:

```go
package main

import (
	"github.com/bornholm/automata/pkg/pluginsdk"
	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

type myPlugin struct {
	proto.UnimplementedAutomataPluginServer
}

func main() {
	pluginsdk.Serve(&myPlugin{})
}
```

`ServeWithUI` adds an embedded HTTP UI, reverse-proxied by the host into
a sandboxed iframe on the admin and profile pages. Implement
`pluginsdk.HostClientSetter` to receive a `HostClient` for configs,
secrets and notifications.

What the host enforces — do not fight it, design with it:

- **Sub-agent LLM calls run host-side.** Declare a system prompt, a
  description and tools in `Describe`; the host runs the loop with its
  own accounted clients.
- **Every tool not marked `read_only` is a write**: it is never executed
  during a turn, only proposed for the user's literal confirmation, then
  replayed with an `idempotency_key`. Make writes idempotent.
- **Identity comes from the host.** `CallContext` designates the org and
  member; trigger events are re-checked against activation and
  membership before anything runs.
- **Per-plugin isolation.** The host binds each broker connection to the
  plugin that established it: you can only ever see your own configs and
  secrets, and only for organizations that activated you.

The bundled `plugins/email` is a complete working example (tools, member
switches, triggers, UI, secret discipline).

## Regenerating the gRPC code

From the repository root: `make generate-proto` (the `.pb.go` files are
committed).
