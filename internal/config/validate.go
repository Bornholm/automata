package config

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	cron "github.com/robfig/cron/v3"
)

var validPermissionScopes = map[string]bool{
	string(ScopePersonal): true,
	string(ScopeGroup):    true,
	string(ScopeOrg):      true,
}

var validPermissionActions = map[string]bool{
	"read":   true,
	"write":  true,
	"delete": true,
}

// Validate vérifie l'intégralité de la configuration : contraintes
// structurelles, unicité des identifiants et références croisées entre
// sections. Toutes les violations trouvées sont retournées agrégées, pas
// seulement la première.
func Validate(cfg *Config, baseDir string) error {
	_ = baseDir

	var errs []error

	errs = append(errs, validateVersion(cfg)...)
	errs = append(errs, validateLLMClients(cfg)...)
	errs = append(errs, validateAgents(cfg)...)
	errs = append(errs, validateMCPServers(cfg)...)
	errs = append(errs, validateAudio(cfg)...)
	errs = append(errs, validateAttachments(cfg)...)
	errs = append(errs, validateIdentities(cfg)...)
	errs = append(errs, validateOrigins(cfg)...)
	errs = append(errs, validateChannels(cfg)...)
	errs = append(errs, validateMemory(cfg)...)
	errs = append(errs, validateSchedules(cfg)...)
	errs = append(errs, validateObservability(cfg)...)
	errs = append(errs, validateCourier(cfg)...)
	errs = append(errs, validateConversation(cfg)...)

	return joinErrors(errs)
}

// validReasoningEfforts énumère les niveaux de réflexion acceptés. La liste
// est dupliquée depuis internal/agent (qui les traduit en options genai)
// plutôt qu'importée : internal/config ne dépend d'aucun paquet applicatif,
// et l'écart éventuel est couvert par un test.
var validReasoningEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh"}

// validateLLMClients vérifie que chaque client déclaré a de quoi appeler un
// fournisseur.
//
// base_url est exigé, et non déduit d'un défaut : une valeur manquante
// enverrait les requêtes chez un autre fournisseur que celui de la clé, ou
// — quand le défaut du provider est écrasé par une chaîne vide — vers une
// URL relative qui n'échoue qu'au premier appel réel. C'est exactement le
// genre de panne qui se découvre des semaines après le déploiement, au
// premier message vocal reçu : mieux vaut refuser de démarrer.
func validateLLMClients(cfg *Config) []error {
	var errs []error

	for _, name := range sortedKeys(cfg.LLMClients) {
		client := cfg.LLMClients[name]

		if client.Provider == "" {
			errs = append(errs, fmt.Errorf("llm_clients.%s.provider: requis", name))
		}
		if client.Model == "" {
			errs = append(errs, fmt.Errorf("llm_clients.%s.model: requis", name))
		}
		if client.APIKey == "" {
			errs = append(errs, fmt.Errorf("llm_clients.%s.api_key: requis", name))
		}
		if client.BaseURL == "" {
			errs = append(errs, fmt.Errorf("llm_clients.%s.base_url: requis (point d'entrée du fournisseur, ex: https://api.openai.com/v1)", name))
		}

		if client.Reasoning != nil && client.Reasoning.Effort != "" && !slices.Contains(validReasoningEfforts, client.Reasoning.Effort) {
			errs = append(errs, fmt.Errorf("llm_clients.%s.reasoning.effort: %q inconnu (valeurs: %s)", name, client.Reasoning.Effort, strings.Join(validReasoningEfforts, ", ")))
		}
	}

	return errs
}

// validateConversation vérifie la section conversation (historique et
// compaction).
func validateConversation(cfg *Config) []error {
	var errs []error

	if cfg.Conversation.HistoryLimit < 0 {
		errs = append(errs, fmt.Errorf("conversation.history_limit: ne peut pas être négatif (valeur actuelle: %d)", cfg.Conversation.HistoryLimit))
	}

	compaction := cfg.Conversation.Compaction
	if compaction.MaxSummaryChars < 0 {
		errs = append(errs, fmt.Errorf("conversation.compaction.max_summary_chars: ne peut pas être négatif (valeur actuelle: %d)", compaction.MaxSummaryChars))
	}

	if !compaction.Enabled {
		return errs
	}

	if compaction.Client == "" {
		errs = append(errs, fmt.Errorf("conversation.compaction.client: requis lorsque conversation.compaction.enabled est vrai"))
	} else if _, ok := cfg.LLMClients[compaction.Client]; !ok {
		errs = append(errs, fmt.Errorf("conversation.compaction.client: client llm %q introuvable dans llm_clients", compaction.Client))
	}

	if compaction.MaxFacts < 0 {
		errs = append(errs, fmt.Errorf("conversation.compaction.max_facts: ne peut pas être négatif (valeur actuelle: %d)", compaction.MaxFacts))
	}

	return errs
}

