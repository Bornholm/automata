package profile

import (
	"database/sql"
	"net/http"

	"github.com/bornholm/automata/internal/i18n"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/web/core"
	"github.com/bornholm/automata/internal/web/view"
)

// L'écran « Suggestions » : ce que l'introspection hebdomadaire propose,
// et les trois gestes de la personne — suivre, écarter, tout couper.
//
// Même invariant de cloisonnement que les souvenirs : toute action sur une
// suggestion passe par UpdateStatus filtré (org, membre), la portée venant
// du lien de profil déjà validé. Un identifiant recopié ne modifie rien.

// maxListedSuggestions borne l'affichage.
const maxListedSuggestions = 30

// suggestionKindLabels associe la nature persistée à sa clé de traduction.
var suggestionKindLabels = map[string]string{
	"automation": "suggestions.kind.automation",
	"activation": "suggestions.kind.activation",
	"fix":        "suggestions.kind.fix",
	"habit":      "suggestions.kind.habit",
}

// HandleProfileSuggestions liste les suggestions du membre.
func (h *Handlers) HandleProfileSuggestions(w http.ResponseWriter, r *http.Request) {
	member, r, minutes, ok := h.resolveProfile(w, r)
	if !ok {
		return
	}

	plugins, ok := h.profilePluginUIs(w, r, member)
	if !ok {
		return
	}

	page := view.SuggestionsPage{
		LinkID:    r.PathValue("link"),
		Header:    h.profileHeader(r, member, minutes),
		CSRFToken: h.CSRFToken(w, r),
		PluginUIs: plugins,
		Muted:     member.SuggestionsMuted,
	}

	switch r.URL.Query().Get("done") {
	case "accepted":
		page.Notice = i18n.TC(r.Context(), "suggestions.notice_accepted")
	case "dismissed":
		page.Notice = i18n.TC(r.Context(), "suggestions.notice_dismissed")
	case "muted":
		page.Notice = i18n.TC(r.Context(), "suggestions.notice_muted")
	case "unmuted":
		page.Notice = i18n.TC(r.Context(), "suggestions.notice_unmuted")
	}

	var suggestions []persistence.Suggestion
	if !h.WithTx(w, r, func(tx *sql.Tx) error {
		var err error
		suggestions, err = persistence.NewSuggestionRepository().
			ListByMember(r.Context(), tx, member.OrgID, member.ID, maxListedSuggestions)
		return err
	}) {
		return
	}

	for _, s := range suggestions {
		row := view.SuggestionRow{
			ID:        s.ID,
			Title:     s.Title,
			Body:      s.Body,
			At:        s.CreatedAt.Local().Format("02/01/2006"),
			KindLabel: i18n.TC(r.Context(), suggestionKindLabels[s.Kind]),
			Open:      s.Status == persistence.SuggestionStatusProposed || s.Status == persistence.SuggestionStatusDelivered,
		}
		switch s.Status {
		case persistence.SuggestionStatusAccepted:
			row.Outcome = i18n.TC(r.Context(), "suggestions.outcome.accepted")
		case persistence.SuggestionStatusDismissed:
			row.Outcome = i18n.TC(r.Context(), "suggestions.outcome.dismissed")
		}
		page.Suggestions = append(page.Suggestions, row)
	}

	h.Render(w, r, http.StatusOK, view.ProfileSuggestions(page))
}

// HandleProfileSuggestionAccept marque une suggestion comme suivie.
func (h *Handlers) HandleProfileSuggestionAccept(w http.ResponseWriter, r *http.Request) {
	h.updateSuggestion(w, r, persistence.SuggestionStatusAccepted, "accepted")
}

// HandleProfileSuggestionDismiss écarte une suggestion, définitivement :
// elle figure ensuite dans le dossier de l'introspection avec ce sort, et
// le prompt interdit de la reproposer.
func (h *Handlers) HandleProfileSuggestionDismiss(w http.ResponseWriter, r *http.Request) {
	h.updateSuggestion(w, r, persistence.SuggestionStatusDismissed, "dismissed")
}

func (h *Handlers) updateSuggestion(w http.ResponseWriter, r *http.Request, status, done string) {
	if !core.CheckCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}

	member, r, _, ok := h.resolveProfile(w, r)
	if !ok {
		return
	}

	link := r.PathValue("link")
	id := r.PathValue("id")

	if !h.WithTx(w, r, func(tx *sql.Tx) error {
		// Le filtre (org, membre) d'UpdateStatus est le cloisonnement : la
		// suggestion d'un autre membre reste intacte, silencieusement.
		_, err := persistence.NewSuggestionRepository().
			UpdateStatus(r.Context(), tx, member.OrgID, member.ID, id, status)
		return err
	}) {
		return
	}

	http.Redirect(w, r, "/p/"+link+"/suggestions?done="+done, http.StatusSeeOther)
}

// HandleProfileSuggestionsMute coupe ou réactive l'introspection pour le
// membre. Couper vaut partout : ni collecte, ni suggestion, ni message.
func (h *Handlers) HandleProfileSuggestionsMute(w http.ResponseWriter, r *http.Request) {
	if !core.CheckCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}

	member, r, _, ok := h.resolveProfile(w, r)
	if !ok {
		return
	}

	muted := r.FormValue("muted") == "1"

	if !h.WithTx(w, r, func(tx *sql.Tx) error {
		return h.Members.SetSuggestionsMuted(r.Context(), tx, member.ID, muted)
	}) {
		return
	}

	done := "unmuted"
	if muted {
		done = "muted"
	}
	http.Redirect(w, r, "/p/"+r.PathValue("link")+"/suggestions?done="+done, http.StatusSeeOther)
}
