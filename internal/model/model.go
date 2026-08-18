// Package model contient les types métier partagés d'Automata : identité,
// portée, conversation. Ces types sont indépendants de la configuration YAML
// (voir internal/config) : ce sont les types de domaine internes utilisés
// par le reste de l'application.
package model

import "context"

// OrgID identifie une organisation.
type OrgID string

// PrincipalID identifie un principal (humain ou service).
type PrincipalID string

// ConversationID identifie une conversation applicative.
type ConversationID string

// ScopeID identifie une ressource au sein d'une portée (identifiant de
// principal pour personal, identifiant de groupe pour group, identifiant
// d'organisation pour org).
type ScopeID string

// Scope énumère les portées de ressources supportées.
type Scope string

const (
	ScopePersonal Scope = "personal"
	ScopeGroup    Scope = "group"
	ScopeOrg      Scope = "org"
)

// PrincipalKind énumère les types de principaux supportés.
type PrincipalKind string

const (
	PrincipalHuman   PrincipalKind = "human"
	PrincipalService PrincipalKind = "service"
)

// Principal décrit un principal (humain ou service) connu de l'organisation.
type Principal struct {
	ID          PrincipalID
	OrgID       OrgID
	Kind        PrincipalKind
	DisplayName string
	Roles       []string
}

// ChannelKind énumère les types de canaux supportés.
type ChannelKind string

const (
	ChannelPrivate ChannelKind = "private"
	ChannelGroup   ChannelKind = "group"
)

// Conversation décrit une conversation applicative rattachée à un canal.
type Conversation struct {
	ID          ConversationID
	OrgID       OrgID
	Provider    string
	ChannelID   string
	Kind        ChannelKind
	Scope       Scope
	ScopeID     ScopeID
	DisplayName string
}

// Trigger énumère les origines de déclenchement d'une exécution.
type Trigger string

const (
	TriggerMessage Trigger = "message"
	// TriggerCron est une occurrence d'un schedule DÉCLARÉ EN CONFIGURATION,
	// exécutée sous un principal de service que personne n'a nommément
	// mandaté : elle n'accède jamais aux ressources personnelles.
	TriggerCron Trigger = "cron"
	// TriggerScheduledTask est une tâche créée CONVERSATIONNELLEMENT par un
	// humain (outil schedule_task), rejouée plus tard sous SON identité,
	// dans SA conversation, et livrée là seulement.
	//
	// Elle suit donc les règles de son canal d'origine, comme un message :
	// elle ne peut rien faire que son auteur ne puisse demander en direct,
	// et la traiter comme un cron de configuration reviendrait à lui refuser
	// l'accès à ses propres données — ce qui rend la fonctionnalité inutile
	// (« je n'ai pas l'autorisation d'accéder aux ressources nécessaires »).
	TriggerScheduledTask Trigger = "scheduled_task"
)

// ExecutionIdentity décrit l'identité d'exécution résolue pour un
// déclenchement (message entrant ou tâche planifiée). Elle est construite
// exclusivement par l'application à partir de la configuration, jamais à
// partir d'un contenu fourni par le LLM ou l'utilisateur.
type ExecutionIdentity struct {
	Trigger        Trigger
	PrincipalID    PrincipalID
	OrgID          OrgID
	ConversationID ConversationID
	Provider       string
	ChannelID      string
	ChannelKind    ChannelKind
	Scope          Scope
	ScopeID        ScopeID
}

// executionIdentityContextKey est un type dédié pour éviter toute collision
// de clé de contexte avec d'autres packages.
type executionIdentityContextKey struct{}

// WithExecutionIdentity place identity dans le contexte ctx.
func WithExecutionIdentity(ctx context.Context, identity ExecutionIdentity) context.Context {
	return context.WithValue(ctx, executionIdentityContextKey{}, identity)
}

// ExecutionIdentityFromContext extrait l'identité d'exécution du contexte
// ctx, si elle y a été placée par WithExecutionIdentity.
func ExecutionIdentityFromContext(ctx context.Context) (ExecutionIdentity, bool) {
	identity, ok := ctx.Value(executionIdentityContextKey{}).(ExecutionIdentity)
	return identity, ok
}
