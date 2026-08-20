// The email plugin gives Automata a per-member mailbox assistant: IMAP
// reading and search, SMTP sending behind the host's human confirmation,
// and incoming-mail triggers. Configuration and password live host-side,
// sealed at rest, per member.
package main

import "github.com/bornholm/automata/pkg/pluginsdk"

func main() {
	pluginsdk.ServeWithUI(newPlugin(), "email", newUIHandler())
}
