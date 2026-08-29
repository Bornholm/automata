package web

import (
	"database/sql"
	"net/http"

	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/web/core"
	"github.com/bornholm/automata/internal/web/view"
)

// handlePlugins — ADM-09 : état des plugins chargés et organisations où
// chacun est actif.
func (s *Server) handlePlugins(w http.ResponseWriter, r *http.Request) {
	page := view.PluginsPage{
		Platforms: s.SidebarPlatforms(),
		CSRFToken: s.CSRFToken(w, r),
	}

	if s.PluginMgr == nil {
		s.Render(w, r, http.StatusOK, view.AdminPlugins(page))
		return
	}

	statuses := s.PluginMgr.Statuses()

	orgsByPlugin := map[string][]string{}
	orgNames := map[string]string{}
	ok := s.WithTx(w, r, func(tx *sql.Tx) error {
		for _, st := range statuses {
			orgs, err := s.PluginActivations.EnabledOrgs(r.Context(), tx, st.Name)
			if err != nil {
				return err
			}
			orgsByPlugin[st.Name] = orgs
		}

		orgs, err := s.Orgs.List(r.Context(), tx, "")
		if err != nil {
			return err
		}
		for _, org := range orgs {
			orgNames[org.ID] = org.DisplayName
		}
		return nil
	})
	if !ok {
		return
	}

	for _, st := range statuses {
		row := view.PluginRow{
			Name:        st.Name,
			Version:     st.Version,
			Description: st.Description,
			Running:     st.Running,
			HasUI:       st.HasUI,
			HasTriggers: st.HasTriggers,
			HasSubAgent: st.HasSubAgent,
		}
		for _, orgID := range orgsByPlugin[st.Name] {
			name := orgNames[orgID]
			if name == "" {
				name = orgID
			}
			row.ActiveOrgs = append(row.ActiveOrgs, name)
		}
		page.Plugins = append(page.Plugins, row)
	}

	s.Render(w, r, http.StatusOK, view.AdminPlugins(page))
}

// handlePluginRestart relance un plugin sur décision de l'opérateur, sans
// délai de refroidissement : l'humain qui clique a vu l'état.
func (s *Server) handlePluginRestart(w http.ResponseWriter, r *http.Request) {
	if s.PluginMgr == nil {
		http.NotFound(w, r)
		return
	}
	if !core.CheckCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}

	name := r.PathValue("name")
	restarted := s.PluginMgr.Restart(r.Context(), name)

	s.Logger.InfoContext(r.Context(), "web: redémarrage de plugin demandé",
		"plugin", name, "restarted", restarted)

	http.Redirect(w, r, "/admin/plugins", http.StatusFound)
}

// handleOrgPlugins enregistre l'activation des plugins pour une
// organisation : le formulaire coche les plugins actifs, tout plugin
// chargé absent des cases est désactivé.
func (s *Server) handleOrgPlugins(w http.ResponseWriter, r *http.Request) {
	if !core.CheckCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}

	orgID := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "formulaire illisible", http.StatusBadRequest)
		return
	}

	checked := map[string]bool{}
	for _, name := range r.Form["plugins"] {
		checked[name] = true
	}

	var loaded []string
	if s.PluginMgr != nil {
		for _, st := range s.PluginMgr.Statuses() {
			loaded = append(loaded, st.Name)
		}
	}

	now := s.Now()
	ok := s.WithTx(w, r, func(tx *sql.Tx) error {
		// Organisation inconnue : non-opération silencieuse, comme les
		// autres actions de la fiche — la contrainte de clé étrangère
		// protège de toute écriture orpheline.
		if _, found, err := s.Orgs.FindByID(r.Context(), tx, orgID); err != nil || !found {
			return err
		}

		for _, name := range loaded {
			if err := s.PluginActivations.Upsert(r.Context(), tx, persistence.PluginActivation{
				PluginName: name,
				OrgID:      orgID,
				Enabled:    checked[name],
				CreatedAt:  now,
				UpdatedAt:  now,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if !ok {
		return
	}

	s.Logger.InfoContext(r.Context(), "web: activation de plugins mise à jour",
		"org_id", orgID, "enabled_count", len(checked))

	http.Redirect(w, r, "/admin/orgs/"+orgID+"?tab=plugins&saved=1", http.StatusFound)
}

// pluginActivationRows prépare l'onglet Plugins de la fiche organisation.
func (s *Server) pluginActivationRows(r *http.Request, tx *sql.Tx, orgID string) ([]view.PluginActivationRow, error) {
	if s.PluginMgr == nil {
		return nil, nil
	}

	enabled, err := s.PluginActivations.EnabledPlugins(r.Context(), tx, orgID)
	if err != nil {
		return nil, err
	}
	enabledSet := map[string]bool{}
	for _, name := range enabled {
		enabledSet[name] = true
	}

	endpoint, _ := s.PluginMgr.(PluginUIEndpoint)

	var rows []view.PluginActivationRow
	for _, st := range s.PluginMgr.Statuses() {
		row := view.PluginActivationRow{
			Name:        st.Name,
			Description: st.Description,
			Running:     st.Running,
			Enabled:     enabledSet[st.Name],
		}
		if endpoint != nil {
			if _, _, hasUI := endpoint.UIEndpoint(st.Name); hasUI {
				// Même raison que côté profil : l'iframe est sandbouclée,
				// aucun cookie ne l'accompagne, le jeton porte l'identité.
				row.UISrc = pluginUIPrefix + s.PluginUIToken(pluginViewAdmin, orgID, "", st.Name, s.Now()) + "/"
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}
