// Package agent définit l'interface applicative Agent (PLAN.md §4.3, §6.1)
// et une implémentation minimale adossée à GenAI : un agent généraliste
// conversationnel, sans délégation, sans outils, sans mémoire (ces capacités
// arrivent aux phases suivantes du plan).
package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/model"
)

// Message est un tour de conversation applicatif, indépendant du format
// GenAI. Role vaut "user" ou "assistant".
type Message struct {
	Role    string
	Content string
}

// Request décrit une exécution demandée à un Agent.
type Request struct {
	Identity     model.ExecutionIdentity
	Conversation model.Conversation
	// History contient les tours précédents, dans l'ordre chronologique.
	// Il ne doit pas inclure Input.
	History []Message
	// Input est le message courant de l'utilisateur.
	Input string
}

// Result est le résultat d'une exécution d'Agent.
type Result struct {
	Reply string
	// References liste les URLs extraites des résultats d'outils exécutés
	// durant ce tour (Phase 12, MCPToolAgent). Vide pour GenAIAgent et
	// OrchestratorAgent, qui ne produisent aucune référence.
	References []string
	// ProposedActions liste les actions sensibles proposées durant ce tour
	// (PLAN.md §10, Phase 15), collectées par OrchestratorAgent depuis les
	// outils qui en produisent (voir MemoryTools.newForgetMemoryTool).
	// internal/conversation.Handler les transforme en persistence.ActionPlan
	// via internal/action.Engine.CreatePlan. Vide pour GenAIAgent et
	// MCPToolAgent, qui n'en produisent aucune.
	ProposedActions []delegation.ProposedAction
}

// Agent exécute une conversation applicative et produit une réponse.
type Agent interface {
	Execute(ctx context.Context, req Request) (Result, error)
}

// ErrEmptyReply est retournée par GenAIAgent lorsque le modèle produit une
// réponse vide (aucun delta de contenu sur tout le flux). Une réponse vide
// n'est jamais silencieusement acceptée : le pipeline ingress ne doit pas
// envoyer de message vide à l'utilisateur sans savoir explicitement
// pourquoi, donc l'agent remonte une erreur plutôt qu'un Result{Reply: ""}
// ambigu (qui pourrait aussi bien signifier "pas de réponse à envoyer").
var ErrEmptyReply = errors.New("agent: réponse du modèle vide")

// GenAIAgent est un agent généraliste minimal : une seule complétion en
// streaming auprès d'un client GenAI, avec un system prompt fixe et
// l'historique fourni par l'appelant. Aucun outil, aucune délégation,
// aucune mémoire : voir PLAN.md Phase 6.
//
// Le choix du streaming plutôt que d'une complétion simple ou d'une boucle
// agent/loop complète est délibéré : la Phase 6 demande explicitement
// "implémenter le streaming" et "garantir la consommation complète des
// canaux" (PLAN.md, AGENTS.md). agent/loop.Handler apporte du tool-calling,
// une gestion de budget et un modèle événementiel bien plus riches que ce
// dont un agent sans outil a besoin ici ; ce ne serait qu'une abstraction
// spéculative pour cette phase. Il sera introduit aux phases où le
// tool-calling (délégation, mémoire, MCP) devient nécessaire.
type GenAIAgent struct {
	client         llm.ChatCompletionStreamingClient
	systemPrompt   string
	agentName      string
	orgDisplayName string
}

// NewGenAIAgent construit un GenAIAgent utilisant client pour les
// complétions en streaming. systemPrompt est le prompt statique déjà
// composé de l'agent (typiquement via BuildSystemPrompt) : il ne contient
// ni le contexte d'exécution ni la demande. agentName et orgDisplayName
// sont les valeurs statiques (connues à la construction, pas par requête)
// utilisées pour peupler le bloc de contexte injecté à chaque exécution
// (voir BuildContextBlock, PLAN.md §7.3).
func NewGenAIAgent(client llm.ChatCompletionStreamingClient, systemPrompt string, agentName string, orgDisplayName string) *GenAIAgent {
	return &GenAIAgent{
		client:         client,
		systemPrompt:   systemPrompt,
		agentName:      agentName,
		orgDisplayName: orgDisplayName,
	}
}

