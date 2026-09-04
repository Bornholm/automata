package agent

import (
	"strings"
	"testing"

	"github.com/bornholm/genai/llm"
)

// Le tour est rejoué, pas poursuivi : la demande de la personne revient
// avec la consigne, et le brouillon fautif n'apparaît nulle part. Le lui
// remontrer serait lui donner le texte à imiter — ce modèle imite le
// dernier message d'assistant qu'il voit.
func TestReplayWithNotice_KeepsTheRequestAndDropsTheDraft(t *testing.T) {
	messages := []llm.Message{
		llm.NewMessage(llm.RoleSystem, "règles"),
		llm.NewMessage(llm.RoleUser, "Analyse le certificat de example.test"),
		llm.NewMessage(llm.RoleAssistant, "Voici l'analyse."),
		llm.NewMessage(llm.RoleUser, "Essaye sur le 465"),
	}

	replay := replayWithNotice(messages, "NOTICE")

	if len(replay) != len(messages) {
		t.Fatalf("%d messages, attendu %d : le rejeu n'ouvre pas de tour de plus", len(replay), len(messages))
	}

	last := replay[len(replay)-1]
	if last.Role() != llm.RoleUser {
		t.Fatalf("le dernier message devrait rester celui de la personne, obtenu %q", last.Role())
	}
	if !strings.Contains(last.Content(), "Essaye sur le 465") {
		t.Errorf("la demande a été perdue: %q", last.Content())
	}
	if !strings.Contains(last.Content(), "NOTICE") {
		t.Errorf("la consigne n'a pas été intégrée: %q", last.Content())
	}

	// L'historique antérieur reste intact.
	if replay[2].Content() != "Voici l'analyse." {
		t.Errorf("l'historique a été altéré: %q", replay[2].Content())
	}
}

// Sans message de la personne en dernier — aucun chemin ne le produit
// aujourd'hui — la consigne s'ajoute derrière plutôt que de se fondre dans
// un message d'un autre rôle.
func TestReplayWithNotice_AppendsWhenLastIsNotTheUser(t *testing.T) {
	messages := []llm.Message{
		llm.NewMessage(llm.RoleSystem, "règles"),
		llm.NewMessage(llm.RoleAssistant, "brouillon"),
	}

	replay := replayWithNotice(messages, "NOTICE")

	if len(replay) != 3 {
		t.Fatalf("%d messages, attendu 3", len(replay))
	}
	if replay[2].Role() != llm.RoleUser || replay[2].Content() != "NOTICE" {
		t.Errorf("consigne mal ajoutée: %q / %q", replay[2].Role(), replay[2].Content())
	}
}

func TestReplayWithNotice_EmptyMessages(t *testing.T) {
	replay := replayWithNotice(nil, "NOTICE")

	if len(replay) != 1 || replay[0].Content() != "NOTICE" {
		t.Fatalf("rejeu inattendu sur un tour vide: %+v", replay)
	}
}

// Les deux consignes s'adressent au modèle comme des notes du système, et
// lui demandent d'écrire À LA PERSONNE. Sans cela, il répond au correcteur :
// « Tu as raison, j'ai inventé les détails. Je le fais ? » est arrivé à un
// membre le 2026-09-04, à la place de sa réponse.
func TestNotices_TellTheModelWhoItAnswers(t *testing.T) {
	for name, notice := range map[string]string{
		"juge":     ungroundedNotice,
		"adresses": unsourcedURLNotice,
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(notice, "Note from the system") {
				t.Error("la consigne ne se distingue pas d'un message de la personne")
			}
			if !strings.Contains(notice, "Write to the person") {
				t.Error("la consigne ne dit pas à qui répondre")
			}
			if !strings.Contains(notice, "Do not mention this note") {
				t.Error("rien n'interdit de commenter la consigne dans la réponse")
			}
			if !strings.Contains(notice, "do not ask permission") {
				t.Error("rien n'interdit de redemander la permission d'appeler un outil")
			}
			// La note parle d'un brouillon écarté, jamais d'une réponse
			// « ci-dessus » : le modèle ne la voit plus.
			if strings.Contains(notice, "reply above") || strings.Contains(notice, "you just wrote") {
				t.Error("la consigne renvoie à un brouillon que le modèle ne voit plus")
			}
		})
	}
}
