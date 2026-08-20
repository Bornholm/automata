package main

import (
	"encoding/json"
	"fmt"
)

// memberConfig is the per-member configuration stored by the host
// (sealed at rest). Secrets never live here: the password goes through
// SetSecret only.
type memberConfig struct {
	IMAPHost string `json:"imap_host"`
	IMAPPort int    `json:"imap_port"`
	SMTPHost string `json:"smtp_host"`
	SMTPPort int    `json:"smtp_port"`
	Username string `json:"username"`
	From     string `json:"from_address"`

	// IMAPInsecure / SMTPInsecure désactivent TLS — réservés aux serveurs
	// de test locaux, jamais exposés dans l'interface.
	IMAPInsecure bool `json:"imap_insecure,omitempty"`
	SMTPInsecure bool `json:"smtp_insecure,omitempty"`

	// AllowRead exposes the reading tools to the agent; AllowWrite
	// exposes the sending tools. Sending ALWAYS goes through the host's
	// human confirmation regardless of these switches — they only decide
	// what the agent can see.
	AllowRead  bool `json:"allow_read"`
	AllowWrite bool `json:"allow_write"`

	// PollSeconds is the mailbox polling interval; 0 keeps the default.
	PollSeconds int `json:"poll_seconds"`

	// LastUID is the watcher cursor: highest UID already announced.
	LastUID uint32 `json:"last_uid"`
	// CursorReady records that the watcher already anchored itself at the
	// end of the mailbox. Without it, an empty inbox would re-anchor on
	// every poll and swallow the first email ever received.
	CursorReady bool `json:"cursor_ready"`
}

const defaultPollSeconds = 120

// secretKeyPassword is the single secret of an account: virtually every
// mailbox uses the same password for IMAP and SMTP submission.
const secretKeyPassword = "password"

func parseConfig(raw string) (memberConfig, error) {
	var cfg memberConfig
	if raw == "" {
		return cfg, fmt.Errorf("no mailbox configured")
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return cfg, fmt.Errorf("unreadable configuration")
	}
	return cfg, nil
}

func (c memberConfig) complete() bool {
	return c.IMAPHost != "" && c.IMAPPort > 0 && c.Username != ""
}

func (c memberConfig) sendComplete() bool {
	return c.SMTPHost != "" && c.SMTPPort > 0 && c.From != ""
}

func (c memberConfig) pollInterval() int {
	if c.PollSeconds > 0 {
		return c.PollSeconds
	}
	return defaultPollSeconds
}

func (c memberConfig) marshal() string {
	raw, _ := json.Marshal(c)
	return string(raw)
}
