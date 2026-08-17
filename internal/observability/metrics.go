// Package observability fournit un registre de métriques applicatives en
// mémoire (PLAN.md §14.3) et un serveur HTTP local optionnel exposant la
// santé du processus (liveness/readiness) et un export JSON de ces
// métriques (PLAN.md Phase 20).
//
// Choix délibéré : aucune dépendance externe (pas de client Prometheus).
// Un registre de compteurs atomiques et d'agrégats de latence simples
// (somme, compte, min, max — pas un histogramme complet) suffit à
// diagnostiquer une panne sans lire le contenu des conversations privées
// (AGENTS.md, "ne pas journaliser les contenus privés"), et reste
// entièrement dans la stdlib (sync/atomic), conformément à PLAN.md §4.2.
// L'export au format texte Prometheus n'est pas fourni : ce serait un
// travail supplémentaire non demandé explicitement par PLAN.md §14.3 (qui
// ne prescrit aucun format particulier) ; un export JSON simple, servi par
// /metrics, est suffisant pour l'exploitation et beaucoup plus simple à
// produire et à faire évoluer.
package observability

import (
	"encoding/json"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics est un registre de compteurs et d'agrégats de latence, sûr pour
// un accès concurrent (chaque champ est soit atomique, soit protégé par un
// mutex dédié). Une valeur nil est utilisable : toutes les méthodes sont
// no-op sur un récepteur nil, pour que l'injection de *Metrics reste
// optionnelle partout dans l'application (aucun composant n'a besoin de
// tester cfg.Observability.Enabled avant d'appeler une méthode).
type Metrics struct {
	messagesReceived         atomic.Int64
	messagesIgnoredNoMention atomic.Int64
	messagesCoalesced        atomic.Int64
	unknownOrigin            atomic.Int64
	duplicateMessages        atomic.Int64
	actionsProposed          atomic.Int64
	actionsConfirmed         atomic.Int64
	memorySearches           atomic.Int64
	remindersCreated         atomic.Int64
	remindersSent            atomic.Int64
	conversationsCompacted   atomic.Int64
	deliveryErrors           atomic.Int64
	toolResultsTruncated     atomic.Int64

	transcriptionLatency latencyStat
	agentLatency         latencyStat

	mu                 sync.Mutex
	delegationsByAgent map[string]int64
	mcpCalls           map[mcpCallKey]*mcpCallStat
	cronOccurrences    map[string]int64
}

// mcpCallKey identifie un couple (serveur, outil) MCP.
type mcpCallKey struct {
	Server string
	Tool   string
}

// mcpCallStat compte les appels réussis/échoués pour un couple (serveur,
// outil) MCP donné.
type mcpCallStat struct {
	Success int64
	Error   int64
}

// latencyStat agrège une série de durées observées : compte, somme, min,
// max. Toutes les opérations sont protégées par un mutex dédié : la
// fréquence des observations (par tour de conversation, par appel de
// transcription) ne justifie pas la complexité d'une implémentation
// entièrement lock-free.
type latencyStat struct {
	mu    sync.Mutex
	count int64
	sum   time.Duration
	min   time.Duration
	max   time.Duration
}

func (l *latencyStat) observe(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.count == 0 || d < l.min {
		l.min = d
	}
	if l.count == 0 || d > l.max {
		l.max = d
	}
	l.sum += d
	l.count++
}

// snapshot retourne un instantané exportable de l'agrégat de latence.
type latencySnapshot struct {
	Count int64   `json:"count"`
	SumMS float64 `json:"sum_ms"`
	AvgMS float64 `json:"avg_ms"`
	MinMS float64 `json:"min_ms"`
	MaxMS float64 `json:"max_ms"`
}

func (l *latencyStat) snapshot() latencySnapshot {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.count == 0 {
		return latencySnapshot{}
	}

	avg := float64(l.sum) / float64(l.count)

	return latencySnapshot{
		Count: l.count,
		SumMS: durationMS(l.sum),
		AvgMS: math.Round(avg/float64(time.Millisecond)*1000) / 1000,
		MinMS: durationMS(l.min),
		MaxMS: durationMS(l.max),
	}
}

func durationMS(d time.Duration) float64 {
	return math.Round(float64(d)/float64(time.Millisecond)*1000) / 1000
}

// NewMetrics construit un registre de métriques vide, prêt à l'emploi.
func NewMetrics() *Metrics {
	return &Metrics{
		delegationsByAgent: make(map[string]int64),
		mcpCalls:           make(map[mcpCallKey]*mcpCallStat),
		cronOccurrences:    make(map[string]int64),
	}
}

// IncMessagesReceived incrémente le compteur de messages entrants reçus par
// l'ingress, avant toute résolution d'identité ou filtrage.
func (m *Metrics) IncMessagesReceived() {
	if m == nil {
		return
	}
	m.messagesReceived.Add(1)
}

// IncMessagesIgnoredNoMention incrémente le compteur de messages de groupe
// ignorés faute de mention explicite de l'assistant (PLAN.md §3.3).
func (m *Metrics) IncMessagesIgnoredNoMention() {
	if m == nil {
		return
	}
	m.messagesIgnoredNoMention.Add(1)
}

// IncMessagesCoalesced ajoute n au compteur de messages absorbés par la
// coalescence des rafales ingress : pour une rafale de k messages fusionnés
// en un tour, n vaut k-1 (le tour lui-même reste compté une fois dans
// messages_received par message d'origine).
func (m *Metrics) IncMessagesCoalesced(n int) {
	if m == nil {
		return
	}
	m.messagesCoalesced.Add(int64(n))
}

// IncUnknownOrigin incrémente le compteur d'origines refusées (émetteur
// externe non déclaré dans identities.origins).
func (m *Metrics) IncUnknownOrigin() {
	if m == nil {
		return
	}
	m.unknownOrigin.Add(1)
}

// IncDuplicateMessage incrémente le compteur de messages déjà traités
// (déduplication ingress).
func (m *Metrics) IncDuplicateMessage() {
	if m == nil {
		return
	}
	m.duplicateMessages.Add(1)
}

// ObserveTranscriptionLatency enregistre la durée d'une transcription audio
// (PLAN.md §3.4).
func (m *Metrics) ObserveTranscriptionLatency(d time.Duration) {
	if m == nil {
		return
	}
	m.transcriptionLatency.observe(d)
}

// ObserveAgentLatency enregistre la durée d'une exécution d'agent
// (Agent.Execute), tous types d'agents confondus (latence de réponse).
func (m *Metrics) ObserveAgentLatency(d time.Duration) {
	if m == nil {
		return
	}
	m.agentLatency.observe(d)
}

// IncDelegation incrémente le compteur de délégations vers l'agent
// spécialiste identifié par agentID (OrchestratorAgent, outil
// "delegate_to_<agentID>").
func (m *Metrics) IncDelegation(agentID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.delegationsByAgent[agentID]++
}

// IncMCPCall incrémente le compteur d'appels d'outil MCP pour (server,
// tool), en succès ou en erreur selon que err est nil.
func (m *Metrics) IncMCPCall(server, tool string, err error) {
	if m == nil {
		return
	}

	key := mcpCallKey{Server: server, Tool: tool}

	m.mu.Lock()
	defer m.mu.Unlock()

	stat, ok := m.mcpCalls[key]
	if !ok {
		stat = &mcpCallStat{}
		m.mcpCalls[key] = stat
	}

	if err != nil {
		stat.Error++
	} else {
		stat.Success++
	}
}

// IncActionProposed incrémente le compteur de plans d'actions proposés
// (action.Engine.CreatePlan).
func (m *Metrics) IncActionProposed() {
	if m == nil {
		return
	}
	m.actionsProposed.Add(1)
}

// IncActionConfirmed incrémente le compteur de plans d'actions confirmés
// par un humain (action.Engine, commande "confirmer").
func (m *Metrics) IncActionConfirmed() {
	if m == nil {
		return
	}
	m.actionsConfirmed.Add(1)
}

// IncMemorySearch incrémente le compteur de recherches dans la mémoire
// persistante (outil "search_memory").
func (m *Metrics) IncMemorySearch() {
	if m == nil {
		return
	}
	m.memorySearches.Add(1)
}

// IncReminderCreated incrémente le compteur de rappels ponctuels créés via
// l'outil create_reminder.
func (m *Metrics) IncReminderCreated() {
	if m == nil {
		return
	}
	m.remindersCreated.Add(1)
}

// IncReminderSent incrémente le compteur de rappels ponctuels effectivement
// délivrés par le dispatcher (internal/reminder).
func (m *Metrics) IncReminderSent() {
	if m == nil {
		return
	}
	m.remindersSent.Add(1)
}

// IncConversationCompacted incrémente le compteur de compactions
// d'historique de conversation (internal/conversation.Compactor).
func (m *Metrics) IncConversationCompacted() {
	if m == nil {
		return
	}
	m.conversationsCompacted.Add(1)
}

// IncCronOccurrence incrémente le compteur d'occurrences planifiées
// déclenchées pour le schedule scheduleID.
func (m *Metrics) IncCronOccurrence(scheduleID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cronOccurrences[scheduleID]++
}

// IncDeliveryError incrémente le compteur d'erreurs de livraison (envoi de
// résultat planifié échoué).
func (m *Metrics) IncDeliveryError() {
	if m == nil {
		return
	}
	m.deliveryErrors.Add(1)
}

// IncToolResultTruncated incrémente le compteur de résultats d'outil MCP
// tronqués (limite AgentLimits.MaxToolResultBytes atteinte).
func (m *Metrics) IncToolResultTruncated() {
	if m == nil {
		return
	}
	m.toolResultsTruncated.Add(1)
}

// Snapshot retourne un instantané exportable, sérialisable en JSON, de
// l'ensemble des métriques courantes. Ne contient jamais de contenu de
// message, de transcription ni d'argument d'outil : uniquement des
// compteurs agrégés et des identifiants déjà présents dans la configuration
// (agent_id, schedule_id, server/tool MCP).
func (m *Metrics) Snapshot() map[string]any {
	if m == nil {
		return map[string]any{}
	}

	m.mu.Lock()
	delegations := make(map[string]int64, len(m.delegationsByAgent))
	for k, v := range m.delegationsByAgent {
		delegations[k] = v
	}

	mcpCalls := make(map[string]map[string]int64, len(m.mcpCalls))
	for k, v := range m.mcpCalls {
		perServer, ok := mcpCalls[k.Server]
		if !ok {
			perServer = make(map[string]int64)
			mcpCalls[k.Server] = perServer
		}
		perServer[k.Tool+"_success"] = v.Success
		perServer[k.Tool+"_error"] = v.Error
	}

	cronOccurrences := make(map[string]int64, len(m.cronOccurrences))
	for k, v := range m.cronOccurrences {
		cronOccurrences[k] = v
	}
	m.mu.Unlock()

	return map[string]any{
		"messages_received":           m.messagesReceived.Load(),
		"messages_ignored_no_mention": m.messagesIgnoredNoMention.Load(),
		"messages_coalesced":          m.messagesCoalesced.Load(),
		"unknown_origin":              m.unknownOrigin.Load(),
		"duplicate_messages":          m.duplicateMessages.Load(),
		"actions_proposed":            m.actionsProposed.Load(),
		"actions_confirmed":           m.actionsConfirmed.Load(),
		"memory_searches":             m.memorySearches.Load(),
		"reminders_created":           m.remindersCreated.Load(),
		"reminders_sent":              m.remindersSent.Load(),
		"conversations_compacted":     m.conversationsCompacted.Load(),
		"delivery_errors":             m.deliveryErrors.Load(),
		"tool_results_truncated":      m.toolResultsTruncated.Load(),
		"transcription_latency":       m.transcriptionLatency.snapshot(),
		"agent_latency":               m.agentLatency.snapshot(),
		"delegations_by_agent":        delegations,
		"mcp_calls":                   mcpCalls,
		"cron_occurrences":            cronOccurrences,
	}
}

// EncodeJSON sérialise Snapshot() en JSON indenté sur w. Fournie pour un
// usage hors HTTP (ex : diagnostic en ligne de commande) ; le serveur HTTP
// (Server, http.go) l'utilise pour /metrics. Nommée différemment de
// io.WriterTo.WriteTo (signature (int64, error)) pour ne pas laisser croire
// à une implémentation de cette interface standard.
func (m *Metrics) EncodeJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(m.Snapshot())
}
