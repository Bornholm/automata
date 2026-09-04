package profile

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/bornholm/automata/internal/i18n"
	"github.com/bornholm/automata/internal/web/core"
	"github.com/bornholm/automata/internal/web/view"
)

// isDeletionConfirmed vérifie le mot que la personne doit recopier pour
// effacer ses données.
//
// La page n'en affiche qu'un, dans SA langue. Les trois sont pourtant
// acceptés, pour la même raison que les mots de confirmation d'action : ce
// qui compte est le geste délibéré de recopier un mot qu'on n'écrit jamais
// par accident, et « DELETE » tapé sur une page espagnole en est un tout
// autant que « BORRAR ». Refuser l'un des trois n'ajouterait aucune
// sécurité, seulement une impasse.
func isDeletionConfirmed(typed string) bool {
	typed = strings.ToUpper(strings.TrimSpace(typed))
	for _, locale := range i18n.Supported {
		if typed == i18n.T(locale, "privacy.delete_keyword") {
			return true
		}
	}
	return false
}

// HandleProfilePrivacy — PRO-04.
func (h *Handlers) HandleProfilePrivacy(w http.ResponseWriter, r *http.Request) {
	member, r, minutes, ok := h.resolveProfile(w, r)
	if !ok {
		return
	}

	page := view.PrivacyPage{
		LinkID:    r.PathValue("link"),
		Header:    h.profileHeader(r, member, minutes),
		CSRFToken: h.CSRFToken(w, r),
	}

	plugins, ok := h.profilePluginUIs(w, r, member)
	if !ok {
		return
	}
	page.PluginUIs = plugins

	if h.Privacy != nil {
		// L'inventaire vient de l'export lui-même : ce que la page annonce
		// est exactement ce que le fichier contient.
		if export, err := h.Privacy.Export(r.Context(), member.ID); err == nil {
			page.Items = []view.PrivacyItem{
				{
					Title:  i18n.TC(r.Context(), "privacy.item.messages.title"),
					Detail: i18n.TC(r.Context(), "privacy.item.messages.detail"),
					Count:  i18n.TN(r.Context(), "privacy.count.messages", len(export.Messages)),
				},
				{
					Title:  i18n.TC(r.Context(), "privacy.item.memories.title"),
					Detail: i18n.TC(r.Context(), "privacy.item.memories.detail"),
					Count:  i18n.TN(r.Context(), "privacy.count.memories", len(export.Memories)),
				},
				{
					Title:  i18n.TC(r.Context(), "privacy.item.usage.title"),
					Detail: i18n.TC(r.Context(), "privacy.item.usage.detail"),
					Count:  i18n.TN(r.Context(), "privacy.count.months", len(export.Usage)),
				},
			}
			if export.Member.Email != "" {
				page.Items = append(page.Items, view.PrivacyItem{
					Title:  i18n.TC(r.Context(), "privacy.item.email.title"),
					Detail: i18n.TC(r.Context(), "privacy.item.email.detail", export.Member.Email),
				})
			}
		}
	}

	if r.URL.Query().Get("erreur") == "confirmation" {
		page.Error = i18n.TC(r.Context(), "privacy.error_confirmation", i18n.TC(r.Context(), "privacy.delete_keyword"))
	}

	h.Render(w, r, http.StatusOK, view.ProfilePrivacy(page))
}

// HandleProfileExport sert l'export des données en JSON.
func (h *Handlers) HandleProfileExport(w http.ResponseWriter, r *http.Request) {
	member, r, _, ok := h.resolveProfile(w, r)
	if !ok {
		return
	}
	if h.Privacy == nil {
		http.NotFound(w, r)
		return
	}

	export, err := h.Privacy.Export(r.Context(), member.ID)
	if err != nil {
		h.Logger.ErrorContext(r.Context(), "web: export des données impossible", "member_id", member.ID, "error", err)
		http.Error(w, "erreur interne", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="mes-donnees-automata.json"`)

	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(export); err != nil {
		h.Logger.ErrorContext(r.Context(), "web: écriture de l'export", "member_id", member.ID, "error", err)
	}

	h.Logger.InfoContext(r.Context(), "web: données exportées", "member_id", member.ID)
}

// HandleProfileDelete efface les données personnelles après confirmation.
func (h *Handlers) HandleProfileDelete(w http.ResponseWriter, r *http.Request) {
	member, r, minutes, ok := h.resolveProfile(w, r)
	if !ok {
		return
	}
	if !core.CheckCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}
	if h.Privacy == nil {
		http.NotFound(w, r)
		return
	}

	linkPath := "/p/" + r.PathValue("link")
	if !isDeletionConfirmed(r.PostFormValue("confirmation")) {
		http.Redirect(w, r, linkPath+"/privacy?erreur=confirmation", http.StatusFound)
		return
	}

	report, err := h.Privacy.Delete(r.Context(), member.ID)
	if err != nil {
		h.Logger.ErrorContext(r.Context(), "web: suppression des données impossible", "member_id", member.ID, "error", err)
		http.Error(w, "erreur interne", http.StatusInternalServerError)
		return
	}

	h.Logger.InfoContext(r.Context(), "web: données personnelles supprimées",
		"member_id", member.ID, "messages", report.Messages, "memories", report.Memories)

	h.Render(w, r, http.StatusOK, view.ProfilePrivacy(view.PrivacyPage{
		LinkID:  r.PathValue("link"),
		Header:  h.profileHeader(r, member, minutes),
		Deleted: true,
		Report: i18n.TC(r.Context(), "privacy.report",
			i18n.TN(r.Context(), "privacy.count.messages", report.Messages),
			i18n.TN(r.Context(), "privacy.count.memories", report.Memories)),
	}))
}
