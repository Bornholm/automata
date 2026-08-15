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
}

// ProposedAction décrit une action que le spécialiste propose mais n'exécute
// pas lui-même (PLAN.md §6.4). Type minimal pour cette phase : aucune action
// réelle n'est encore produite tant que le tool-calling MCP n'existe pas
// (Phase 10+) ; seul le champ nécessaire pour compiler et pour restituer une
// intention lisible est conservé ici.
type ProposedAction struct {
	Summary string
}

// Result est la réponse d'un spécialiste à une délégation.
type Result struct {
	// Summary est le résumé, en langage naturel, à agréger dans la réponse
	// de l'orchestrateur.
	Summary string
	// ProposedActions liste les actions proposées par le spécialiste,
	// nécessitant une confirmation avant exécution (PLAN.md §6.4). Vide tant
	// qu'aucun spécialiste réel ne les produit.
	ProposedActions []ProposedAction
	// References liste les sources citées par le spécialiste. Type minimal
	// (titres) pour cette phase ; sera enrichi (Phase 10+) quand la mémoire
	// et les MCP réels produiront des références structurées.
	References []string
}

// Specialist exécute une délégation. Une implémentation typique adapte un
// internal/agent.Agent existant (voir internal/agent.AgentSpecialist) mais
// rien n'impose que ce soit le cas : un spécialiste factice de test peut
// implémenter Specialist directement.
type Specialist interface {
	Execute(ctx context.Context, req Request) (Result, error)
}
