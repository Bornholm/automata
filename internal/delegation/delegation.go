// Package delegation définit le contrat de délégation vers un sous-agent
// spécialiste (PLAN.md §6.3, Phase 8) : ce que l'orchestrateur ("main")
// transmet à un spécialiste (agenda, research, todo...), et ce qu'il reçoit
// en retour.
//
// Ce package ne connaît ni le LLM, ni l'orchestration par tool-calling : il
// ne fait que définir des types de transport et l'interface Specialist.
// L'adaptateur qui relie un internal/agent.Agent existant à un Specialist
// (internal/agent.AgentSpecialist) vit dans le package agent, pas ici, pour
// éviter un cycle d'import (agent a besoin de délégation pour exposer les
// sous-agents comme outils du modèle ; delegation ne doit donc jamais
// importer agent).
package delegation

import (
	"context"

	"github.com/bornholm/automata/internal/media"
	"github.com/bornholm/automata/internal/model"
)

// Request décrit une délégation demandée par un orchestrateur à un
// spécialiste. Elle est volontairement pauvre : PLAN.md §6.3 exige que le
// sous-agent "ne reçoive pas automatiquement l'intégralité de l'historique"
// de la conversation principale — seuls Goal, RelevantInput et Constraints,
// explicitement construits par l'orchestrateur (typiquement à partir
// d'arguments produits par le modèle), sont transmis.
type Request struct {
	// AgentID identifie le spécialiste ciblé (nom déclaré dans
	// config.Config.Agents).
	AgentID string
	// Goal est l'objectif de la délégation, en langage naturel.
	Goal string
	// RelevantInput contient les éléments de contexte explicitement
	// sélectionnés comme nécessaires à l'exécution, jamais l'historique
	// complet de la conversation principale.
	RelevantInput string
	// Constraints liste des contraintes additionnelles à respecter.
	Constraints []string
	// Identity est l'identité d'exécution résolue par l'application (portée,
	// principal, canal) : jamais décidée par le modèle.
	Identity model.ExecutionIdentity
	// Attachments porte les pièces jointes du message courant.
	//
	// Elles accompagnent TOUTE délégation, contrairement au reste du
	// contexte que l'orchestrateur choisit de transmettre (Goal,
	// RelevantInput) : un modèle ne peut pas recopier une image dans une
	// chaîne de caractères, donc s'il ne la transmettait pas lui-même, le
	// spécialiste ne pourrait jamais la voir. Sans cela, « regarde cette
	// affiche et ajoute l'événement à l'agenda » serait irréalisable.
	//
	// Cela ne contredit pas PLAN.md §6.3 (« le sous-agent ne reçoit pas
	// automatiquement l'intégralité de l'historique ») : seules les pièces
	// jointes du tour courant sont transmises, jamais celles des tours
	// précédents.
	Attachments []media.Media
	// RecentAttachments porte les fichiers reçus lors des messages
	// précédents de la conversation, du plus récent au plus ancien —
	// réservés aux outils ou non : une photo ordinaire est un fichier comme
	// un autre pour un spécialiste qui sait l'éditer.
	//
	// Ils ne sont transmis qu'aux spécialistes qui déclarent savoir
	// manipuler des fichiers (FileCapable), et restent invisibles du
	// modèle : ces spécialistes ne reçoivent aucune pièce jointe dans leur
	// contexte, seulement des noms, et vont chercher les octets avec leurs
	// outils. C'est ce qui permet à un membre d'envoyer une vidéo ou une
	// photo, puis d'en demander la transformation au message suivant.
	//
	// La nuance avec PLAN.md §6.3 est nette : le spécialiste ne reçoit
	// toujours aucun contenu de l'historique — ni texte, ni image lisible
	// par le modèle — mais il peut désigner par son nom un fichier déjà
	// reçu, comme l'utilisateur le ferait.
	RecentAttachments []media.Media
}

// FileCapable est implémentée par les spécialistes qui savent manipuler
// des fichiers. L'orchestrateur s'en sert pour décider à qui transmettre
// RecentAttachments : il interroge une capacité déclarée, il ne connaît
// aucun spécialiste par son nom.
type FileCapable interface {
	// SupportsFiles indique que le spécialiste dispose d'outils capables
	// d'importer et de produire des fichiers.
	SupportsFiles() bool
}

