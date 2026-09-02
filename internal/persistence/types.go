package persistence

import "github.com/bornholm/automata/internal/model"

// Ce fichier définit les DTO de persistance minimalistes pour les entités
// qui n'existent pas encore dans internal/model (plan de conception, §13.1). Ce ne
// sont pas les modèles métier riches des phases ultérieures : ils seront
// affinés quand ces phases arriveront.

// ActionPlanID identifie un plan d'actions.
type ActionPlanID string

// ActionID identifie une action au sein d'un plan.
type ActionID string

// ScheduledRunID identifie une exécution planifiée.
type ScheduledRunID string

// DeliveryAttemptID identifie une tentative de livraison.
type DeliveryAttemptID string

// AuditEventID identifie un événement d'audit.
type AuditEventID string

// Principal est le DTO de persistance de la table principals.
type Principal struct {
	ID          model.PrincipalID
	OrgID       model.OrgID
	Kind        model.PrincipalKind
	DisplayName string
	CreatedAt   string
	UpdatedAt   string
}

// Conversation est le DTO de persistance de la table conversations.
type Conversation struct {
	ID                model.ConversationID
	OrgID             model.OrgID
	Provider          string
	ExternalChannelID string
	Kind              model.ChannelKind
	Scope             model.Scope
	ScopeID           model.ScopeID
	CreatedAt         string
	UpdatedAt         string
}

// Message est le DTO de persistance de la table messages.
type Message struct {
	ID                string
	ConversationID    model.ConversationID
	ExternalMessageID string
	PrincipalID       model.PrincipalID
	Role              string
	Content           string
	ContentKind       string
	CreatedAt         string
}

// MessageAttachment est le DTO de persistance de la table
// message_attachments : une pièce jointe conservée pour être rejouée dans
// l'historique remis au modèle.
//
// Ne porte jamais d'audio : les notes vocales sont transcrites sans être
// conservées (plan de conception, §3.4).
type MessageAttachment struct {
	ID        string
	MessageID string
	Position  int
	Kind      string
	MimeType  string
	Filename  string
	Caption   string
	Data      []byte
	CreatedAt string
	// ToolOnly marque une pièce jointe réservée aux outils, jamais rejouée
	// vers le modèle (voir migration 0018 et internal/media).
	ToolOnly bool
}

// ProcessedMessage est le DTO de persistance de la table processed_messages.
type ProcessedMessage struct {
	Provider          string
	ExternalMessageID string
	ProcessedAt       string
	Status            string
}

// ActionPlan est le DTO de persistance de la table action_plans.
type ActionPlan struct {
	ID             ActionPlanID
	OrgID          model.OrgID
	ConversationID model.ConversationID
	CreatedBy      model.PrincipalID
	Scope          model.Scope
	ScopeID        model.ScopeID
	Status         string
	ExpiresAt      *string
	CreatedAt      string
	UpdatedAt      string
}

// Action est le DTO de persistance de la table actions.
type Action struct {
	ID                   ActionID
	PlanID               ActionPlanID
	Position             int
	AgentID              string
	MCPServer            string
	ToolName             string
	ArgumentsJSON        string
	Summary              string
	RequiredPermission   string
	RequiresConfirmation bool
	Status               string
	ErrorCode            *string
	CreatedAt            string
	StartedAt            *string
	CompletedAt          *string
}

// ScheduledRun est le DTO de persistance de la table scheduled_runs.
type ScheduledRun struct {
	ID             ScheduledRunID
	ScheduleID     string
	ScheduledFor   string
	StartedAt      *string
	CompletedAt    *string
	Status         string
	PrincipalID    model.PrincipalID
	OrgID          model.OrgID
	Scope          model.Scope
	ScopeID        model.ScopeID
	AgentID        string
	ErrorCode      *string
	DeliveryStatus *string
	CreatedAt      string
}

// DeliveryAttempt est le DTO de persistance de la table delivery_attempts.
type DeliveryAttempt struct {
	ID             DeliveryAttemptID
	ScheduledRunID ScheduledRunID
	Provider       string
	ChannelID      string
	Attempt        int
	Status         string
	ErrorCode      *string
	CreatedAt      string
	CompletedAt    *string
}

// AuditEvent est le DTO de persistance de la table audit_events.
type AuditEvent struct {
	ID              AuditEventID
	OrgID           model.OrgID
	PrincipalID     model.PrincipalID
	Trigger         model.Trigger
	ConversationID  *model.ConversationID
	EventType       string
	ResourceKind    string
	ResourceScope   model.Scope
	ResourceScopeID model.ScopeID
	Outcome         string
	MetadataJSON    *string
	CreatedAt       string
}

// ReminderID identifie un rappel ponctuel.
type ReminderID string

// Reminder est le DTO de persistance de la table reminders : un rappel
// ponctuel créé conversationnellement, délivré une seule fois sur le canal
// où il a été demandé. Message est du contenu privé : ne jamais le
// journaliser.
type Reminder struct {
	ID             ReminderID
	OrgID          model.OrgID
	PrincipalID    model.PrincipalID
	ConversationID model.ConversationID
	Provider       string
	ChannelID      string
	Message        string
	FireAt         string
	Status         string
	CreatedAt      string
	SentAt         *string
	// Recurrence est une expression cron standard (5 champs, dialecte de
	// cron.ParseStandard, le même que schedules) ; vide pour un rappel à
	// déclenchement unique. Un rappel récurrent reste pending après chaque
	// envoi, FireAt avançant sur l'occurrence suivante.
	Recurrence string
	// Timezone est le fuseau IANA dans lequel Recurrence s'évalue ; vide
	// pour un rappel à déclenchement unique.
	Timezone string
	// Attempts compte les tentatives de livraison infructueuses de
	// l'échéance courante (voir migration 0008). Remis à zéro à chaque
	// réarmement de récurrence.
	Attempts int
	// Kind distingue un rappel (ReminderKindMessage : Message est délivré
	// tel quel) d'une tâche planifiée (ReminderKindTask : Message est une
	// consigne donnée à AgentID, dont la réponse est délivrée).
	Kind string
	// AgentID est l'agent exécutant une tâche planifiée, figé à sa
	// création. Vide pour un rappel.
	AgentID string
}

// ConversationSummary est le DTO de persistance de la table
// conversation_summaries : le résumé roulant des messages d'une conversation
// plus anciens que la fenêtre d'historique (compaction,
// internal/conversation.Compactor). Summary est du contenu privé : ne jamais
// le journaliser.
type ConversationSummary struct {
	ConversationID model.ConversationID
	Summary        string
	// LastMessageRowID est le rowid du dernier message couvert : les
	// messages de rowid supérieur restent rejoués verbatim.
	LastMessageRowID int64
	MessagesCovered  int64
	UpdatedAt        string
}
