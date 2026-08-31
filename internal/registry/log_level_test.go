package registry

import (
	"log/slog"
	"os"
	"testing"
)

// Le niveau passé à whatsmeow suit celui de l'instance. Il ne doit surtout
// pas retomber sur DEBUG : whatsmeow écrit alors une paire de trames de
// maintien toutes les vingt-cinq secondes, sur la sortie standard et hors du
// journal structuré. Le 2026-08-31, ce bruit a enseveli les deux seules
// lignes qui expliquaient une panne de Rocket.Chat.
func TestWhatsmeowLogLevel_FollowsTheInstanceLogger(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })

	for _, tc := range []struct {
		level    slog.Level
		expected string
	}{
		{slog.LevelDebug, "DEBUG"},
		{slog.LevelInfo, "INFO"},
		{slog.LevelWarn, "WARN"},
		{slog.LevelError, "ERROR"},
	} {
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: tc.level})))

		if got := whatsmeowLogLevel(); got != tc.expected {
			t.Errorf("logger en %s : niveau = %q, attendu %q", tc.level, got, tc.expected)
		}
	}
}

// Le logger par défaut d'Automata (cmd/automata/main.go, options nil) est en
// INFO : c'est ce que doit recevoir whatsmeow en exploitation courante.
func TestWhatsmeowLogLevel_DefaultIsInfo(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	if got := whatsmeowLogLevel(); got != "INFO" {
		t.Errorf("niveau = %q, attendu INFO", got)
	}
}