// validateCourier vérifie la fenêtre de coalescence des rafales. Une valeur
// négative n'a pas de sens, et une fenêtre de plus de 30 secondes ferait
// passer l'assistant pour muet : chaque tour attend au moins la fenêtre
// entière avant de traiter.
func validateCourier(cfg *Config) []error {
	if cfg.Courier.CoalesceWindow == nil {
		return nil
	}

	window := cfg.Courier.CoalesceWindow.Duration()
	if window < 0 {
		return []error{fmt.Errorf("courier.coalesce_window: durée négative (%s)", window)}
	}
	if window > 30*time.Second {
		return []error{fmt.Errorf("courier.coalesce_window: %s dépasse le maximum de 30s (chaque réponse attendrait au moins ce délai)", window)}
	}

	return nil
}

func validateObservability(cfg *Config) []error {
	if !cfg.Observability.Enabled {
		return nil
	}

	if cfg.Observability.Addr == "" {
		return []error{fmt.Errorf("observability.addr: requis lorsque observability.enabled vaut true")}
	}

	return nil
}

func validateVersion(cfg *Config) []error {
	if cfg.Version != 1 {
		return []error{fmt.Errorf("version: doit valoir 1 (valeur actuelle: %d)", cfg.Version)}
	}

	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}

func validateAgents(cfg *Config) []error {
	var errs []error

	for _, name := range sortedKeys(cfg.Agents) {
		agent := cfg.Agents[name]
		prefix := fmt.Sprintf("agents.%s", name)

		switch agent.Type {
		case AgentTypeOrchestrator, AgentTypeSpecialist:
		default:
			errs = append(errs, fmt.Errorf("%s.type: valeur invalide %q (attendu orchestrator|specialist)", prefix, agent.Type))
		}

		if agent.Client == "" {
			errs = append(errs, fmt.Errorf("%s.client: requis", prefix))
		} else if _, ok := cfg.LLMClients[agent.Client]; !ok {
			errs = append(errs, fmt.Errorf("%s.client: client llm inconnu %q", prefix, agent.Client))
		}

		errs = append(errs, validateSystemPrompt(prefix, agent.SystemPrompt)...)

		for _, delegate := range agent.Delegates {
			target, ok := cfg.Agents[delegate]
			if !ok {
				errs = append(errs, fmt.Errorf("%s.delegates: agent inconnu %q", prefix, delegate))
				continue
			}

			if target.Type != AgentTypeSpecialist {
				errs = append(errs, fmt.Errorf("%s.delegates: %q n'est pas un agent de type specialist", prefix, delegate))
			}
		}

		for _, server := range agent.MCPServers {
			if _, ok := cfg.MCPServers[server]; !ok {
				errs = append(errs, fmt.Errorf("%s.mcp_servers: serveur mcp inconnu %q", prefix, server))
			}
		}

		errs = append(errs, validateAgentLimits(prefix, agent.Limits)...)
	}

	errs = append(errs, detectDelegationCycles(cfg)...)

	return errs
}

