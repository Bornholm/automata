// The pages plugin lets the assistant build small web pages — an
// invitation, a photo page, a mini-site — in a per-member draft space,
// then publish them behind the user's confirmation under a short public
// link served by the host. The files live in the host's plugin object
// store; the plugin itself keeps no state.
package main

import "github.com/bornholm/automata/pkg/pluginsdk"

func main() {
	pluginsdk.ServeWithUI(newPlugin(), "pages", newUIHandler())
}
