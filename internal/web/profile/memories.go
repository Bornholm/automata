package profile

import (
	"net/http"
	"strings"

	"github.com/bornholm/automata/internal/memory"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/web/core"
	"github.com/bornholm/automata/internal/web/view"
)

// L'écran « Ce que je retiens » : voir, corriger et effacer ses souvenirs.
//
// Un souvenir faux est pire qu'un souvenir absent — l'assistant s'en servira
// avec aplomb, pendant des mois, sans que rien ne le signale. La correction
// n'est donc pas un raffinement de confort : c'est ce qui rend la mémoire
// utilisable en confiance.
//
// INVARIANT DE CLOISONNEMENT. Toute opération sur un souvenir désigné par
// son identifiant vérifie D'ABORD qu'il appartient à la portée personnelle
// du membre, par GetByID(orgID, personal, memberID). Cette vérification
// n'est pas une formalité : memory.Store.Forget supprime par identifiant
// SANS aucun contrôle de portée, si bien qu'un identifiant deviné ou recopié
// effacerait le souvenir de quelqu'un d'autre. La portée du membre ne vient
// jamais de la requête, toujours du lien de profil déjà validé.

// HandleProfileMemories liste les souvenirs personnels du membre.
func (h *Handlers) HandleProfileMemories(w http.ResponseWriter, r *http.Request) {
	member, minutes, ok := h.resolveProfile(w, r)
	if !ok {
		return
	}
	if h.Memory == nil {
		http.NotFound(w, r)
		return
	}

	plugins, ok := h.profilePluginUIs(w, r, member)
	if !ok {
		return
	}

	page := view.MemoriesPage{
		LinkID:    r.PathValue("link"),
		Header:    h.profileHeader(r, member, minutes),
		CSRFToken: h.CSRFToken(w, r),
		PluginUIs: plugins,
		EditID:    r.URL.Query().Get("edit"),
	}

	switch r.URL.Query().Get("done") {
	case "edited":
		page.Notice = "Souvenir corrigé. Je m'en tiendrai à cette version."
	case "deleted":
		page.Notice = "Souvenir effacé. Je ne m'en servirai plus."
	}
	if r.URL.Query().Get("error") == "1" {
		page.Error = "Ce souvenir n'a pas pu être modifié. Rechargez la page et réessayez."
	}

	memories, err := h.Memory.ListByScope(r.Context(), model.OrgID(member.OrgID),
		model.ScopePersonal, model.ScopeID(member.ID))
	if err != nil {
		h.Logger.ErrorContext(r.Context(), "web: lecture des souvenirs",
			"org_id", member.OrgID, "member_id", member.ID, "error", err)
		page.Error = "Vos souvenirs n'ont pas pu être lus. Réessayez dans un instant."
	}

	for _, m := range memories {
		page.Memories = append(page.Memories, view.MemoryRow{
			ID:      m.ID,
			Content: m.Content,
			At:      m.CreatedAt.Local().Format("02/01/2006"),
			Origin:  originLabel(m.Metadata["origin"]),
		})
	}

	h.Render(w, r, http.StatusOK, view.ProfileMemories(page))
}