// validateAgentLimits vérifie que chaque limite d'exécution d'un agent est
// strictement positive. Une limite à zéro ou négative n'a pas de sens
// applicatif (elle interdirait tout appel ou tolérerait une taille
// négative) : elle doit être rejetée explicitement plutôt que silencieusement
// désactivée (PLAN.md Phase 7, §6.2 "ses limites").
func validateAgentLimits(prefix string, limits AgentLimits) []error {
	var errs []error

	if limits.MaxSequentialToolCalls <= 0 {
		errs = append(errs, fmt.Errorf("%s.limits.max_sequential_tool_calls: doit être strictement positif (valeur actuelle: %d)", prefix, limits.MaxSequentialToolCalls))
	}

	if limits.MaxActionsPerTurn <= 0 {
		errs = append(errs, fmt.Errorf("%s.limits.max_actions_per_turn: doit être strictement positif (valeur actuelle: %d)", prefix, limits.MaxActionsPerTurn))
	}

	if limits.ToolTimeout <= 0 {
		errs = append(errs, fmt.Errorf("%s.limits.tool_timeout: doit être strictement positif (valeur actuelle: %s)", prefix, limits.ToolTimeout.Duration()))
	}

	if limits.MaxToolResultBytes <= 0 {
		errs = append(errs, fmt.Errorf("%s.limits.max_tool_result_bytes: doit être strictement positif (valeur actuelle: %d)", prefix, limits.MaxToolResultBytes))
	}

	if limits.MaxToolContextBytes <= 0 {
		errs = append(errs, fmt.Errorf("%s.limits.max_tool_context_bytes: doit être strictement positif (valeur actuelle: %d)", prefix, limits.MaxToolContextBytes))
	}

	return errs
}

func validateSystemPrompt(prefix string, sp SystemPrompt) []error {
	var errs []error

	hasFile := sp.File != ""
	hasInline := sp.Inline != ""

	switch {
	case hasFile && hasInline:
		errs = append(errs, fmt.Errorf("%s.system_prompt: file et inline sont mutuellement exclusifs", prefix))
	case !hasFile && !hasInline:
		errs = append(errs, fmt.Errorf("%s.system_prompt: file ou inline requis", prefix))
	case hasFile:
		info, err := os.Stat(sp.File)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s.system_prompt.file: fichier introuvable ou illisible %q: %w", prefix, sp.File, err))
		} else if info.IsDir() {
			errs = append(errs, fmt.Errorf("%s.system_prompt.file: %q est un répertoire", prefix, sp.File))
		}
	}

	return errs
}

// detectDelegationCycles détecte les cycles dans le graphe de délégation des
// agents par un parcours en profondeur.
func detectDelegationCycles(cfg *Config) []error {
	const (
		stateUnvisited = 0
		stateVisiting  = 1
		stateDone      = 2
	)

	state := make(map[string]int, len(cfg.Agents))

	var errs []error

	var visit func(name string, path []string) bool

	visit = func(name string, path []string) bool {
		switch state[name] {
		case stateDone:
			return false
		case stateVisiting:
			errs = append(errs, fmt.Errorf("agents: cycle de délégation détecté: %s -> %s", strings.Join(path, " -> "), name))
			return true
		}

		state[name] = stateVisiting

		agent, ok := cfg.Agents[name]
		if ok {
			for _, delegate := range agent.Delegates {
				if _, ok := cfg.Agents[delegate]; !ok {
					continue
				}

				if visit(delegate, append(path, name)) {
					state[name] = stateDone
					return true
				}
			}
		}

		state[name] = stateDone

		return false
	}

	for _, name := range sortedKeys(cfg.Agents) {
		if state[name] == stateUnvisited {
			visit(name, nil)
		}
	}

	return errs
}

