// The workspace plugin gives Automata a sandboxed shell workspace: the
// sub-agent can import a file the user attached to a message, transform it
// with ffmpeg or imagemagick inside a LeaSH sandbox, and send the result
// back as an attachment.
//
// The plugin itself executes nothing: it runs inside Automata's distroless
// container and is a thin bridge to a LeaSH server reachable over the
// internal network (LEASH_SERVER_URL, LEASH_API_KEY).
package main

import "github.com/bornholm/automata/pkg/pluginsdk"

func main() {
	pluginsdk.Serve(newPlugin(newLeashClientFromEnv()))
}