// HandleProfileMemoryUpdate corrige un souvenir.
//
// La correction est une suppression suivie d'un nouvel enregistrement : la
// mémoire n'expose pas de mise à jour en place. Deux propriétés sont
// préservées à la main — la date d'acquisition, pour ne pas laisser croire
// qu'Automata vient de l'apprendre, et l'origine, qui dit d'où le souvenir
// venait. L'identifiant, lui, change : il n'est jamais montré ni cité
// ailleurs.
func (h *Handlers) HandleProfileMemoryUpdate(w http.ResponseWriter, r *http.Request) {
	member, ok := h.resolveMemoryAction(w, r)
	if !ok {
		return
	}

	link := r.PathValue("link")
	id := r.PathValue("id")

	content := strings.TrimSpace(r.FormValue("content"))
	if content == "" {
		// Un souvenir vidé est un souvenir qu'on veut effacer : le dire
		// plutôt que d'enregistrer une ligne blanche.
		h.forgetMemory(w, r, member, id, link)
		return
	}

	existing, found, err := h.Memory.GetByID(r.Context(), model.OrgID(member.OrgID),
		model.ScopePersonal, model.ScopeID(member.ID), id)
	if err != nil || !found {
		h.memoryFailure(w, r, link, member, id, "correction", err, found)
		return
	}

	if err := h.Memory.Forget(r.Context(), id); err != nil {
		h.memoryFailure(w, r, link, member, id, "correction", err, true)
		return
	}

	if _, err := h.Memory.Remember(r.Context(), memory.NewMemory{
		Content:          content,
		OrgID:            model.OrgID(member.OrgID),
		Scope:            model.ScopePersonal,
		ScopeID:          model.ScopeID(member.ID),
		OwnerPrincipalID: model.PrincipalID(member.ID),
		CreatedBy:        model.PrincipalID(member.ID),
		CreatedAt:        existing.CreatedAt,
		Origin:           existing.Metadata["origin"],
	}); err != nil {
		// Le souvenir d'origine est déjà effacé : le signaler franchement
		// vaut mieux que d'afficher un succès sur une perte.
		h.Logger.ErrorContext(r.Context(), "web: souvenir perdu à la correction",
			"org_id", member.OrgID, "member_id", member.ID, "error", err)
		http.Redirect(w, r, "/p/"+link+"/memories?error=1", http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/p/"+link+"/memories?done=edited", http.StatusSeeOther)
}

// HandleProfileMemoryDelete efface un souvenir.
func (h *Handlers) HandleProfileMemoryDelete(w http.ResponseWriter, r *http.Request) {
	member, ok := h.resolveMemoryAction(w, r)
	if !ok {
		return
	}

	h.forgetMemory(w, r, member, r.PathValue("id"), r.PathValue("link"))
}

// forgetMemory efface après vérification de la portée.
func (h *Handlers) forgetMemory(w http.ResponseWriter, r *http.Request, member persistence.Member, id, link string) {
	_, found, err := h.Memory.GetByID(r.Context(), model.OrgID(member.OrgID),
		model.ScopePersonal, model.ScopeID(member.ID), id)
	if err != nil || !found {
		h.memoryFailure(w, r, link, member, id, "suppression", err, found)
		return
	}

	if err := h.Memory.Forget(r.Context(), id); err != nil {
		h.memoryFailure(w, r, link, member, id, "suppression", err, true)
		return
	}

	http.Redirect(w, r, "/p/"+link+"/memories?done=deleted", http.StatusSeeOther)
}

// resolveMemoryAction valide une action d'écriture sur un souvenir : session
// de profil, CSRF, et mémoire disponible.
func (h *Handlers) resolveMemoryAction(w http.ResponseWriter, r *http.Request) (persistence.Member, bool) {
	if !core.CheckCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return persistence.Member{}, false
	}

	member, _, ok := h.resolveProfile(w, r)
	if !ok {
		return persistence.Member{}, false
	}
	if h.Memory == nil {
		http.NotFound(w, r)
		return persistence.Member{}, false
	}

	return member, true
}

// memoryFailure journalise et renvoie sur la liste. Un souvenir absent de la
// portée du membre est traité comme n'importe quel échec : ne pas distinguer
// « il n'existe pas » de « il ne vous appartient pas » évite d'en faire un
// moyen de découvrir les souvenirs d'autrui.
func (h *Handlers) memoryFailure(w http.ResponseWriter, r *http.Request, link string, member persistence.Member, id, action string, err error, found bool) {
	h.Logger.WarnContext(r.Context(), "web: action sur un souvenir refusée",
		"action", action, "org_id", member.OrgID, "member_id", member.ID,
		"memory_id", id, "found", found, "error", err)

	http.Redirect(w, r, "/p/"+link+"/memories?error=1", http.StatusSeeOther)
}

// originLabel met en mots la provenance d'un souvenir. Les valeurs viennent
// des mécanismes applicatifs (internal/conversation, internal/consolidation,
// internal/onboarding) et ne veulent rien dire pour la personne concernée :
// « episode_reflection » n'explique rien, « observé au fil des échanges »
// si.
func originLabel(origin string) string {
	switch origin {
	case "":
		return "vous me l'avez dit"
	case "onboarding":
		return "recueilli à notre première conversation"
	case "compaction":
		return "retenu d'une conversation"
	case "consolidation":
		return "regroupé avec d'autres souvenirs"
	case "reflection", "episode_reflection":
		return "observé au fil des échanges"
	default:
		return ""
	}
}
