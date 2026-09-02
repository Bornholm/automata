package agent

import "testing"

// Les modèles à réflexion font précéder leur réponse de leur raisonnement.
// Selon le fournisseur, il ne reste que les sauts de ligne qui l'entouraient
// — le message part alors sur WhatsApp avec des lignes vides avant le
// premier mot — ou le bloc <think> lui-même.
func TestCleanReply(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "sauts de ligne laissés par un raisonnement séparé",
			in:   "\n\nCoucou Alice ! Comment ça va ?",
			want: "Coucou Alice ! Comment ça va ?",
		},
		{
			name: "bloc de raisonnement non séparé",
			in:   "<think>L'utilisateur dit bonjour, je réponds brièvement.</think>\n\nCoucou !",
			want: "Coucou !",
		},
		{
			name: "bloc multiligne et balise alternative",
			in:   "<reasoning>\nligne 1\nligne 2\n</reasoning>Bonjour",
			want: "Bonjour",
		},
		{
			name: "réponse ordinaire inchangée",
			in:   "Voici la météo :\n- 22 °C\n- ciel dégagé",
			want: "Voici la météo :\n- 22 °C\n- ciel dégagé",
		},
		{
			name: "balise au milieu : c'est du contenu, pas du raisonnement",
			in:   "Pour masquer un bloc, écris <think>ceci</think> dans ta page.",
			want: "Pour masquer un bloc, écris <think>ceci</think> dans ta page.",
		},
		{
			name: "balise jamais refermée : flux tronqué, on ne vide pas la réponse",
			in:   "<think>je réfléchis et la connexion coupe",
			want: "<think>je réfléchis et la connexion coupe",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cleanReply(tc.in); got != tc.want {
				t.Errorf("cleanReply(%q) = %q, attendu %q", tc.in, got, tc.want)
			}
		})
	}
}
