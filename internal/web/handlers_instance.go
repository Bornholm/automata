package web

import (
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/web/view"
)

// handleInstance — ADM-07, en lecture seule.
func (s *Server) handleInstance(w http.ResponseWriter, r *http.Request) {
	page := view.InstancePage{
		Platforms: s.sidebarPlatforms(),
		CSRFToken: s.csrfToken(w, r),
		Version:   fmt.Sprintf("configuration version %d", s.cfg.Version),
	}

	// Catalogue des modèles et défauts d'instance : lus une fois, ils
	// servent la section des agents (quel modèle sert chacun) et la leur.
	var (
		catalog          []persistence.LLMClient
		instanceDefaults map[string]string
	)
	if !s.withTx(w, r, func(tx *sql.Tx) error {
		var err error
		if catalog, err = s.llmClients.List(r.Context(), tx, ""); err != nil {
			return err
		}
		instanceDefaults, err = s.orgClients.ListByOrg(r.Context(), tx, "")
		return err
	}) {
		return
	}
	models := make(map[string]string, len(catalog))
	for _, client := range catalog {
		models[client.Name] = client.Model
	}

	// Agents : le cœur de ce qu'on veut vérifier sans SSH.
	agents := view.InstanceSection{
		Title:     "Agents",
		Detail:    "Ce que l'assistant sait faire, et à qui il peut déléguer",
		Href:      "/admin/orgs",
		HrefLabel: "Personnaliser par organisation",
	}
	var agentNames []string
	for name := range s.cfg.Agents {
		agentNames = append(agentNames, name)
	}
	sort.Strings(agentNames)

	for _, name := range agentNames {
		agentCfg := s.cfg.Agents[name]

		// Le modèle d'un agent est le défaut d'instance de son rôle,
		// réglé en ligne — le fichier ne le connaît plus.
		value := models[instanceDefaults[name]]
		if value == "" {
			value = "non configuré"
		}

		hint := agentCfg.Description
		if agentCfg.Type == config.AgentTypeOrchestrator && len(agentCfg.Delegates) > 0 {
			hint = fmt.Sprintf("délègue à %d spécialiste(s)", len(agentCfg.Delegates))
		}

		chip := view.Chip{Label: "spécialiste", Tone: "neutral"}
		if agentCfg.Type == config.AgentTypeOrchestrator {
			chip = view.Chip{Label: "généraliste", Tone: "brand"}
		}

		agents.Rows = append(agents.Rows, view.InstanceRow{
			Name: name, Value: value, Hint: hint, Chip: &chip,
		})
	}
	page.Sections = append(page.Sections, agents)

	// Clients de modèles : le catalogue tel qu'il est EN BASE, qui fait
	// autorité depuis la migration 0022 — la configuration n'en est plus
	// que le semis initial. Le modèle et le fournisseur, jamais la clé.
	clients := view.InstanceSection{
		Title:     "Clients de modèles",
		Detail:    "Fournisseurs et modèles en service — les clés d'API ne sont jamais affichées",
		Href:      "/admin/llm-clients",
		HrefLabel: "Gérer les modèles",
	}
	for _, client := range catalog {
		hint := client.Provider
		if client.Kind == persistence.LLMClientKindImage {
			hint += " — images"
		}

		clients.Rows = append(clients.Rows, view.InstanceRow{
			Name:  client.Name,
			Value: client.Model,
			Hint:  hint,
		})
	}
	page.Sections = append(page.Sections, clients)

	// Serveurs d'outils.
	mcp := view.InstanceSection{
		Title:  "Serveurs d'outils",
		Detail: "Les capacités externes que les spécialistes peuvent appeler",
	}
	var mcpNames []string
	for name := range s.cfg.MCPServers {
		mcpNames = append(mcpNames, name)
	}
	sort.Strings(mcpNames)
	for _, name := range mcpNames {
		server := s.cfg.MCPServers[name]
		// L'URL d'un serveur distant, ou la commande d'un serveur local :
		// c'est ce qui dit d'où viennent réellement les outils.
		hint := server.URL
		if server.Transport == "stdio" && len(server.Command) > 0 {
			hint = server.Command[0]
		}

		mcp.Rows = append(mcp.Rows, view.InstanceRow{
			Name:  name,
			Value: server.Transport,
			Hint:  hint,
		})
	}
	page.Sections = append(page.Sections, mcp)

	// Services de fond : ce qui tourne en plus des conversations.
	services := view.InstanceSection{Title: "Services de fond"}
	services.Rows = append(services.Rows,
		booleanRow("Compaction des conversations", s.cfg.Conversation.Compaction.Enabled,
			"résume les échanges anciens pour tenir le contexte"),
		booleanRow("Consolidation de la mémoire", s.cfg.Memory.Consolidation.Enabled,
			"réorganise les souvenirs, la nuit"),
		booleanRow("Sauvegardes", s.cfg.Backup.Enabled, sauvegardeHint(s.cfg)),
		booleanRow("Paiement en ligne", s.cfg.Web.Stripe.Enabled(),
			"achat de crédits par carte depuis le profil"),
		booleanRow("Envoi de courriels", s.cfg.Web.MailProvider != "",
			"codes de vérification de l'adresse de récupération"),
	)
	page.Sections = append(page.Sections, services)

	s.render(w, r, http.StatusOK, view.AdminInstance(page))
}

// booleanRow décrit un service actif ou non.
func booleanRow(name string, enabled bool, hint string) view.InstanceRow {
	chip := view.Chip{Label: "inactif", Tone: "neutral", Dot: true}
	value := "non configuré"
	if enabled {
		chip = view.Chip{Label: "actif", Tone: "ok", Dot: true}
		value = "en fonctionnement"
	}

	return view.InstanceRow{Name: name, Value: value, Hint: hint, Chip: &chip}
}

// sauvegardeHint décrit la politique de sauvegarde en clair.
func sauvegardeHint(cfg *config.Config) string {
	if !cfg.Backup.Enabled {
		return "aucune copie des bases n'est faite"
	}

	return fmt.Sprintf("toutes les %s, %d copies conservées",
		humanDuration(cfg.Backup.EffectiveInterval()), cfg.Backup.EffectiveKeep())
}

// humanDuration rend une durée lisible : « 6 heures » plutôt que
// « 6h0m0s », qui est une notation de programmeur.
func humanDuration(d time.Duration) string {
	switch {
	case d >= 24*time.Hour && d%(24*time.Hour) == 0:
		days := int(d / (24 * time.Hour))
		if days == 1 {
			return "24 heures"
		}
		return fmt.Sprintf("%d jours", days)
	case d >= time.Hour && d%time.Hour == 0:
		hours := int(d / time.Hour)
		if hours == 1 {
			return "heure"
		}
		return fmt.Sprintf("%d heures", hours)
	case d >= time.Minute:
		return fmt.Sprintf("%d minutes", int(d/time.Minute))
	default:
		return d.String()
	}
}
