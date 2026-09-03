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
- **A plugin may offer a catalogue of sub-agents.** Set
  `provides_sub_agents` and implement `ListSubAgents`, and the host mounts
  one delegate per entry you return — each with its own name, prompt,
  description and tools, chosen per member. `ListSubAgents` runs once per
  turn: answer from cached state, never by connecting to anything. Tool
  names need only be unique within an entry; `CallToolInput.sub_agent`
  says which one the call belongs to, on a confirmed write too. A name
  colliding with a configured agent or another mounted sub-agent is
  skipped: two identical `delegate_to_<name>` tools are indistinguishable
  to the model.
- **Every tool not marked `read_only` is a write**: it is never executed
  during a turn, only proposed for the user's literal confirmation, then
  replayed with an `idempotency_key`. Make writes idempotent.
- **Identity comes from the host.** `CallContext` designates the org and
  member; trigger events are re-checked against activation and
  membership before anything runs.
- **A plugin may hold the event store.** Set `provides_event_store` and
  implement `PutEvent`/`DeleteEvent`/`ListEvents`, and the reminders of
  every member whose configuration carries
  `pluginsdk.EventStoreConfigKey: true` live in your backend instead of
  the host's table. You own the text and the schedule; the host keeps the
  delivery. Announce a due occurrence with a `TriggerEvent` whose
  `deliver_text` is set — it is sent to the member word for word, with no
  sub-agent turn. Recurrence travels as a 5-field cron expression, the
  host's dialect everywhere: translate it, and refuse with `is_error`
  what your backend cannot express rather than storing an approximation.
- **Per-plugin isolation.** The host binds each broker connection to the
  plugin that established it: you can only ever see your own configs and
  secrets, and only for organizations that activated you.
- **Log with `slog`, and your lines join the host's stream.** `Serve`
  installs a handler whose shape the host actually reads; write to
  `slog` and nothing else, at the level you mean. Unstructured output on
  stderr reaches the host too, but as Debug. Say what failed and why —
  a plugin that swallows the cause of a connection error cannot be
  diagnosed from the other side of a deployment.
- **Certificate exceptions.** `TrustedTLSConfig(serverName, fingerprint)`
  builds the `*tls.Config` for a connection: ordinary verification with an
  empty fingerprint, and with one, a certificate accepted when it verifies
  normally OR matches that exact pin. `InspectTLS` reports what a server
  presents — subject, issuer, SHA-256 — without trusting any of it, so a
  member can see a certificate before accepting it. Never ship a blanket
  `InsecureSkipVerify`: it accepts a machine-in-the-middle just as
  happily.

The bundled `plugins/email` is a complete working example (tools, member
switches, triggers, UI, secret discipline).

## Regenerating the gRPC code

From the repository root: `make generate-proto` (the `.pb.go` files are
committed).