func validateMCPServers(cfg *Config) []error {
	var errs []error

	for _, name := range sortedKeys(cfg.MCPServers) {
		server := cfg.MCPServers[name]
		switch server.Transport {
		case "":
			errs = append(errs, fmt.Errorf("mcp_servers.%s.transport: requis", name))
		case "http", "streamable-http":
			if server.URL == "" {
				errs = append(errs, fmt.Errorf("mcp_servers.%s.url: requis pour un transport %s", name, server.Transport))
			}
			if len(server.Command) > 0 {
				errs = append(errs, fmt.Errorf("mcp_servers.%s.command: sans effet pour un transport %s", name, server.Transport))
			}
			if len(server.Env) > 0 {
				errs = append(errs, fmt.Errorf("mcp_servers.%s.env: sans effet pour un transport %s", name, server.Transport))
			}
		case "stdio":
			if len(server.Command) == 0 {
				errs = append(errs, fmt.Errorf("mcp_servers.%s.command: requis pour un transport stdio", name))
			}
			if server.URL != "" {
				errs = append(errs, fmt.Errorf("mcp_servers.%s.url: sans effet pour un transport stdio", name))
			}
			if len(server.Headers) > 0 {
				errs = append(errs, fmt.Errorf("mcp_servers.%s.headers: sans effet pour un transport stdio (utiliser env)", name))
			}
		default:
			errs = append(errs, fmt.Errorf("mcp_servers.%s.transport: %q non supporté (transports: %s)", name, server.Transport, SupportedMCPTransports))
		}

		if server.Resource != nil {
			if server.Resource.Key == "" {
				errs = append(errs, fmt.Errorf("mcp_servers.%s.resource.key: requis (clé lue dans channels[].resources)", name))
			}

			if server.Resource.Parameter == "" {
				errs = append(errs, fmt.Errorf("mcp_servers.%s.resource.parameter: requis (nom du paramètre attendu par le serveur)", name))
			}
		}

		// Sans domaine, l'application ne saurait pas quelle permission
		// exiger avant d'exécuter une action confirmée : elle exécuterait
		// donc une écriture externe sans contrôle d'autorisation.
		if server.Tools.ConfirmWrites && server.PermissionDomain == "" {
			errs = append(errs, fmt.Errorf("mcp_servers.%s.permission_domain: requis lorsque tools.confirm_writes est vrai", name))
		}

		if server.PermissionDomain != "" && strings.Contains(server.PermissionDomain, ".") {
			errs = append(errs, fmt.Errorf("mcp_servers.%s.permission_domain: %q ne doit pas contenir de point (le domaine est le premier segment de <domaine>.<portée>.<action>)", name, server.PermissionDomain))
		}

		if len(server.Tools.ReadPrefixes) > 0 && !server.Tools.ConfirmWrites {
			errs = append(errs, fmt.Errorf("mcp_servers.%s.tools.read_prefixes: sans effet lorsque tools.confirm_writes est faux (tous les outils s'exécutent déjà directement)", name))
		}

		if server.Tools.TrustReadOnlyHint && !server.Tools.ConfirmWrites {
			errs = append(errs, fmt.Errorf("mcp_servers.%s.tools.trust_read_only_hint: sans effet lorsque tools.confirm_writes est faux (aucun outil n'est soumis à confirmation)", name))
		}
	}

	return errs
}

func validateAudio(cfg *Config) []error {
	var errs []error

	if !cfg.Audio.Enabled {
		return nil
	}

	if cfg.Audio.TranscriptionClient == "" {
		errs = append(errs, fmt.Errorf("audio.transcription_client: requis lorsque audio.enabled est vrai"))
		return errs
	}

	if _, ok := cfg.LLMClients[cfg.Audio.TranscriptionClient]; !ok {
		errs = append(errs, fmt.Errorf("audio.transcription_client: client llm inconnu %q", cfg.Audio.TranscriptionClient))
	}

	return errs
}

// validateAttachments vérifie que les limites de pièces jointes sont
// exploitables. Aucune valeur par défaut n'est inventée : une section
// activée mais incomplète est une erreur de configuration, pas une invitation
// à deviner une taille ou une liste de types.
func validateAttachments(cfg *Config) []error {
	if !cfg.Attachments.Enabled {
		return nil
	}

	var errs []error

	if cfg.Attachments.MaxSize <= 0 {
		errs = append(errs, fmt.Errorf("attachments.max_size: doit être strictement positif lorsque attachments.enabled est vrai (valeur actuelle: %d)", cfg.Attachments.MaxSize))
	}

	if cfg.Attachments.MaxCount <= 0 {
		errs = append(errs, fmt.Errorf("attachments.max_count: doit être strictement positif lorsque attachments.enabled est vrai (valeur actuelle: %d)", cfg.Attachments.MaxCount))
	}

	if len(cfg.Attachments.AcceptedTypes) == 0 {
		errs = append(errs, fmt.Errorf("attachments.accepted_types: au moins un type mime est requis lorsque attachments.enabled est vrai"))
	}

	for i, mimeType := range cfg.Attachments.AcceptedTypes {
		if strings.TrimSpace(mimeType) == "" {
			errs = append(errs, fmt.Errorf("attachments.accepted_types[%d]: type mime vide", i))
			continue
		}

		if !strings.Contains(mimeType, "/") {
			errs = append(errs, fmt.Errorf("attachments.accepted_types[%d]: %q n'est pas un type mime valide (forme attendue: type/sous-type)", i, mimeType))
		}
	}

	if cfg.Attachments.MaxHistory < 0 {
		errs = append(errs, fmt.Errorf("attachments.max_history: ne peut pas être négatif (valeur actuelle: %d)", cfg.Attachments.MaxHistory))
	}

	if cfg.Attachments.MaxReply < 0 {
		errs = append(errs, fmt.Errorf("attachments.max_reply: ne peut pas être négatif (valeur actuelle: %d)", cfg.Attachments.MaxReply))
	}

	return errs
}