// ProposedAction décrit une action que le spécialiste propose mais n'exécute
// pas lui-même (PLAN.md §6.4). Depuis la Phase 15, elle porte tout ce qui
// est nécessaire pour qu'internal/action.Engine puisse persister un
// persistence.Action exécutable et le rejouer plus tard, au moment d'une
// confirmation :
//   - MCPServer/ToolName identifient l'outil à ré-exécuter. La convention
//     "internal" pour MCPServer désigne une action qui n'est pas exécutée
//     via un serveur MCP mais par un exécuteur applicatif interne enregistré
//     auprès de l'Engine (voir internal/action, ex : "memory.forget") ;
//   - Arguments sont les arguments à passer à l'outil, déjà entièrement
//     déterminés par l'application (jamais un identifiant de ressource
//     externe fourni par le modèle, AGENTS.md) ;
//   - RequiredPermission/Scope/ScopeID décrivent la permission à
//     revérifier et la portée cible au moment de la confirmation (PLAN.md
//     §10.5, "recalculer les permissions"), jamais réutilisées telles que
//     vérifiées au moment de la proposition.
type ProposedAction struct {
	// Summary est la description humaine de l'action, affichée dans la
	// liste numérotée du plan (PLAN.md §8.5, généralisé à toute action).
	Summary string
	// AgentID identifie le spécialiste ou l'agent à l'origine de la
	// proposition (informatif, persisté sur l'action).
	AgentID string
	// MCPServer est le nom du serveur MCP à utiliser pour ré-exécuter
	// l'action, ou "internal" pour un exécuteur applicatif interne.
	MCPServer string
	// ToolName est le nom de l'outil à appeler sur MCPServer.
	ToolName string
	// Arguments sont les arguments de l'appel d'outil, déjà résolus par
	// l'application (hors ressources à résoudre à nouveau au moment de la
	// confirmation, voir PLAN.md §10.5 point 6).
	Arguments map[string]any
	// RequiredPermission est la permission ("<domaine>.<scope>.<action>",
	// voir internal/authorization) requise pour exécuter cette action.
	RequiredPermission string
	// Scope/ScopeID sont la portée cible de l'action, revérifiée au moment
	// de la confirmation.
	Scope   model.Scope
	ScopeID model.ScopeID
}

// Result est la réponse d'un spécialiste à une délégation.
type Result struct {
	// Summary est le résumé, en langage naturel, à agréger dans la réponse
	// de l'orchestrateur.
	Summary string
	// ProposedActions liste les actions proposées par le spécialiste,
	// nécessitant une confirmation avant exécution (PLAN.md §6.4, §10).
	// Collectées par l'orchestrateur (internal/agent.OrchestratorAgent) et
	// transformées en persistence.ActionPlan par internal/action.Engine.
	ProposedActions []ProposedAction
	// References liste les sources citées par le spécialiste. Type minimal
	// (titres) pour cette phase ; sera enrichi (Phase 10+) quand la mémoire
	// et les MCP réels produiront des références structurées.
	References []string
	// Attachments porte les médias produits par le spécialiste durant la
	// délégation (typiquement le résultat d'un outil MCP : graphique,
	// capture, document). L'orchestrateur les agrège pour les joindre à la
	// réponse envoyée à l'utilisateur.
	Attachments []media.Media
}

// CapabilityReport décrit ce qu'un spécialiste sait réellement faire à
// l'instant de l'appel : ses outils effectivement joignables, ou la raison
// courte de son indisponibilité. Detail est destiné au modèle et s'écrit en
// anglais, sans détail technique sensible (AGENTS.md).
type CapabilityReport struct {
	// Available vaut false quand le spécialiste ne peut pas travailler en
	// ce moment (serveur MCP injoignable, par exemple).
	Available bool
	// Tools liste les noms des outils actuellement disponibles.
	Tools []string
	// Detail complète le rapport : raison d'une indisponibilité, ou nature
	// des capacités quand elles ne s'expriment pas en noms d'outils.
	Detail string
}

// CapabilityReporter est implémenté par les spécialistes capables de
// rapporter leurs capacités du moment. Interface optionnelle : un
// spécialiste qui ne l'implémente pas est simplement décrit par sa
// description statique de configuration.
type CapabilityReporter interface {
	ReportCapabilities(ctx context.Context, identity model.ExecutionIdentity) CapabilityReport
}

// Specialist exécute une délégation. Une implémentation typique adapte un
// internal/agent.Agent existant (voir internal/agent.AgentSpecialist) mais
// rien n'impose que ce soit le cas : un spécialiste factice de test peut
// implémenter Specialist directement.
type Specialist interface {
	Execute(ctx context.Context, req Request) (Result, error)
}
