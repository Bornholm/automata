package admin

import (
	"database/sql"
	"net/http"
	"net/url"

	"github.com/bornholm/automata/internal/web/core"
)

// Détacher un canal d'une organisation.
//
// C'est le seul geste de l'onglet Canaux. Un groupe rattaché au mauvais
// foyer, un groupe dissous, un jeton envoyé dans la mauvaise conversation :
// jusqu'ici la seule issue était SQL sur le volume. Le rattachement, lui,
// ne se fait pas d'ici mais par jeton, depuis la conversation, parce que
// c'est la conversation qui doit prouver qu'elle existe.
//
// Détacher n'efface rien. La conversation et ses messages restent en
// base ; Automata cesse simplement d'y répondre. L'effacement a ses
// propres boutons, sur le membre (RGPD) ou sur l'organisation entière.

// HandleOrgChannelUnbind détache un canal rattaché en ligne. Les canaux
// déclarés dans le fichier de configuration n'ont pas de bouton : ils se
// retirent en éditant le fichier, et ce handler ne les connaît pas.
func (h *Handlers) HandleOrgChannelUnbind(w http.ResponseWriter, r *http.Request) {
	if !core.CheckCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}

	orgID := r.PathValue("id")
	provider := r.FormValue("provider")
	channelID := r.FormValue("channel_id")
	if provider == "" || channelID == "" {
		http.Redirect(w, r, "/admin/orgs/"+orgID+"?tab=channels", http.StatusFound)
		return
	}

	var detached bool
	if !h.WithTx(w, r, func(tx *sql.Tx) error {
		var err error
		// Le filtre par organisation est dans Delete : un canal d'une
		// autre organisation ne peut pas être détaché depuis cette fiche.
		detached, err = h.Bindings.Delete(r.Context(), tx, orgID, provider, channelID)
		return err
	}) {
		return
	}

	if !detached {
		http.Redirect(w, r, "/admin/orgs/"+orgID+"?tab=channels&error="+
			url.QueryEscape("Ce canal n'est pas rattaché à cette organisation."), http.StatusFound)
		return
	}

	// Le résolveur d'identité relit la base à chaque message : le canal
	// est ignoré dès le suivant, sans redémarrage. Le journal ne porte que
	// des identifiants.
	h.Logger.InfoContext(r.Context(), "web: canal détaché",
		"org_id", orgID, "provider", provider, "channel_id", channelID)

	http.Redirect(w, r, "/admin/orgs/"+orgID+"?tab=channels", http.StatusFound)
}
