package main

import (
	"encoding/json"
	"fmt"
)

// unmarshalJSON décode une configuration quelconque du service hôte.
func unmarshalJSON(raw string, target any) error {
	if raw == "" {
		return fmt.Errorf("empty configuration")
	}
	return json.Unmarshal([]byte(raw), target)
}

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

	// Exceptions TLS du membre : l'empreinte SHA-256 du certificat qu'il a
	// explicitement accepté, après l'avoir vu. Vide — le cas normal —
	// donne la vérification habituelle. Une exception vaut pour CE
	// certificat et pour ce serveur : les deux sont distinctes parce que
	// la messagerie entrante et la sortante peuvent être deux machines.
	IMAPTLSFingerprint string `json:"imap_tls_fingerprint,omitempty"`
	SMTPTLSFingerprint string `json:"smtp_tls_fingerprint,omitempty"`

	// AuthMode is "password" (default) or "oauth" for a Google mailbox
	// connected through the consent screen.
	AuthMode string `json:"auth_mode,omitempty"`

	// AllowRead exposes the reading tools to the agent; AllowWrite
	// exposes the sending tools. Sending ALWAYS goes through the host's
	// human confirmation regardless of these switches — they only decide
	// what the agent can see.
	AllowRead  bool `json:"allow_read"`
	AllowWrite bool `json:"allow_write"`

	// PollSeconds is the mailbox polling interval; 0 keeps the default.
	PollSeconds int `json:"poll_seconds"`

	// Instructions is the member's own standing orders for incoming mail,
	// in their own words: which senders to ignore, which ones matter, what
	// to do about them. It is appended to the sub-agent's brief on every
	// triggered turn.
	//
	// Free text rather than a rule engine, deliberately: a mailbox's
	// exceptions are endless ("ignore the newsletters, except the union
	// one"), and no form would ever hold them. What the member writes here
	// is read by the model, so it is guidance — not a filter, and never a
	// security boundary.
	Instructions string `json:"instructions,omitempty"`

	// ProcessedLabel is the IMAP keyword set on a message once Automata has
	// dealt with it. Empty keeps the default; the label is what lets the
	// member see, in their own mail client, what was handled — without
	// Automata ever marking anything as read.
	ProcessedLabel string `json:"processed_label,omitempty"`

	// LastUID is the watcher cursor: highest UID already announced.
	LastUID uint32 `json:"last_uid"`
	// CursorReady records that the watcher already anchored itself at the
	// end of the mailbox. Without it, an empty inbox would re-anchor on
	// every poll and swallow the first email ever received.
	CursorReady bool `json:"cursor_ready"`
}

const defaultPollSeconds = 120

// defaultProcessedLabel est le mot-clé IMAP posé sur un message traité.
//
// Marquer « lu » serait plus simple, et c'est ce que faisait le plugin : un
// message que personne n'a ouvert se retrouvait lu, la boîte perdait son
// compteur de non-lus, et rien ne distinguait ce qu'Automata avait vu de ce
// que la personne avait vraiment lu. Un mot-clé propre dit exactement cela,
// et se voit dans la plupart des clients de messagerie.
const defaultProcessedLabel = "Automata"

// processedLabel retourne le mot-clé effectif.
func (c memberConfig) processedLabel() string {
	if c.ProcessedLabel == "" {
		return defaultProcessedLabel
	}
	return c.ProcessedLabel
}

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

// oauth indique si le compte s'authentifie par jeton Google.
func (c memberConfig) oauth() bool { return c.AuthMode == authModeOAuth }

// googleDefaults renseigne les serveurs Gmail : un compte connecté par
// consentement n'a aucune raison de les saisir à la main.
func (c memberConfig) googleDefaults(address string) memberConfig {
	c.AuthMode = authModeOAuth
	c.IMAPHost, c.IMAPPort = googleIMAPHost, googleIMAPPort
	c.SMTPHost, c.SMTPPort = googleSMTPHost, googleSMTPPort
	c.IMAPInsecure, c.SMTPInsecure = false, false
	if address != "" {
		c.Username, c.From = address, address
	}
	return c
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