func validatePermission(perm string) bool {
	parts := strings.Split(perm, ".")
	if len(parts) != 3 {
		return false
	}

	domain, scope, action := parts[0], parts[1], parts[2]

	if domain == "" {
		return false
	}

	return validPermissionScopes[scope] && validPermissionActions[action]
}

func validateIdentities(cfg *Config) []error {
	var errs []error

	for _, roleName := range sortedKeys(cfg.Identities.Roles) {
		role := cfg.Identities.Roles[roleName]
		for _, perm := range role.Permissions {
			if !validatePermission(perm) {
				errs = append(errs, fmt.Errorf("identities.roles.%s.permissions: permission invalide %q (attendu <domaine>.<personal|group|org>.<read|write|delete>)", roleName, perm))
			}
		}
	}

	seenPrincipals := map[string]bool{}

	for i, principal := range cfg.Identities.Principals {
		prefix := fmt.Sprintf("identities.principals[%d]", i)

		if principal.ID == "" {
			errs = append(errs, fmt.Errorf("%s.id: requis", prefix))
		} else if seenPrincipals[principal.ID] {
			errs = append(errs, fmt.Errorf("identities.principals: identifiant dupliqué %q", principal.ID))
		} else {
			seenPrincipals[principal.ID] = true
		}

		switch principal.Kind {
		case PrincipalKindHuman, PrincipalKindService:
		default:
			errs = append(errs, fmt.Errorf("%s.kind: valeur invalide %q (attendu human|service)", prefix, principal.Kind))
		}

		for _, role := range principal.Roles {
			if _, ok := cfg.Identities.Roles[role]; !ok {
				errs = append(errs, fmt.Errorf("%s.roles: rôle inconnu %q", prefix, role))
			}
		}

		// Une surcharge visant un serveur inexistant ne serait jamais
		// appliquée : le principal se connecterait silencieusement avec le
		// jeton commun, donc potentiellement aux ressources de quelqu'un
		// d'autre. C'est une erreur de configuration, pas un détail.
		for _, serverName := range sortedKeys(principal.MCP) {
			server, known := cfg.MCPServers[serverName]
			if !known {
				errs = append(errs, fmt.Errorf("%s.mcp: serveur mcp inconnu %q", prefix, serverName))
			}

			override := principal.MCP[serverName]
			if override.URL == "" && len(override.Headers) == 0 && len(override.Values) == 0 {
				errs = append(errs, fmt.Errorf("%s.mcp.%s: surcharge vide (déclarer une url, un en-tête ou des values)", prefix, serverName))
			}

			if !known {
				continue
			}

			// Chaque champ de surcharge n'a de sens que pour un transport :
			// une surcharge silencieusement inopérante ferait croire à une
			// connexion personnelle qui n'existe pas.
			var placeholders []string
			switch server.Transport {
			case "stdio":
				if override.URL != "" {
					errs = append(errs, fmt.Errorf("%s.mcp.%s.url: sans effet pour un serveur stdio", prefix, serverName))
				}
				if len(override.Headers) > 0 {
					errs = append(errs, fmt.Errorf("%s.mcp.%s.headers: sans effet pour un serveur stdio", prefix, serverName))
				}
				placeholders = server.TemplatePlaceholders()
			case "http", "streamable-http":
				// Configuration EFFECTIVE : URL du principal si elle
				// remplace celle du serveur, en-têtes fusionnés. Les deux
				// transports HTTP se configurent de la même façon.
				placeholders = server.HTTPTemplatePlaceholders(override)
			default:
				continue
			}

			// Une surcharge incomplète ne démarrerait jamais : autant le
			// dire au chargement, avec les noms manquants (jamais les
			// valeurs, potentiellement secrètes). Et une valeur sans patron
			// correspondant est une surcharge inopérante — au mieux du bruit,
			// au pire la conviction erronée qu'une connexion personnelle
			// existe.
			declared := map[string]bool{}
			for _, placeholder := range placeholders {
				declared[placeholder] = true
				if _, ok := override.Values[placeholder]; !ok {
					errs = append(errs, fmt.Errorf("%s.mcp.%s.values: patron {{%s}} du serveur sans valeur", prefix, serverName, placeholder))
				}
			}
			for _, key := range sortedKeys(override.Values) {
				if !declared[key] {
					errs = append(errs, fmt.Errorf("%s.mcp.%s.values.%s: aucun patron {{%s}} dans la configuration du serveur", prefix, serverName, key, key))
				}
			}
		}
	}

	return errs
}

