package main

import (
	"strings"
	"testing"
)

// Le message trompeur de yt-dlp (« This video is not available » alors que
// la cause est l'absence de runtime JavaScript) doit produire une consigne
// qui INTERDIT de rejouer la même vidéo sous d'autres formes d'URL : c'est
// ce que l'agent a fait douze fois en production, avant d'annoncer à
// l'utilisateur un diagnostic inventé — « vidéo privée ou géo-restreinte »
// — pour une vidéo parfaitement publique.
func TestDownloadFailureAdvice(t *testing.T) {
	missingRuntime := "## STDERR\nWARNING: [youtube] No supported JavaScript runtime could be found. " +
		"Only deno is enabled by default\nERROR: [youtube] xxx: This video is not available\n"

	advice := downloadFailureAdvice(missingRuntime)
	if advice == "" {
		t.Fatal("aucune consigne pour l'absence de runtime JavaScript")
	}
	for _, want := range []string{"fault in the download tool", "Do NOT retry"} {
		if !strings.Contains(advice, want) {
			t.Errorf("consigne %q : %q attendu", advice, want)
		}
	}

	// Une vraie indisponibilité, elle, mérite une autre conclusion.
	if downloadFailureAdvice("ERROR: [youtube] xxx: Private video. Sign in if you've been granted access") == "" {
		t.Error("aucune consigne pour une vidéo réellement privée")
	}

	// Rien de connu : on ne surinterprète pas, la sortie brute suffit.
	if got := downloadFailureAdvice("ERROR: some unexpected failure"); got != "" {
		t.Errorf("consigne %q pour une erreur inconnue, attendue vide", got)
	}
}
