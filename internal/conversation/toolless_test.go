package conversation

import "testing"

// Le constat était accolé aux messages de l'assistant dans l'historique.
// Le modèle l'a recopié dans sa réponse, TRONQUÉ de son crochet fermant, et
// la personne l'a lu (2026-09-03). Le nettoyage doit donc reconnaître le
// marqueur altéré — c'est la comparaison exacte qui a laissé passer.
func TestStripToollessMarker_ToleratesAlteredCopies(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			// La forme exacte observée en production, sans crochet fermant.
			name: "crochet fermant absent",
			in:   "Le service de calendrier est indisponible. Réessayez plus tard. [no tool was called for this message",
			want: "Le service de calendrier est indisponible. Réessayez plus tard.",
		},
		{
			name: "forme complète",
			in:   "Le service est indisponible. [no tool was called for this message]",
			want: "Le service est indisponible.",
		},
		{
			name: "casse changée",
			in:   "Indisponible. [No Tool Was Called for this message]",
			want: "Indisponible.",
		},
		{
			name: "fin reformulée",
			in:   "Indisponible. [no tool was called when writing this reply]",
			want: "Indisponible.",
		},
		{
			name: "au milieu de la phrase",
			in:   "Indisponible [no tool was called for this message] pour l'instant.",
			want: "Indisponible pour l'instant.",
		},
		{
			name: "rien à retirer",
			in:   "Voici votre lien : https://exemple.test/p/aaaaaa.bbbbbbbbbbbbbbbbbbbb",
			want: "Voici votre lien : https://exemple.test/p/aaaaaa.bbbbbbbbbbbbbbbbbbbb",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripToollessMarker(tc.in); got != tc.want {
				t.Errorf("stripToollessMarker(%q)\n  = %q\nattendu %q", tc.in, got, tc.want)
			}
		})
	}
}

// Le nettoyage ne dévore pas le reste du message : le motif s'arrête au
// crochet fermant, ou à la fin de ce qu'il peut raisonnablement absorber.
func TestStripToollessMarker_StopsAtTheClosingBracket(t *testing.T) {
	in := "Indisponible. [no tool was called for this message] Voici la suite, qui doit rester."
	want := "Indisponible. Voici la suite, qui doit rester."

	if got := stripToollessMarker(in); got != want {
		t.Fatalf("stripToollessMarker a dévoré la suite:\n  = %q\nattendu %q", got, want)
	}
}