func principalExists(cfg *Config, id string) bool {
	for _, p := range cfg.Identities.Principals {
		if p.ID == id {
			return true
		}
	}

	return false
}

func validateOrigins(cfg *Config) []error {
	var errs []error

	seen := map[string]bool{}

	for i, origin := range cfg.Origins {
		prefix := fmt.Sprintf("origins[%d]", i)

		key := origin.Provider + "|" + origin.ExternalUserID
		if seen[key] {
			errs = append(errs, fmt.Errorf("origins: doublon pour le fournisseur %q et l'identifiant externe %q", origin.Provider, origin.ExternalUserID))
		} else {
			seen[key] = true
		}

		if origin.PrincipalID == "" {
			errs = append(errs, fmt.Errorf("%s.principal_id: requis", prefix))
		} else if !principalExists(cfg, origin.PrincipalID) {
			errs = append(errs, fmt.Errorf("%s.principal_id: principal inconnu %q", prefix, origin.PrincipalID))
		}
	}

	return errs
}

func validateChannels(cfg *Config) []error {
	var errs []error

	seen := map[string]bool{}

	for i, ch := range cfg.Channels {
		prefix := fmt.Sprintf("channels[%d]", i)

		key := ch.Provider + "|" + ch.ChannelID
		if seen[key] {
			errs = append(errs, fmt.Errorf("channels: doublon pour le fournisseur %q et le canal %q", ch.Provider, ch.ChannelID))
		} else {
			seen[key] = true
		}

		switch ch.Scope {
		case ScopePersonal, ScopeGroup, ScopeOrg:
		default:
			errs = append(errs, fmt.Errorf("%s.scope: valeur invalide %q (attendu personal|group|org)", prefix, ch.Scope))
		}

		switch ch.Kind {
		case ChannelKindPrivate:
			if ch.PrincipalID == "" {
				errs = append(errs, fmt.Errorf("%s.principal_id: requis pour un canal privé", prefix))
			} else if !principalExists(cfg, ch.PrincipalID) {
				errs = append(errs, fmt.Errorf("%s.principal_id: principal inconnu %q", prefix, ch.PrincipalID))
			}

			if len(ch.Members) > 0 {
				errs = append(errs, fmt.Errorf("%s.members: doit être vide pour un canal privé", prefix))
			}
		case ChannelKindGroup:
			if len(ch.Members) == 0 {
				errs = append(errs, fmt.Errorf("%s.members: requis pour un canal de groupe", prefix))
			}

			if ch.Activation != "mention" {
				errs = append(errs, fmt.Errorf("%s.activation: valeur invalide %q (seul mention est supporté en V1)", prefix, ch.Activation))
			}
		default:
			errs = append(errs, fmt.Errorf("%s.kind: valeur invalide %q (attendu private|group)", prefix, ch.Kind))
		}

		for _, member := range ch.Members {
			if !principalExists(cfg, member) {
				errs = append(errs, fmt.Errorf("%s.members: principal inconnu %q", prefix, member))
			}
		}
	}

	return errs
}

func channelExists(cfg *Config, provider, channelID string) bool {
	for _, ch := range cfg.Channels {
		if ch.Provider == provider && ch.ChannelID == channelID {
			return true
		}
	}

	return false
}

