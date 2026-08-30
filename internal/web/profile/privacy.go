package profile

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/bornholm/automata/internal/web/core"
	"github.com/bornholm/automata/internal/web/view"
)

// deletionConfirmation est le mot que la personne doit écrire pour
// effacer ses données : une suppression irréversible ne se déclenche pas
// d'un clic distrait.
const deletionConfirmation = "SUPPRIMER"

// HandleProfilePrivacy — PRO-04.
func (h *Handlers) HandleProfilePrivacy(w http.ResponseWriter, r *http.Request) {
	member, minutes, ok := h.resolveProfile(w, r)
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
					Title:  "Vos conversations privées",
					Detail: "Ce que vous avez écrit à Automata en tête-à-tête, et ses réponses.",
					Count:  countLabel(len(export.Messages), "message", "messages"),
				},
				{
					Title:  "Ce qu'il a retenu de vous",
					Detail: "Les informations qu'Automata garde pour vous être utile : vos préférences, vos habitudes, ce que vous lui avez demandé de ne pas oublier.",
					Count:  countLabel(len(export.Memories), "souvenir", "souvenirs"),
				},
				{
					Title:  "Votre usage",
					Detail: "Le volume de vos échanges, mois par mois. Ces relevés servent de pièces comptables.",
					Count:  countLabel(len(export.Usage), "mois", "mois"),
				},
			}
			if export.Member.Email != "" {
				page.Items = append(page.Items, view.PrivacyItem{
					Title:  "Votre adresse de secours",
					Detail: export.Member.Email + " — utilisée uniquement pour vous retrouver si vous perdez l'accès à votre messagerie.",
				})
			}
		}
	}

	if r.URL.Query().Get("erreur") == "confirmation" {
		page.Error = "Pour supprimer vos données, écrivez exactement " + deletionConfirmation + "."
	}

	h.Render(w, r, http.StatusOK, view.ProfilePrivacy(page))
}

// countLabel accorde un décompte (« 1 souvenir », « 12 souvenirs »).
func countLabel(n int, singular, plural string) string {
	if n <= 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}

// HandleProfileExport sert l'export des données en JSON.
func (h *Handlers) HandleProfileExport(w http.ResponseWriter, r *http.Request) {
	member, _, ok := h.resolveProfile(w, r)
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
	member, minutes, ok := h.resolveProfile(w, r)
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
	if strings.TrimSpace(r.PostFormValue("confirmation")) != deletionConfirmation {
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
		Report: fmt.Sprintf("%s et %s ont été effacés, ainsi que vos rappels et votre adresse de secours.",
			countLabel(report.Messages, "message", "messages"),
			countLabel(report.Memories, "souvenir", "souvenirs")),
	}))
}
