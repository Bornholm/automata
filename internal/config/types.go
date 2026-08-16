package config

import (
	"fmt"
	"time"

	"github.com/dustin/go-humanize"
)

// Duration enveloppe time.Duration pour permettre un unmarshalling YAML à
// partir d'une chaîne ("5s", "2m", "10m").
type Duration time.Duration

// UnmarshalYAML implémente yaml.Unmarshaler pour Duration.
func (d *Duration) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}

	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("durée invalide %q: %w", s, err)
	}

	*d = Duration(parsed)

	return nil
}

// Duration retourne la valeur time.Duration sous-jacente.
func (d Duration) Duration() time.Duration {
	return time.Duration(d)
}

// ByteSize enveloppe une taille en octets pour permettre un unmarshalling
// YAML à partir d'une chaîne ("20MiB", "16KiB", "64KiB").
type ByteSize uint64

// UnmarshalYAML implémente yaml.Unmarshaler pour ByteSize.
func (b *ByteSize) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}

	parsed, err := humanize.ParseBytes(s)
	if err != nil {
		return fmt.Errorf("taille invalide %q: %w", s, err)
	}

	*b = ByteSize(parsed)

	return nil
}

// Bytes retourne la valeur uint64 sous-jacente.
func (b ByteSize) Bytes() uint64 {
	return uint64(b)
}

// Config est la racine de la configuration YAML d'Automata.
type Config struct {
	Version       int                  `yaml:"version"`
	Organization  Organization         `yaml:"organization"`
	Storage       Storage              `yaml:"storage"`
	Courier       Courier              `yaml:"courier"`
	Audio         Audio                `yaml:"audio"`
	Attachments   Attachments          `yaml:"attachments"`
	LLMClients    map[string]LLMClient `yaml:"llm_clients"`
	Agents        map[string]Agent     `yaml:"agents"`
	MCPServers    map[string]MCPServer `yaml:"mcp_servers"`
	Memory        Memory               `yaml:"memory"`
	Identities    Identities           `yaml:"identities"`
	Origins       []Origin             `yaml:"origins"`
	Channels      []Channel            `yaml:"channels"`
	Schedules     []Schedule           `yaml:"schedules"`
	Observability Observability        `yaml:"observability"`
}

// Observability décrit le serveur HTTP local optionnel de santé et de
// métriques (PLAN.md Phase 20). Désactivé par défaut : une section absente
// (ou Enabled: false) ne démarre aucun serveur HTTP. Aucune valeur par
// défaut n'est inventée pour Addr au-delà de cette désactivation par
// défaut ; lorsque Enabled est vrai, Addr est requis.
type Observability struct {
	Enabled bool   `yaml:"enabled"`
	Addr    string `yaml:"addr"`
}

// Organization décrit l'organisation propriétaire de l'instance.
type Organization struct {
	ID          string `yaml:"id"`
	DisplayName string `yaml:"display_name"`
}

// Storage décrit le stockage applicatif.
type Storage struct {
	Application StorageApplication `yaml:"application"`
}

// StorageApplication décrit la base SQLite applicative.
type StorageApplication struct {
	Driver  string  `yaml:"driver"`
	Path    string  `yaml:"path"`
	Pragmas Pragmas `yaml:"pragmas"`
}

// Pragmas décrit les pragmas SQLite appliqués à l'ouverture.
type Pragmas struct {
	ForeignKeys bool     `yaml:"foreign_keys"`
	JournalMode string   `yaml:"journal_mode"`
	BusyTimeout Duration `yaml:"busy_timeout"`
}

// Courier décrit la configuration des fournisseurs de messagerie.
type Courier struct {
	Providers map[string]CourierProvider `yaml:"providers"`
}

// CourierProvider décrit un fournisseur Go Courier. Le champ Type est requis,
// les autres champs sont libres et conservés dans Extra.
type CourierProvider struct {
	Type  string                 `yaml:"type"`
	Extra map[string]interface{} `yaml:",inline"`
}

// UnmarshalYAML implémente un décodage manuel pour capturer à la fois le
// champ Type et les champs libres restants dans Extra.
func (p *CourierProvider) UnmarshalYAML(unmarshal func(interface{}) error) error {
	raw := map[string]interface{}{}
	if err := unmarshal(&raw); err != nil {
		return err
	}

	typ, _ := raw["type"].(string)
	p.Type = typ
	delete(raw, "type")
	p.Extra = raw

	return nil
}

// Audio décrit la configuration de traitement des messages vocaux.
type Audio struct {
	Enabled              bool     `yaml:"enabled"`
	TranscriptionClient  string   `yaml:"transcription_client"`
	MaxSize              ByteSize `yaml:"max_size"`
	Timeout              Duration `yaml:"timeout"`
	PersistAudio         bool     `yaml:"persist_audio"`
	PersistTranscription bool     `yaml:"persist_transcription"`
}

