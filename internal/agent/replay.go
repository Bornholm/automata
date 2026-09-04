package agent

import (
	"strings"

	"github.com/bornholm/genai/llm"
)

// Rejouer la demande, sans le brouillon.
//
// Une relecture qui échoue — le juge, ou le contrôle des adresses — rend le
// tour au modèle. La première version le faisait en poursuivant la
// conversation : le brouillon fautif était ajouté comme sa réponse, puis la
// consigne comme un nouveau message.
//
// C'était donner au modèle le texte même qu'il ne doit pas reproduire. La
// sonde l'a mesuré (automata admin probe-tools) : ce modèle imite le dernier
// message d'assistant qu'il voit, trois fois sur six quand celui-ci est un
// refus. Vu en production le 2026-09-04, où une relance a redemandé la
// permission d'appeler l'outil au lieu de l'appeler, la consigne
// « CALL IT NOW » sous les yeux.
//
// Le tour est donc REJOUÉ : mêmes règles, même historique, même demande, à
// laquelle s'ajoute la consigne. Le brouillon n'existe pour personne — ce
// qui n'est pas montré ne peut pas être recopié, exactement comme pour les
// liens de profil caviardés (internal/conversation/redact.go).
//
// L'information nécessaire n'est pas perdue pour autant : la consigne NOMME
// ce qui était fautif, et c'est ce qui la rend utilisable sans le texte
// d'origine.

// replayWithNotice reconstruit les messages du tour en y intégrant notice.
//
// La consigne rejoint la demande de la personne dans le même message
// utilisateur, plutôt que d'en ouvrir un second : le modèle rejoue UN tour,
// il n'en poursuit pas une conversation à deux temps. Si le dernier message
// n'est pas celui de la personne — cas qu'aucun chemin ne produit
// aujourd'hui, mais que rien n'interdit demain — la consigne est ajoutée
// derrière plutôt que fondue dans un message d'un autre rôle.
func replayWithNotice(messages []llm.Message, notice string) []llm.Message {
	replay := append([]llm.Message(nil), messages...)
	if len(replay) == 0 {
		return append(replay, llm.NewMessage(llm.RoleUser, notice))
	}

	last := replay[len(replay)-1]
	if last.Role() != llm.RoleUser {
		return append(replay, llm.NewMessage(llm.RoleUser, notice))
	}

	replay[len(replay)-1] = llm.NewMessage(llm.RoleUser,
		strings.TrimSpace(last.Content())+"\n\n---\n\n"+notice)

	return replay
}
