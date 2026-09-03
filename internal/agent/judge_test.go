package agent

import (
	"strings"
	"testing"
)

// L'avis du juge est du JSON contraint par un schéma. Un modèle qui
// l'entoure d'une clôture Markdown reste lisible : jeter un avis pour trois
// backticks priverait le tour de sa relecture.
func TestParseGrounding(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    Grounding
		wantErr bool
	}{
		{
			name: "avis négatif",
			raw:  `{"grounded": false, "reason": "you stated the calendar service is unavailable, but you never called a calendar tool"}`,
			want: Grounding{Grounded: false, Reason: "you stated the calendar service is unavailable, but you never called a calendar tool"},
		},
		{
			name: "avis positif",
			raw:  `{"grounded": true, "reason": ""}`,
			want: Grounding{Grounded: true},
		},
		{
			name: "clôture markdown",
			raw:  "```json\n{\"grounded\": false, \"reason\": \"you claimed to have sent it\"}\n```",
			want: Grounding{Grounded: false, Reason: "you claimed to have sent it"},
		},
		{
			name:    "texte libre",
			raw:     "Je pense que la réponse est correcte.",
			wantErr: true,
		},
		{
			name:    "vide",
			raw:     "   ",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseGrounding(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("erreur attendue, obtenu %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGrounding: %v", err)
			}
			if got != tc.want {
				t.Errorf("parseGrounding(%q)\n  = %+v\nattendu %+v", tc.raw, got, tc.want)
			}
		})
	}
}

// Le juge ne reçoit ni l'historique ni les outils : deux textes encadrés,
// et rien d'autre. Un modèle qui reçoit deux textes collés en juge un seul.
func TestJudgePrompt_FramesBothTexts(t *testing.T) {
	prompt := judgePrompt("Quels sont mes rendez-vous ?", "Le service de calendrier est indisponible.")

	for _, want := range []string{
		"<user_request>", "</user_request>",
		"<assistant_reply>", "</assistant_reply>",
		"Quels sont mes rendez-vous ?",
		"Le service de calendrier est indisponible.",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("le prompt du juge ne contient pas %q:\n%s", want, prompt)
		}
	}
}

// La consigne du juge borne ce qu'il juge. Sans cette borne, un modèle à
// qui l'on demande « est-ce une bonne réponse ? » commente le ton et la
// longueur, et déclenche des relances sur des réponses honnêtes.
func TestJudgeSystemPrompt_StaysWithinItsQuestion(t *testing.T) {
	if !strings.Contains(JudgeSystemPrompt, "called NO tool") {
		t.Error("la consigne ne donne pas au juge le fait sur lequel il s'appuie")
	}
	if !strings.Contains(JudgeSystemPrompt, "Never comment on tone, length, style or usefulness") {
		t.Error("la consigne ne borne pas le juge à sa seule question")
	}
	// Les cas honnêtes sont nommés : sans eux, un refus sincère (« je ne
	// peux pas, je n'ai pas essayé ») serait jugé non fondé.
	if !strings.Contains(JudgeSystemPrompt, "did not try") {
		t.Error("la consigne ne protège pas le refus honnête")
	}
}

// La relance transporte la RAISON du juge. « Ta réponse n'est pas fondée »
// n'apprend rien au modèle, qui recommence à l'identique.
func TestUngroundedNotice_CarriesTheReason(t *testing.T) {
	const reason = "you stated the calendar service is unavailable, but you never called a calendar tool"
	notice := strings.Replace(ungroundedNotice, "%s", reason, 1)

	if !strings.Contains(notice, reason) {
		t.Fatalf("la consigne ne transporte pas la raison: %q", notice)
	}
	if strings.Contains(notice, "%s") {
		t.Error("le gabarit n'a pas été substitué")
	}
	if !strings.Contains(notice, "CALL IT NOW") {
		t.Error("la consigne ne rouvre pas l'appel d'outil")
	}
	if !strings.Contains(notice, "say plainly what you did not do") {
		t.Error("la consigne ne laisse pas l'issue honnête")
	}
}