// Attachments décrit le traitement des pièces jointes non vocales (images,
// documents) reçues et renvoyées : les notes vocales relèvent d'Audio.
//
// Désactivé par défaut : sans section explicite, une pièce jointe est écartée
// et signalée à l'agent, jamais transmise au modèle à son insu.
type Attachments struct {
	Enabled bool `yaml:"enabled"`
	// MaxSize borne la taille d'UNE pièce jointe reçue.
	MaxSize ByteSize `yaml:"max_size"`
	// MaxCount borne le nombre de pièces jointes retenues par message.
	MaxCount int `yaml:"max_count"`
	// AcceptedTypes énumère les types MIME transmis au modèle. Ce filtre
	// protège le tour entier : un fournisseur refuse la requête complète
	// lorsqu'une pièce jointe ne lui convient pas, laissant l'utilisateur
	// sans réponse. Il doit donc rester aligné sur ce que le modèle visé
	// accepte réellement.
	AcceptedTypes []string `yaml:"accepted_types"`
	// MaxHistory borne le nombre de pièces jointes rejouées depuis
	// l'historique à chaque tour, les plus récentes d'abord. Sans cette
	// borne, une conversation riche en images ferait croître indéfiniment la
	// taille (et le coût) de chaque requête.
	MaxHistory int `yaml:"max_history"`
	// MaxReply borne le nombre de pièces jointes renvoyées à l'utilisateur
	// en une réponse, pour qu'un outil prolixe ne l'inonde pas.
	MaxReply int `yaml:"max_reply"`
}

// LLMClient décrit un client LLM configuré.
type LLMClient struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
	APIKey   string `yaml:"api_key"`
	BaseURL  string `yaml:"base_url"`
}

// AgentType énumère les types d'agents supportés.
type AgentType string

const (
	AgentTypeOrchestrator AgentType = "orchestrator"
	AgentTypeSpecialist   AgentType = "specialist"
)

// Agent décrit un agent LLM (orchestrateur ou spécialiste).
type Agent struct {
	Type         AgentType    `yaml:"type"`
	Client       string       `yaml:"client"`
	SystemPrompt SystemPrompt `yaml:"system_prompt"`
	Delegates    []string     `yaml:"delegates"`
	Memory       AgentMemory  `yaml:"memory"`
	MCPServers   []string     `yaml:"mcp_servers"`
	Capabilities []string     `yaml:"capabilities"`
	Limits       AgentLimits  `yaml:"limits"`
}

// SystemPrompt décrit la source du system prompt d'un agent : soit un
// fichier, soit un contenu direct. Le contenu chargé est disponible via
// Content après Load.
type SystemPrompt struct {
	File    string `yaml:"file"`
	Inline  string `yaml:"inline"`
	Content string `yaml:"-"`
}

// AgentMemory décrit les capacités mémoire accordées à un agent.
type AgentMemory struct {
	Search   bool `yaml:"search"`
	Remember bool `yaml:"remember"`
	Forget   bool `yaml:"forget"`
}

// AgentLimits décrit les limites d'exécution d'un agent.
type AgentLimits struct {
	MaxSequentialToolCalls int      `yaml:"max_sequential_tool_calls"`
	MaxActionsPerTurn      int      `yaml:"max_actions_per_turn"`
	ToolTimeout            Duration `yaml:"tool_timeout"`
	MaxToolResultBytes     ByteSize `yaml:"max_tool_result_bytes"`
	MaxToolContextBytes    ByteSize `yaml:"max_tool_context_bytes"`
}

// MCPServer décrit un serveur MCP accessible aux agents.
type MCPServer struct {
	Transport string            `yaml:"transport"`
	URL       string            `yaml:"url"`
	Headers   map[string]string `yaml:"headers"`
}

// Memory décrit la configuration du système de mémoire (Amoxtli).
type Memory struct {
	Store    MemoryStore    `yaml:"store"`
	Indexes  []MemoryIndex  `yaml:"indexes"`
	Policies MemoryPolicies `yaml:"policies"`
}

// MemoryStore décrit le stockage principal de la mémoire.
type MemoryStore struct {
	Driver string `yaml:"driver"`
	Path   string `yaml:"path"`
}

// MemoryIndex décrit un index de recherche mémoire.
type MemoryIndex struct {
	ID     string  `yaml:"id"`
	Type   string  `yaml:"type"`
	Path   string  `yaml:"path"`
	Weight float64 `yaml:"weight"`
}

// MemoryPolicies décrit les règles de propagation entre portées mémoire.
type MemoryPolicies struct {
	PrivateCanWriteOrg    bool `yaml:"private_can_write_org"`
	OrgReadableByChildren bool `yaml:"org_readable_by_children"`
}

