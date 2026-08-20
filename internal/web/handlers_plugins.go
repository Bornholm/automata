package web

import (
	"context"
	"database/sql"
	"net/http"

	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/plugin"
	"github.com/bornholm/automata/internal/web/view"
)

// PluginManager est la vue du gestionnaire de plugins dont l'écran
// d'administration a besoin. Interface déclarée côté consommateur, comme
// PlatformManager.
type PluginManager interface {
	Statuses() []plugin.Status
	Restart(ctx context.Context, name string) bool
}

// handlePlugins — ADM-09 : état des plugins chargés et organisations où
// chacun est actif.
func (s *Server) handlePlugins(w http.ResponseWriter, r *http.Request) {
	page := view.PluginsPage{
		Platforms: s.sidebarPlatforms(),
		CSRFToken: s.csrfToken(w, r),
	}

	if s.pluginManager == nil {
		s.render(w, r, http.StatusOK, view.AdminPlugins(page))
		return
	}

	statuses := s.pluginManager.Statuses()

	orgsByPlugin := map[string][]string{}
	orgNames := map[string]string{}
	ok := s.withTx(w, r, func(tx *sql.Tx) error {
		for _, st := range statuses {
			orgs, err := s.pluginActivations.EnabledOrgs(r.Context(), tx, st.Name)
			if err != nil {
				return err
			}
			orgsByPlugin[st.Name] = orgs
		}

		orgs, err := s.orgs.List(r.Context(), tx, "")
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

	s.render(w, r, http.StatusOK, view.AdminPlugins(page))
}

// handlePluginRestart relance un plugin sur décision de l'opérateur, sans
// délai de refroidissement : l'humain qui clique a vu l'état.
func (s *Server) handlePluginRestart(w http.ResponseWriter, r *http.Request) {
	if s.pluginManager == nil {
		http.NotFound(w, r)
		return
	}
	if !checkCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}

	name := r.PathValue("name")
	restarted := s.pluginManager.Restart(r.Context(), name)

	s.logger.InfoContext(r.Context(), "web: redémarrage de plugin demandé",
		"plugin", name, "restarted", restarted)

	http.Redirect(w, r, "/admin/plugins", http.StatusFound)
}

// handleOrgPlugins enregistre l'activation des plugins pour une
// organisation : le formulaire coche les plugins actifs, tout plugin
// chargé absent des cases est désactivé.
func (s *Server) handleOrgPlugins(w http.ResponseWriter, r *http.Request) {
	if !checkCSRF(r) {
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
	if s.pluginManager != nil {
		for _, st := range s.pluginManager.Statuses() {
			loaded = append(loaded, st.Name)
		}
	}

	now := s.now()
	ok := s.withTx(w, r, func(tx *sql.Tx) error {
		// Organisation inconnue : non-opération silencieuse, comme les
		// autres actions de la fiche — la contrainte de clé étrangère
		// protège de toute écriture orpheline.
		if _, found, err := s.orgs.FindByID(r.Context(), tx, orgID); err != nil || !found {
			return err
		}

		for _, name := range loaded {
			if err := s.pluginActivations.Upsert(r.Context(), tx, persistence.PluginActivation{
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

	s.logger.InfoContext(r.Context(), "web: activation de plugins mise à jour",
		"org_id", orgID, "enabled_count", len(checked))

	http.Redirect(w, r, "/admin/orgs/"+orgID+"?tab=plugins&saved=1", http.StatusFound)
}

// pluginActivationRows prépare l'onglet Plugins de la fiche organisation.
func (s *Server) pluginActivationRows(r *http.Request, tx *sql.Tx, orgID string) ([]view.PluginActivationRow, error) {
	if s.pluginManager == nil {
		return nil, nil
	}

	enabled, err := s.pluginActivations.EnabledPlugins(r.Context(), tx, orgID)
	if err != nil {
		return nil, err
	}
	enabledSet := map[string]bool{}
	for _, name := range enabled {
		enabledSet[name] = true
	}

	endpoint, _ := s.pluginManager.(PluginUIEndpoint)

	var rows []view.PluginActivationRow
	for _, st := range s.pluginManager.Statuses() {
		row := view.PluginActivationRow{
			Name:        st.Name,
			Description: st.Description,
			Running:     st.Running,
			Enabled:     enabledSet[st.Name],
		}
		if endpoint != nil {
			if _, _, hasUI := endpoint.UIEndpoint(st.Name); hasUI {
				row.UISrc = "/admin/orgs/" + orgID + "/plugins/" + st.Name + "/ui/"
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}
