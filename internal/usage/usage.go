// Package usage comptabilise la consommation d'inférence LLM (tokens et
// coût facturé) par organisation et par utilisateur, dans l'optique d'une
// refacturation de l'accès à l'instance.
//
// Le mécanisme repose sur deux canaux orthogonaux, tous deux portés par le
// contexte :
//
//   - le Recorder, attaché UNE fois à la racine du worker
//     (internal/registry.Run) : sans lui, l'enregistrement est un no-op
//     silencieux — les tests et les commandes CLI n'enregistrent rien sans
//     câblage supplémentaire ;
//   - l'Attribution, attachée à chaque point d'entrée d'exécution (tour
//     d'agent, compaction, consolidation, transcription) : elle dit POUR QUI
//     l'appel est fait, ce que le client LLM — construit une fois au
//     démarrage et partagé par toutes les requêtes — ne peut pas savoir.
//
// Le transport par le contexte suit le précédent de mediaCollector
// (internal/agent/toolloop.go) : les clients sont construits une fois et
// partagés, toute donnée propre à une requête doit voyager avec elle.
//
// Un enregistrement ne porte JAMAIS de contenu (ni message, ni prompt, ni
// réponse) : uniquement des identifiants, des comptes de tokens et des
// montants — conforme à AGENTS.md, « ne pas journaliser les contenus
// privés ». Le PrincipalID enregistré reste interne à la base applicative,
// jamais exposé au modèle.
package usage

import (
	"context"
	"time"
)

// Valeurs de Record.Kind : la nature de l'appel facturé, fixée par le
// décorateur de client (client.go), indépendamment du composant appelant.
const (
	KindChat          = "chat"
	KindTranscription = "transcription"
	KindImage         = "image"
)

// Valeurs d'Attribution.Component : le composant applicatif pour le compte
// duquel l'appel est fait.
const (
	ComponentAgent         = "agent"
	ComponentCompaction    = "compaction"
	ComponentConsolidation = "consolidation"
	ComponentReflection    = "reflection"
	ComponentTranscription = "transcription"
)

// Record est la trace comptable d'un appel d'inférence. Les champs
// d'attribution (OrgID, PrincipalID, ConversationID, Component, Agent)
// peuvent être vides si l'appel n'a pas pu être attribué — l'appel est tout
// de même enregistré : pour une comptabilité, un montant orphelin vaut
// mieux qu'un montant manquant.
type Record struct {
	CreatedAt time.Time

	// Attribution : qui a déclenché l'appel. PrincipalID vide = tâche de
	// fond facturée à l'organisation (compaction, consolidation).
	OrgID          string
	PrincipalID    string
	ConversationID string
	Component      string
	Agent          string

	// Nature et cible de l'appel.
	Kind     string
	Provider string
	Model    string

	// Volumes rapportés par le provider. Pour une transcription,
	// PromptTokens porte les tokens d'entrée (audio) et CompletionTokens
	// les tokens de sortie.
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	CachedTokens     int64

	// CostAmount/CostCurrency ne sont renseignés que si CostReported est
	// vrai : le provider a rapporté le montant réellement facturé
	// (OpenRouter le fait, en USD). Sinon, seuls les tokens font foi.
	CostAmount   float64
	CostCurrency string
	CostReported bool
}

// Recorder enregistre les traces comptables. L'enregistrement ne retourne
// pas d'erreur : il ne doit JAMAIS faire échouer le tour qui l'a produit —
// l'implémentation journalise elle-même ses échecs.
type Recorder interface {
	RecordUsage(ctx context.Context, rec Record)
}

// Attribution identifie le déclencheur d'un appel d'inférence. Elle est
// attachée au contexte à chaque point d'entrée d'exécution et lue par le
// décorateur de client au moment de l'appel.
type Attribution struct {
	OrgID          string
	PrincipalID    string
	ConversationID string
	Component      string
	// Agent est le nom de l'agent au sens de la configuration (main,
	// research...). Chaque Agent.Execute attache sa propre attribution :
	// un appel délégué est ainsi attribué au spécialiste qui l'a fait,
	// pas à l'orchestrateur qui a délégué.
	Agent string
}

type recorderContextKey struct{}
type attributionContextKey struct{}

// ContextWithRecorder attache recorder à ctx. Attaché une fois à la racine
// du worker, il voyage ensuite dans tous les contextes dérivés (pipelines
// ingress, scheduler, dispatcher de rappels, consolidateur).
func ContextWithRecorder(ctx context.Context, recorder Recorder) context.Context {
	return context.WithValue(ctx, recorderContextKey{}, recorder)
}

// RecorderFromContext extrait le Recorder de ctx, s'il y en a un.
func RecorderFromContext(ctx context.Context) (Recorder, bool) {
	recorder, ok := ctx.Value(recorderContextKey{}).(Recorder)
	return recorder, ok && recorder != nil
}

// ContextWithAttribution attache attr à ctx, remplaçant toute attribution
// déjà présente (un spécialiste délégué remplace celle de l'orchestrateur).
func ContextWithAttribution(ctx context.Context, attr Attribution) context.Context {
	return context.WithValue(ctx, attributionContextKey{}, attr)
}

// AttributionFromContext extrait l'Attribution de ctx. La valeur zéro est
// retournée si aucune attribution n'est attachée : l'appel sera enregistré
// orphelin, pas ignoré.
func AttributionFromContext(ctx context.Context) (Attribution, bool) {
	attr, ok := ctx.Value(attributionContextKey{}).(Attribution)
	return attr, ok
}
