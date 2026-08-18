// Package memory adapte Amoxtli (github.com/bornholm/amoxtli) à la mémoire
// applicative d'Automata (PLAN.md §8, Phase 10) : ajout, recherche et
// suppression d'informations textuelles, cloisonnées par portée
// (personal/group/org).
//
// Ce package NE DÉCIDE JAMAIS de la portée d'une opération : Query et
// NewMemory portent déjà la portée résolue par l'appelant (voir
// internal/agent, qui applique internal/authorization.Authorizer avant tout
// appel ici). Ce package se contente d'appliquer les filtres demandés au
// niveau du store et de l'index amoxtli (§8.3, "les filtres doivent être
// appliqués au niveau du store et des index").
package memory

import (
	"context"
	"time"

	"github.com/bornholm/automata/internal/model"
)

// Memory est une information mémorisée, restituée par Search ou Remember.
type Memory struct {
	ID        string
	Content   string
	Metadata  map[string]string
	CreatedAt time.Time
}

// Query décrit une recherche mémoire déjà cloisonnée : OrgID, Scope et
// ScopeID sont fournis par l'appelant, jamais déduits ici.
type Query struct {
	Text       string
	OrgID      model.OrgID
	Scope      model.Scope
	ScopeID    model.ScopeID
	MaxResults int
}

// NewMemory décrit l'ajout d'une information textuelle. Le LLM ne propose
// que Content ; tous les autres champs sont déterminés par l'application
// (PLAN.md §8.4).
type NewMemory struct {
	Content              string
	Scope                model.Scope
	ScopeID              model.ScopeID
	OrgID                model.OrgID
	OwnerPrincipalID     model.PrincipalID
	CreatedBy            model.PrincipalID
	SourceConversationID model.ConversationID
	// Origin identifie le mécanisme applicatif ayant produit la mémoire :
	// vide pour l'outil conversationnel remember, "compaction" pour
	// l'extraction de faits durables (internal/conversation.Compactor),
	// "consolidation" pour une fusion produite par la réorganisation
	// périodique (internal/consolidation). Toujours décidée par
	// l'application, jamais par le LLM.
	Origin string
}

// Store est l'interface applicative de la mémoire persistante. AmoxtliStore
// (amoxtli_store.go) en est l'implémentation réelle, adossée à un
// *amoxtli.Codex ; internal/agent s'appuie sur cette interface plutôt que
// sur le type concret, pour permettre des doubles de test légers au niveau
// de l'orchestration des outils (la mémoire elle-même, en revanche, est
// toujours testée avec un Amoxtli réel, voir PLAN.md §16).
type Store interface {
	// Remember ajoute une information textuelle et attend la fin de son
	// indexation (asynchrone côté amoxtli) avant de retourner.
	Remember(ctx context.Context, mem NewMemory) (Memory, error)
	// Search retourne les mémoires correspondant à query.Text dans la
	// portée exacte décrite par query.
	Search(ctx context.Context, query Query) ([]Memory, error)
	// GetByID retourne la mémoire identifiée par id, si elle existe dans la
	// portée (orgID, scope, scopeID) donnée. ok vaut false si aucune
	// mémoire de cet identifiant n'existe dans cette portée précise (elle
	// peut très bien exister dans une autre portée : ce n'est pas une
	// erreur, l'appelant doit essayer d'autres portées autorisées si
	// pertinent).
	GetByID(ctx context.Context, orgID model.OrgID, scope model.Scope, scopeID model.ScopeID, id string) (Memory, bool, error)
	// Forget supprime définitivement la mémoire identifiée par id.
	Forget(ctx context.Context, id string) error
	// List retourne TOUTES les mémoires de l'instance, toutes portées
	// confondues. Réservé aux mécanismes de maintenance internes
	// (internal/consolidation) : cette méthode ignore volontairement le
	// cloisonnement par portée et ne doit JAMAIS être exposée, directement
	// ou indirectement, à un agent LLM ou à un utilisateur.
	List(ctx context.Context) ([]Memory, error)
	// Reindex reconstruit l'index de recherche à partir du store (commande
	// CLI "automata memory reindex", PLAN.md §8.6).
	Reindex(ctx context.Context) error
}
