package admin

import (
	"context"
	"database/sql"
	"net/http"
	"sort"
	"strings"

	"github.com/bornholm/automata/internal/llmclients"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/web/core"
	"github.com/bornholm/automata/internal/web/view"
)

// modelRoleRows dresse la liste des rôles réglables avec le catalogue
// propre à chacun : les modèles de conversation, ou ceux de génération
// d'images — les deux familles ne sont pas interchangeables.
//
// orgID vide dresse les rôles de l'INSTANCE : Chosen est alors le défaut
// lui-même, et il n'y a pas de niveau au-dessus (Default reste vide, un
// rôle sans choix est en alerte).
func (h *Handlers) modelRoleRows(ctx context.Context, tx *sql.Tx, orgID string) ([]view.OrgModelRole, error) {
	catalog, err := h.LLMClients.List(ctx, tx, "")
	if err != nil {
		return nil, err
	}

	chosen, err := h.OrgClients.ListByOrg(ctx, tx, orgID)
	if err != nil {
		return nil, err
	}

	// Les défauts d'instance donnent, sur l'écran d'une organisation, le
	// libellé de l'option « défaut » ; sur l'écran de l'instance ils SONT
	// le choix.
	instanceDefaults := chosen
	if orgID != "" {
		if instanceDefaults, err = h.OrgClients.ListByOrg(ctx, tx, ""); err != nil {
			return nil, err
		}
	}

	models := make(map[string]string, len(catalog))
	byKind := map[string][]view.OrgModelOption{}
	for _, row := range catalog {
		models[row.Name] = row.Model
		byKind[row.Kind] = append(byKind[row.Kind], view.OrgModelOption{Name: row.Name, Model: row.Model})
	}

	roleNames := llmclients.Roles(h.Cfg)
	roles := make([]view.OrgModelRole, 0, len(roleNames))
	for _, role := range roleNames {
		entry := view.OrgModelRole{
			Role:         role,
			Chosen:       chosen[role],
			Options:      byKind[persistence.LLMClientKindLLM],
			InstanceView: orgID == "",
		}
		if orgID != "" {
			entry.Default = instanceDefaults[role]
			entry.DefaultModel = models[entry.Default]
		}

		switch {
		case strings.HasPrefix(role, llmclients.RoleImagePrefix):
			agentName := strings.TrimPrefix(role, llmclients.RoleImagePrefix)
			entry.Label = "Images de l'agent " + agentName
			entry.Hint = "Le modèle qui dessine derrière generate_image, distinct de celui qui converse."
			entry.Options = byKind[persistence.LLMClientKindImage]
		case strings.HasPrefix(role, llmclients.RoleEmbeddingsPrefix):
			indexID := strings.TrimPrefix(role, llmclients.RoleEmbeddingsPrefix)
			entry.Label = "Embeddings de l'index " + indexID
			entry.Hint = "VERROUILLÉ après le premier démarrage : changer de modèle rendrait les vecteurs déjà écrits incomparables. Effet au redémarrage."
		case roleLabels[role] != [2]string{}:
			labels := roleLabels[role]
			entry.Label, entry.Hint = labels[0], labels[1]
		default:
			entry.Label = "Agent " + role
			entry.Hint = h.Cfg.Agents[role].Description
			if entry.Hint == "" {
				entry.Hint = "Agent déclaré dans la configuration de l'instance."
			}
		}

		roles = append(roles, entry)
	}

	// Les agents d'abord, par ordre alphabétique, puis les rôles système :
	// l'opérateur cherche d'abord son assistant, pas la compaction.
	sort.Slice(roles, func(i, j int) bool {
		iSystem, jSystem := roleLabels[roles[i].Role], roleLabels[roles[j].Role]
		if (iSystem != [2]string{}) != (jSystem != [2]string{}) {
			return jSystem != [2]string{}
		}
		return roles[i].Role < roles[j].Role
	})

	return roles, nil
}

// HandleOrgModels enregistre les modèles choisis par une organisation. Un
// champ laissé sur « défaut de l'instance » efface la surcharge : une
// organisation ne garde jamais un choix qu'elle n'a plus exprimé.

// HandleOrgModels enregistre les modèles choisis par une organisation. Un
// champ laissé sur « défaut de l'instance » efface la surcharge : une
// organisation ne garde jamais un choix qu'elle n'a plus exprimé.
func (h *Handlers) HandleOrgModels(w http.ResponseWriter, r *http.Request) {
	if !core.CheckCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}

	h.saveModelRoles(w, r, r.PathValue("id"), "/admin/orgs/"+r.PathValue("id")+"?tab=models")
}

// HandleInstanceModels enregistre les défauts d'instance : mêmes règles que
// pour une organisation, org_id vide.

// HandleInstanceModels enregistre les défauts d'instance : mêmes règles que
// pour une organisation, org_id vide.
func (h *Handlers) HandleInstanceModels(w http.ResponseWriter, r *http.Request) {
	if !core.CheckCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}

	h.saveModelRoles(w, r, "", "/admin/llm-clients")
}

// saveModelRoles enregistre les choix de modèles postés, pour une
// organisation ou pour l'instance (orgID vide). Un champ laissé vide efface
// la ligne : personne ne garde un choix qu'il n'a plus exprimé.

// saveModelRoles enregistre les choix de modèles postés, pour une
// organisation ou pour l'instance (orgID vide). Un champ laissé vide efface
// la ligne : personne ne garde un choix qu'il n'a plus exprimé.
func (h *Handlers) saveModelRoles(w http.ResponseWriter, r *http.Request, orgID, redirect string) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "formulaire illisible", http.StatusBadRequest)
		return
	}

	now := h.Now()
	var unknown string

	if !h.WithTx(w, r, func(tx *sql.Tx) error {
		catalog, err := h.LLMClients.List(r.Context(), tx, "")
		if err != nil {
			return err
		}
		kinds := make(map[string]string, len(catalog))
		for _, row := range catalog {
			kinds[row.Name] = row.Kind
		}

		// Seuls les rôles que l'instance déclare sont acceptés : un champ
		// forgé ne doit pas créer une ligne que rien ne lira. Et chaque
		// rôle n'accepte que sa famille — un générateur d'images ne
		// conversera jamais.
		for _, role := range llmclients.Roles(h.Cfg) {
			name := strings.TrimSpace(r.PostFormValue("role:" + role))

			if name == "" {
				if err := h.OrgClients.Unset(r.Context(), tx, orgID, role); err != nil {
					return err
				}
				continue
			}

			wantKind := persistence.LLMClientKindLLM
			if strings.HasPrefix(role, llmclients.RoleImagePrefix) {
				wantKind = persistence.LLMClientKindImage
			}
			if kinds[name] != wantKind {
				unknown = name
				return nil
			}

			if err := h.OrgClients.Set(r.Context(), tx, persistence.OrgAgentClient{
				OrgID:      orgID,
				Role:       role,
				ClientName: name,
				UpdatedAt:  now,
			}); err != nil {
				return err
			}
		}

		return nil
	}) {
		return
	}

	if unknown != "" {
		http.Error(w, "modèle inconnu ou de la mauvaise famille : "+unknown, http.StatusBadRequest)
		return
	}

	h.Logger.InfoContext(r.Context(), "web: rôles de modèles modifiés", "org", orgID)

	http.Redirect(w, r, redirect, http.StatusFound)
}

// HandleLLMClientDelete supprime un client, sauf si un rôle ou une
// organisation s'y réfère encore : la suppression laisserait alors des
// agents sans modèle.
