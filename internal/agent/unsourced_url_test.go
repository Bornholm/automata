package agent

import (
	"strings"
	"testing"
)

// Une adresse qu'aucune source du tour ne contient a été composée par le
// modèle. Vu en production le 2026-09-03 : « Voici votre lien :
// https://profile.cadoles.com/?t=8c2a4f9d1e7b5c3a », entièrement inventé,
// alors qu'open_profile_link n'avait pas été appelé.
func TestUnsourcedURLs(t *testing.T) {
	const historique = "Voici votre lien : https://automata.test/p/aaaaaa.bbbbbbbbbbbbbbbbbbbb"
	const resultatOutil = "Private profile link: https://automata.test/p/cccccc.dddddddddddddddddddd"

	cases := []struct {
		name  string
		reply string
		want  []string
	}{
		{
			name:  "adresse fabriquée",
			reply: "Voici votre lien : https://profile.cadoles.com/?t=8c2a4f9d1e7b5c3a",
			want:  []string{"https://profile.cadoles.com/?t=8c2a4f9d1e7b5c3a"},
		},
		{
			// Celle-là vient de l'outil appelé pendant le tour : elle est
			// légitime, et c'est le cas nominal.
			name:  "adresse rendue par un outil",
			reply: "Voici votre lien : https://automata.test/p/cccccc.dddddddddddddddddddd",
			want:  nil,
		},
		{
			// Recopier un lien de l'historique est un autre défaut (le lien
			// est périmé), traité par le caviardage : ce n'est pas une
			// invention, et ce contrôle-ci n'a pas à s'en mêler.
			name:  "adresse déjà présente dans la conversation",
			reply: "Je te redonne : https://automata.test/p/aaaaaa.bbbbbbbbbbbbbbbbbbbb",
			want:  nil,
		},
		{
			// La ponctuation finale ne fait pas une autre adresse.
			name:  "même adresse, ponctuée",
			reply: "C'est ici (https://automata.test/p/cccccc.dddddddddddddddddddd).",
			want:  nil,
		},
		{
			name:  "aucune adresse",
			reply: "Je n'ai pas pu générer de lien.",
			want:  nil,
		},
		{
			name:  "deux fabriquées, une légitime",
			reply: "Essaie https://a.invalid/x puis https://b.invalid/y, sinon https://automata.test/p/cccccc.dddddddddddddddddddd",
			want:  []string{"https://a.invalid/x", "https://b.invalid/y"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := unsourcedURLs(tc.reply, historique, resultatOutil)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("unsourcedURLs(%q)\n  = %v\nattendu %v", tc.reply, got, tc.want)
			}
		})
	}
}

// La consigne de relance nomme les adresses en cause : sans elles, le
// modèle choisit lui-même laquelle corriger, et il choisit mal.
func TestUnsourcedURLNotice_NamesTheAddresses(t *testing.T) {
	notice := strings.Replace(unsourcedURLNotice, "%s", "https://a.invalid/x", 1)

	if !strings.Contains(notice, "https://a.invalid/x") {
		t.Fatalf("la consigne ne nomme pas l'adresse: %q", notice)
	}
	if strings.Contains(notice, "%s") {
		t.Error("le gabarit n'a pas été substitué")
	}
	// Deux issues, et deux seulement : appeler l'outil, ou répondre sans
	// l'adresse. « Vérifie qu'elle est légitime » serait une déclaration,
	// pas une preuve.
	if !strings.Contains(notice, "CALL IT NOW") {
		t.Error("la consigne ne demande pas d'appeler l'outil")
	}
	if !strings.Contains(notice, "without that address") {
		t.Error("la consigne n'offre pas l'issue honnête")
	}
}