func validateMemory(cfg *Config) []error {
	var errs []error

	seen := map[string]bool{}

	for i, idx := range cfg.Memory.Indexes {
		prefix := fmt.Sprintf("memory.indexes[%d]", i)

		if idx.ID == "" {
			errs = append(errs, fmt.Errorf("%s.id: requis", prefix))
		} else if seen[idx.ID] {
			errs = append(errs, fmt.Errorf("memory.indexes: identifiant dupliqué %q", idx.ID))
		} else {
			seen[idx.ID] = true
		}
	}

	consolidation := cfg.Memory.Consolidation
	if consolidation.MinMemories < 0 {
		errs = append(errs, fmt.Errorf("memory.consolidation.min_memories: ne peut pas être négatif (valeur actuelle: %d)", consolidation.MinMemories))
	}

	if consolidation.Enabled {
		if consolidation.Client == "" {
			errs = append(errs, fmt.Errorf("memory.consolidation.client: requis lorsque memory.consolidation.enabled est vrai"))
		} else if _, ok := cfg.LLMClients[consolidation.Client]; !ok {
			errs = append(errs, fmt.Errorf("memory.consolidation.client: client llm %q introuvable dans llm_clients", consolidation.Client))
		}

		if consolidation.Cron != "" {
			if _, err := cron.ParseStandard(consolidation.Cron); err != nil {
				errs = append(errs, fmt.Errorf("memory.consolidation.cron: expression cron invalide %q: %w", consolidation.Cron, err))
			}
		}
	}

	return errs
}

func validateSchedules(cfg *Config) []error {
	var errs []error

	seen := map[string]bool{}

	for i, sched := range cfg.Schedules {
		prefix := fmt.Sprintf("schedules[%d]", i)

		if sched.ID == "" {
			errs = append(errs, fmt.Errorf("%s.id: requis", prefix))
		} else if seen[sched.ID] {
			errs = append(errs, fmt.Errorf("schedules: identifiant dupliqué %q", sched.ID))
		} else {
			seen[sched.ID] = true
		}

		if _, err := cron.ParseStandard(sched.Schedule.Cron); err != nil {
			errs = append(errs, fmt.Errorf("%s.schedule.cron: expression cron invalide %q: %w", prefix, sched.Schedule.Cron, err))
		}

		if _, err := time.LoadLocation(sched.Schedule.Timezone); err != nil {
			errs = append(errs, fmt.Errorf("%s.schedule.timezone: fuseau horaire invalide %q: %w", prefix, sched.Schedule.Timezone, err))
		}

		if sched.Execution.PrincipalID == "" {
			errs = append(errs, fmt.Errorf("%s.execution.principal_id: requis", prefix))
		} else if !principalExists(cfg, sched.Execution.PrincipalID) {
			errs = append(errs, fmt.Errorf("%s.execution.principal_id: principal inconnu %q", prefix, sched.Execution.PrincipalID))
		}

		if sched.Execution.Agent == "" {
			errs = append(errs, fmt.Errorf("%s.execution.agent: requis", prefix))
		} else if _, ok := cfg.Agents[sched.Execution.Agent]; !ok {
			errs = append(errs, fmt.Errorf("%s.execution.agent: agent inconnu %q", prefix, sched.Execution.Agent))
		}

		switch sched.Execution.Scope {
		case ScopePersonal, ScopeGroup, ScopeOrg:
		default:
			errs = append(errs, fmt.Errorf("%s.execution.scope: valeur invalide %q (attendu personal|group|org)", prefix, sched.Execution.Scope))
		}

		switch sched.Execution.Actions.Policy {
		case ActionsPolicyReadOnly, ActionsPolicyRequireConfirmation:
		default:
			errs = append(errs, fmt.Errorf("%s.execution.actions.policy: valeur invalide %q (attendu read_only|require_confirmation)", prefix, sched.Execution.Actions.Policy))
		}

		switch sched.Delivery.Mode {
		case DeliveryModeAlways, DeliveryModeOnContent, DeliveryModeOnFailure:
		default:
			errs = append(errs, fmt.Errorf("%s.delivery.mode: valeur invalide %q (attendu always|on_content|on_failure)", prefix, sched.Delivery.Mode))
		}

		if sched.Delivery.ChannelID == "" {
			errs = append(errs, fmt.Errorf("%s.delivery.channel_id: requis", prefix))
		} else if !channelExists(cfg, sched.Delivery.Provider, sched.Delivery.ChannelID) {
			errs = append(errs, fmt.Errorf("%s.delivery.channel_id: canal inconnu pour le fournisseur %q: %q", prefix, sched.Delivery.Provider, sched.Delivery.ChannelID))
		}

		switch sched.Concurrency.Policy {
		case "":
			// Défaut: forbid, appliqué au niveau applicatif.
		case ConcurrencyPolicyForbid, ConcurrencyPolicyAllow:
		default:
			errs = append(errs, fmt.Errorf("%s.concurrency.policy: valeur invalide %q (attendu forbid|allow)", prefix, sched.Concurrency.Policy))
		}
	}

	return errs
}
