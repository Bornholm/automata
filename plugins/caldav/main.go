// The caldav plugin gives Automata a per-member calendar assistant:
// reading and searching a CalDAV agenda, creating events behind the host's
// human confirmation, and — when the member asks for it — holding their
// reminders in that calendar instead of the host's own table.
package main

import "github.com/bornholm/automata/pkg/pluginsdk"

func main() {
	pluginsdk.ServeWithUI(newPlugin(), "caldav", newUIHandler())
}
