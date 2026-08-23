package conversation

import "testing"

// Le format d'un lien de profil est strict (six caractères Crockford, un
// point, vingt) : le caviardage doit l'attraper partout où il se présente,
// et ne rien attraper d'autre.
func TestRedactProfileLinks(t *testing.T) {
	const link = "https://automata.example.fr/p/cw2vj0.bq7yaptagka09h9mnt7c"

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "lien seul",
			in:   link,
			want: redactedProfileLink,
		},
		{
			name: "lien dans une phrase",
			in:   "Voici le lien vers ton profil : " + link + "\n\nIl est valable 15 minutes.",
			want: "Voici le lien vers ton profil : " + redactedProfileLink + "\n\nIl est valable 15 minutes.",
		},
		{
			name: "plusieurs liens",
			in:   link + " puis https://automata.example.fr/p/h07jrm.bq7yaptagka09h9mnt7c",
			want: redactedProfileLink + " puis " + redactedProfileLink,
		},
		{
			name: "chemin nu, sans adresse",
			in:   "ouvre /p/cw2vj0.bq7yaptagka09h9mnt7c",
			want: "ouvre " + redactedProfileLink,
		},
		{
			name: "adresse absorbée en entier",
			in:   "http://localhost:5000/p/cw2vj0.bq7yaptagka09h9mnt7c",
			want: redactedProfileLink,
		},
		{
			name: "aucun lien",
			in:   "Ton solde est de 291 crédits.",
			want: "Ton solde est de 291 crédits.",
		},
		{
			name: "une autre URL n'est pas touchée",
			in:   "https://fr.wikipedia.org/wiki/Automate p/court.trop",
			want: "https://fr.wikipedia.org/wiki/Automate p/court.trop",
		},
		{
			name: "segment de mauvaise longueur",
			in:   "/p/abc.def",
			want: "/p/abc.def",
		},
		{
			name: "texte vide",
			in:   "",
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactProfileLinks(tc.in); got != tc.want {
				t.Errorf("redactProfileLinks(%q)\n  = %q\nattendu %q", tc.in, got, tc.want)
			}
		})
	}
}
