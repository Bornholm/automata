package admin

import (
	"database/sql"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
)

// HandleOrgCustomization enregistre la personnalisation d'une
// organisation : consigne ajoutée, spécialistes conservés, plafond
// d'outils.
func (h *Handlers) HandleOrgCustomization(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("id")

	// Les cases cochées disent ce qui reste : ce qui est retiré est le
	// complément, calculé sur les spécialistes réellement déclarés.
	kept := map[string]struct{}{}
	for _, name := range r.PostForm["agent"] {
		kept[name] = struct{}{}
	}

	var disabled []string
	for name, agentCfg := range h.Cfg.Agents {
		if agentCfg.Type == config.AgentTypeOrchestrator {
			continue
		}
		if _, ok := kept[name]; !ok {
			disabled = append(disabled, name)
		}
	}
	sort.Strings(disabled)

	maxToolCalls, _ := strconv.Atoi(r.PostFormValue("max_tool_calls"))
	if maxToolCalls < 0 {
		maxToolCalls = 0
	}

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		return h.OrgSettings.Upsert(r.Context(), tx, persistence.OrgSettings{
			OrgID:          orgID,
			PromptExtra:    strings.TrimSpace(r.PostFormValue("prompt_extra")),
			DisabledAgents: disabled,
			MaxToolCalls:   maxToolCalls,
			UpdatedAt:      h.Now(),
		})
	})
	if !ok {
		return
	}

	h.Logger.InfoContext(r.Context(), "web: personnalisation d'organisation enregistrée",
		"org_id", orgID, "disabled_agents", len(disabled), "max_tool_calls", maxToolCalls)

	http.Redirect(w, r, "/admin/orgs/"+orgID+"?tab=customization&saved=1", http.StatusFound)
}

// HandleOrgDelete supprime une organisation et tout ce qui n'existe que
// par elle.
//
// La confirmation demande de retaper le nom : la liste d'administration
// peut présenter deux organisations homonymes, et se tromper de ligne
// n'aurait aucun recours — l'effacement est complet et définitif.