// Execute implémente Agent. Le contexte ctx gouverne la durée de l'appel :
// aucun timeout n'est ajouté ici (PLAN.md Phase 6 ne définit pas de champ de
// configuration dédié ; AgentLimits.ToolTimeout concerne les appels d'outils,
// absents de cette phase). L'appelant est responsable d'attacher un
// context.WithTimeout si nécessaire.
func (a *GenAIAgent) Execute(ctx context.Context, req Request) (Result, error) {
	messages := a.buildMessages(req)

	chunks, err := a.client.ChatCompletionStream(ctx, llm.WithMessages(messages...))
	if err != nil {
		return Result{}, fmt.Errorf("agent: appel du client LLM: %w", err)
	}

	var reply strings.Builder
	var streamErr error

	// La consommation du canal jusqu'à sa fermeture est systématique, même
	// en cas d'erreur de flux ou d'annulation du contexte : c'est
	// l'implémentation GenAI qui a la responsabilité de fermer le canal
	// (notamment lorsque ctx est annulé), notre boucle ne fait que la
	// suivre jusqu'au bout pour ne jamais laisser de goroutine fuiter
	// (AGENTS.md).
	for chunk := range chunks {
		switch chunk.Type() {
		case llm.StreamChunkTypeDelta:
			if delta := chunk.Delta(); delta != nil {
				reply.WriteString(delta.Content())
			}
		case llm.StreamChunkTypeError:
			if streamErr == nil {
				streamErr = chunk.Error()
			}
		}
	}

	// L'annulation du contexte prime sur tout : si ctx a été annulé, on ne
	// prétend pas avoir une réponse exploitable même si des deltas ont déjà
	// été reçus.
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	if streamErr != nil {
		return Result{}, fmt.Errorf("agent: erreur du flux du client LLM: %w", streamErr)
	}

	text := reply.String()
	if text == "" {
		return Result{}, ErrEmptyReply
	}

	return Result{Reply: text}, nil
}

// buildMessages transforme req en messages GenAI : system prompt en
// premier, puis l'historique dans l'ordre chronologique, puis le message
// utilisateur courant. Le message système envoyé au modèle est le prompt
// statique de l'agent suivi du bloc de contexte d'exécution propre à cette
// requête (PLAN.md §7.2, §7.3) : le contexte n'est jamais mélangé au prompt
// statique construit une fois pour toutes à l'enregistrement de l'agent.
func (a *GenAIAgent) buildMessages(req Request) []llm.Message {
	return buildChatMessages(a.systemPrompt, a.agentName, a.orgDisplayName, req)
}

// buildChatMessages transforme req en messages GenAI, partagé par toutes les
// implémentations d'Agent adossées à GenAI (GenAIAgent, OrchestratorAgent) :
// system prompt statique suivi du bloc de contexte d'exécution en premier
// message, puis l'historique dans l'ordre chronologique, puis le message
// utilisateur courant (PLAN.md §7.2, §7.3).
func buildChatMessages(systemPrompt, agentName, orgDisplayName string, req Request) []llm.Message {
	messages := make([]llm.Message, 0, len(req.History)+2)

	systemMessage := systemPrompt + "\n\n---\n\n" + BuildContextBlock(req.Identity, orgDisplayName, agentName)
	messages = append(messages, llm.NewMessage(llm.RoleSystem, systemMessage))

	for _, m := range req.History {
		messages = append(messages, llm.NewMessage(genaiRole(m.Role), m.Content))
	}

	messages = append(messages, llm.NewMessage(llm.RoleUser, req.Input))

	return messages
}

// genaiRole traduit un rôle applicatif ("user"/"assistant") vers le type
// llm.Role attendu par GenAI. Tout rôle inconnu est traité comme "user" par
// prudence plutôt que de provoquer une panique.
func genaiRole(role string) llm.Role {
	if role == "assistant" {
		return llm.RoleAssistant
	}
	return llm.RoleUser
}

var _ Agent = &GenAIAgent{}
