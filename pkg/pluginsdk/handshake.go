package pluginsdk

import "github.com/hashicorp/go-plugin"

// PluginName is the dispense key shared by host and plugins.
const PluginName = "automata_plugin"

// HandshakeConfig is shared between the Automata host and plugin binaries.
// ProtocolVersion must be incremented whenever the gRPC contract changes
// incompatibly.
var HandshakeConfig = plugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   "AUTOMATA_PLUGIN",
	MagicCookieValue: "automata-plugin-v1",
}
