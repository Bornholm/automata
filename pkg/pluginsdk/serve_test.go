package pluginsdk

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
	"time"
)

// A plugin's structured log must come out in the shape go-plugin parses.
// Anything else is forwarded whole, as a Debug message, and dropped by a
// host running at INFO — which is how a plugin that logged an error on
// every failed connection looked completely silent.
func TestHclogKeys_ShapeGoPluginParses(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{
		Level:       slog.LevelDebug,
		ReplaceAttr: hclogKeys,
	}))

	logger.Warn("connexion refusée", "server", "imap.example.org", "attempt", 2)

	var entry map[string]any
	if err := json.Unmarshal(out.Bytes(), &entry); err != nil {
		t.Fatalf("ligne illisible: %v (%s)", err, out.String())
	}

	if entry["@message"] != "connexion refusée" {
		t.Errorf("@message absent ou faux: %v", entry["@message"])
	}
	if entry["@level"] != "warn" {
		t.Errorf("@level = %v, attendu warn", entry["@level"])
	}
	// Le format d'horodatage est le seul que go-plugin accepte : tout autre
	// rend la ligne ENTIÈRE inanalysable, niveau compris.
	stamp, _ := entry["@timestamp"].(string)
	if _, err := time.Parse(hclogTimeFormat, stamp); err != nil {
		t.Errorf("@timestamp %q illisible par go-plugin: %v", stamp, err)
	}

	// Les clés de slog ne doivent plus apparaître : go-plugin les
	// prendrait pour des paires métier.
	for _, key := range []string{slog.TimeKey, slog.LevelKey, slog.MessageKey} {
		if _, present := entry[key]; present {
			t.Errorf("clé slog %q encore présente", key)
		}
	}

	// Les attributs du plugin voyagent tels quels.
	if entry["server"] != "imap.example.org" {
		t.Errorf("attribut perdu: %v", entry["server"])
	}
}

func TestHclogLevelName(t *testing.T) {
	cases := map[slog.Level]string{
		slog.LevelDebug - 4: "trace",
		slog.LevelDebug:     "debug",
		slog.LevelInfo:      "info",
		slog.LevelWarn:      "warn",
		slog.LevelError:     "error",
		slog.LevelError + 4: "error",
	}

	for level, want := range cases {
		if got := hclogLevelName(level); got != want {
			t.Errorf("hclogLevelName(%v) = %q, attendu %q", level, got, want)
		}
	}
}

// Un attribut groupé garde son nom : seul le sommet du document porte les
// clés réservées de go-plugin.
func TestHclogKeys_LeavesGroupedAttributesAlone(t *testing.T) {
	attr := slog.String(slog.MessageKey, "dans un groupe")
	if got := hclogKeys([]string{"details"}, attr); got.Key != slog.MessageKey {
		t.Errorf("attribut groupé renommé en %q", got.Key)
	}
}
