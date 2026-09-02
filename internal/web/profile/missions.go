package profile

import (
	"database/sql"
	"net/http"

	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/web/core"
	"github.com/bornholm/automata/internal/web/view"
)

// L'écran « Dossiers » : les missions au long cours, journal de bord
// compris — la personne lit exactement ce que l'agent relira au prochain
// réveil, comme pour les souvenirs.
//
// Même invariant de cloisonnement que les suggestions : l'abandon passe par
// UpdateStatus filtré (org, membre). Un identifiant recopié d'un autre
// profil ne modifie rien.

// maxListedMissions borne l'affichage — au-delà de la borne de missions
// actives, il couvre l'historique des dossiers clos.
const maxListedMissions = 30

// missionStatusLabels traduit le statut pour la personne.
var missionStatusLabels = map[string]string{
	persistence.MissionStatusActive:    "suivie",
	persistence.MissionStatusDone:      "terminée",
	persistence.MissionStatusAbandoned: "abandonnée",
}

// HandleProfileMissions liste les dossiers du membre.
func (h *Handlers) HandleProfileMissions(w http.ResponseWriter, r *http.Request) {
	member, minutes, ok := h.resolveProfile(w, r)
	if !ok {
		return
	}

	plugins, ok := h.profilePluginUIs(w, r, member)
	if !ok {
		return
	}

	page := view.MissionsPage{
		LinkID:    r.PathValue("link"),
		Header:    h.profileHeader(r, member, minutes),
		CSRFToken: h.CSRFToken(w, r),
		PluginUIs: plugins,
	}

	if r.URL.Query().Get("done") == "abandoned" {
		page.Notice = "Dossier abandonné. Je ne ferai plus de point d'étape dessus."
	}

	var missions []persistence.Mission
	if !h.WithTx(w, r, func(tx *sql.Tx) error {
		var err error
		missions, err = persistence.NewMissionRepository(h.DB.Cipher()).
			ListByMember(r.Context(), tx, member.OrgID, member.ID, maxListedMissions)
		return err
	}) {
		return
	}

	for _, m := range missions {
		row := view.MissionRow{
			ID:          m.ID,
			Title:       m.Title,
			Objective:   m.Objective,
			Journal:     m.Journal,
			StatusLabel: missionStatusLabels[m.Status],
			Active:      m.Status == persistence.MissionStatusActive,
		}
		if row.Active && !m.NextCheckAt.IsZero() {
			row.NextCheckAt = m.NextCheckAt.Local().Format("02/01/2006 à 15:04")
		}
		page.Missions = append(page.Missions, row)
	}

	h.Render(w, r, http.StatusOK, view.ProfileMissions(page))
}

// HandleProfileMissionAbandon clôt un dossier, définitivement : plus aucun
// réveil. Le geste appartient à la personne — l'agent, lui, ne peut que
// clore en « terminée ».
func (h *Handlers) HandleProfileMissionAbandon(w http.ResponseWriter, r *http.Request) {
	if !core.CheckCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}

	member, _, ok := h.resolveProfile(w, r)
	if !ok {
		return
	}

	link := r.PathValue("link")
	id := r.PathValue("id")

	if !h.WithTx(w, r, func(tx *sql.Tx) error {
		// Le filtre (org, membre) d'UpdateStatus est le cloisonnement : le
		// dossier d'un autre membre reste intact, silencieusement.
		_, err := persistence.NewMissionRepository(h.DB.Cipher()).
			UpdateStatus(r.Context(), tx, member.OrgID, member.ID, id,
				persistence.MissionStatusAbandoned, h.Now())
		return err
	}) {
		return
	}

	http.Redirect(w, r, "/p/"+link+"/missions?done=abandoned", http.StatusSeeOther)
}