// Identities décrit les rôles et principaux connus de l'instance.
type Identities struct {
	Roles      map[string]Role `yaml:"roles"`
	Principals []Principal     `yaml:"principals"`
}

// Role décrit un rôle et ses permissions.
type Role struct {
	Permissions []string `yaml:"permissions"`
}

// PrincipalKind énumère les types de principaux supportés.
type PrincipalKind string

const (
	PrincipalKindHuman   PrincipalKind = "human"
	PrincipalKindService PrincipalKind = "service"
)

// Principal décrit un principal (humain ou service) connu de l'instance.
type Principal struct {
	ID          string        `yaml:"id"`
	Kind        PrincipalKind `yaml:"kind"`
	DisplayName string        `yaml:"display_name"`
	Roles       []string      `yaml:"roles"`
}

// Origin associe une identité externe (fournisseur + identifiant externe) à
// un principal connu.
type Origin struct {
	Provider       string `yaml:"provider"`
	ExternalUserID string `yaml:"external_user_id"`
	PrincipalID    string `yaml:"principal_id"`
}

// ChannelKind énumère les types de canaux supportés.
type ChannelKind string

const (
	ChannelKindPrivate ChannelKind = "private"
	ChannelKindGroup   ChannelKind = "group"
)

// Scope énumère les portées supportées.
type Scope string

const (
	ScopePersonal Scope = "personal"
	ScopeGroup    Scope = "group"
	ScopeOrg      Scope = "org"
)

// Channel décrit un canal de conversation (privé ou groupe).
type Channel struct {
	Provider    string            `yaml:"provider"`
	ChannelID   string            `yaml:"channel_id"`
	DisplayName string            `yaml:"display_name"`
	Kind        ChannelKind       `yaml:"kind"`
	OrgID       string            `yaml:"org_id"`
	Scope       Scope             `yaml:"scope"`
	ScopeID     string            `yaml:"scope_id"`
	Activation  string            `yaml:"activation"`
	Members     []string          `yaml:"members"`
	PrincipalID string            `yaml:"principal_id"`
	Resources   map[string]string `yaml:"resources"`
}

// Schedule décrit un déclenchement planifié.
type Schedule struct {
	ID          string              `yaml:"id"`
	Enabled     bool                `yaml:"enabled"`
	Schedule    ScheduleCron        `yaml:"schedule"`
	Execution   ScheduleExecution   `yaml:"execution"`
	Delivery    ScheduleDelivery    `yaml:"delivery"`
	Concurrency ScheduleConcurrency `yaml:"concurrency"`
}

// ScheduleCron décrit l'expression cron et le fuseau horaire d'un
// déclenchement planifié.
type ScheduleCron struct {
	Cron     string `yaml:"cron"`
	Timezone string `yaml:"timezone"`
}

// ActionsPolicy énumère les politiques d'exécution des actions supportées.
type ActionsPolicy string

const (
	ActionsPolicyReadOnly            ActionsPolicy = "read_only"
	ActionsPolicyRequireConfirmation ActionsPolicy = "require_confirmation"
)

// ScheduleActions décrit la politique d'actions d'un déclenchement planifié.
type ScheduleActions struct {
	Policy ActionsPolicy `yaml:"policy"`
}

// ScheduleExecution décrit le contexte d'exécution d'un déclenchement
// planifié.
type ScheduleExecution struct {
	PrincipalID string          `yaml:"principal_id"`
	OrgID       string          `yaml:"org_id"`
	Scope       Scope           `yaml:"scope"`
	ScopeID     string          `yaml:"scope_id"`
	Agent       string          `yaml:"agent"`
	Prompt      string          `yaml:"prompt"`
	Actions     ScheduleActions `yaml:"actions"`
}

// DeliveryMode énumère les modes de livraison supportés.
type DeliveryMode string

const (
	DeliveryModeAlways    DeliveryMode = "always"
	DeliveryModeOnContent DeliveryMode = "on_content"
	DeliveryModeOnFailure DeliveryMode = "on_failure"
)

// ScheduleDelivery décrit la livraison du résultat d'un déclenchement
// planifié.
type ScheduleDelivery struct {
	Provider  string       `yaml:"provider"`
	ChannelID string       `yaml:"channel_id"`
	Mode      DeliveryMode `yaml:"mode"`
}

// ConcurrencyPolicy énumère les politiques de concurrence supportées.
type ConcurrencyPolicy string

const (
	ConcurrencyPolicyForbid ConcurrencyPolicy = "forbid"
	ConcurrencyPolicyAllow  ConcurrencyPolicy = "allow"
)

// ScheduleConcurrency décrit la politique de concurrence d'un déclenchement
// planifié.
type ScheduleConcurrency struct {
	Policy  ConcurrencyPolicy `yaml:"policy"`
	Timeout Duration          `yaml:"timeout"`
}
