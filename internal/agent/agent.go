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
	"time"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/media"
	"github.com/bornholm/automata/internal/model"
)

// Message est un tour de conversation applicatif, indépendant du format
// GenAI. Role vaut "user" ou "assistant".
type Message struct {
	Role    string
	Content string
	// Attachments porte les pièces jointes de ce tour, rejouées depuis la
	// persistance. Toujours vide pour un tour "assistant" : les fournisseurs
	// refusent les pièces jointes sur un message assistant ou system, seuls
	// les messages "user" et les résultats d'outils peuvent en porter.
	Attachments []media.Media
}

// Request décrit une exécution demandée à un Agent.
type Request struct {
	Identity     model.ExecutionIdentity
	Conversation model.Conversation
	// History contient les tours précédents, dans l'ordre chronologique.
	// Il ne doit pas inclure Input.
	History []Message
	// Summary est le résumé roulant des messages plus anciens que History
	// (compaction, internal/conversation.Compactor). Vide si la compaction
	// est désactivée ou n'a encore rien condensé. Injecté dans le message
	// system, jamais dans l'historique : ce n'est pas un tour de
	// conversation.
	Summary string
	// Input est le message courant de l'utilisateur.
	Input string
	// Attachments porte les pièces jointes du message courant (images,
	// documents), déjà filtrées et bornées par internal/media selon la
	// configuration. Les notes vocales n'y figurent jamais : elles sont
	// transcrites vers Input.
	Attachments []media.Media
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
	// via internal/action.Engine.CreatePlan. Vide pour GenAIAgent.
	ProposedActions []delegation.ProposedAction
	// Attachments porte les médias produits durant le tour (résultats
	// d'outils MCP : graphique, capture, document), à joindre à la réponse
	// envoyée à l'utilisateur.
	Attachments []media.Media
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
	client       llm.ChatCompletionStreamingClient
	systemPrompt string
	orgPrompts   map[string]string
	agentName    string
	// textOnly indique que le modèle refuse les images en entrée
	// (llm_clients.<nom>.vision: false) : buildChatMessages n'envoie alors
	// aucune pièce jointe et signale leur présence en texte.
	textOnly bool
}

// NewGenAIAgent construit un GenAIAgent utilisant client pour les
// complétions en streaming. systemPrompt est le prompt statique déjà
// composé de l'agent (typiquement via BuildSystemPrompt) : il ne contient
// ni le contexte d'exécution ni la demande. agentName est la seule valeur
// statique du bloc de contexte injecté à chaque exécution : le nom de
// l'organisation, lui, dépend de la requête et voyage dans l'identité
// résolue (voir BuildContextBlock, PLAN.md §7.3).
func NewGenAIAgent(client llm.ChatCompletionStreamingClient, systemPrompt string, agentName string) *GenAIAgent {
	return &GenAIAgent{
		client:       client,
		systemPrompt: systemPrompt,
		agentName:    agentName,
	}
}

// Execute implémente Agent. Le contexte ctx gouverne la durée de l'appel :
// aucun timeout n'est ajouté ici (PLAN.md Phase 6 ne définit pas de champ de
// configuration dédié ; AgentLimits.ToolTimeout concerne les appels d'outils,
// absents de cette phase). L'appelant est responsable d'attacher un
// context.WithTimeout si nécessaire (voir internal/ingress.handleTimeout,
// qui borne ctx pour tout appel passant par le pipeline d'ingress — PLAN.md
// Phase 19, point 8).
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

	text := cleanReply(reply.String())
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
	return buildChatMessages(resolveSystemPrompt(a.systemPrompt, a.orgPrompts, req.Identity.OrgID), a.agentName, a.textOnly, "", req)
}

// WithVision déclare si le modèle du client accepte les images en entrée.
// À false, aucune pièce jointe ne part vers le modèle. Retourne a pour
// permettre le chaînage.
func (a *GenAIAgent) WithVision(enabled bool) *GenAIAgent {
	a.textOnly = !enabled
	return a
}

// WithOrgSystemPrompts remplace le prompt système par organisation : la clé
// est un organizations[].id, la valeur un prompt complet déjà composé (voir
// BuildOrgSystemPrompts). Le prompt du constructeur reste le défaut pour
// toute organisation absente de la map. Retourne a pour permettre le
// chaînage.
func (a *GenAIAgent) WithOrgSystemPrompts(prompts map[string]string) *GenAIAgent {
	a.orgPrompts = prompts
	return a
}

// buildChatMessages transforme req en messages GenAI, partagé par toutes les
// implémentations d'Agent adossées à GenAI (GenAIAgent, OrchestratorAgent) :
// system prompt statique suivi du bloc de contexte d'exécution en premier
// message, puis l'historique dans l'ordre chronologique, puis le message
// utilisateur courant (PLAN.md §7.2, §7.3).
// Les pièces jointes sont portées par les seuls messages "user" : les
// fournisseurs refusent la requête entière si un message system ou assistant
// en contient (voir la validation par provider dans genai).
// textOnly retire toute pièce jointe des messages : un fournisseur
// texte-seul rejette la requête entière dès qu'un message en contient une.
// Celles du tour courant sont signalées en texte pour que le modèle sache
// qu'elles existent — et qu'il délègue à un spécialiste qui les voit au
// lieu d'en deviner le contenu.
func buildChatMessages(systemPrompt, agentName string, textOnly bool, recallNote string, req Request) []llm.Message {
	messages := make([]llm.Message, 0, len(req.History)+2)

	systemMessage := systemPrompt + "\n\n---\n\n" + BuildContextBlock(req.Identity, req.Conversation.DisplayName, agentName, time.Now())

	if req.Summary != "" {
		systemMessage += "\n\n---\n\n## Résumé de la conversation antérieure\n\n" +
			"Les messages ci-dessous ne remontent pas au début de la conversation : " +
			"ce résumé, généré automatiquement, condense les échanges plus anciens.\n\n" +
			req.Summary
	}

	// Rappel automatique (memory.recall) : souvenirs jugés pertinents pour
	// le message entrant, récupérés par l'orchestrateur AVANT l'appel —
	// jamais construit ici, buildChatMessages ne décide d'aucun accès
	// mémoire.
	if recallNote != "" {
		systemMessage += "\n\n---\n\n" + recallNote
	}

	messages = append(messages, llm.NewMessage(llm.RoleSystem, systemMessage))

	for _, m := range req.History {
		role := genaiRole(m.Role)

		if textOnly {
			messages = append(messages, llm.NewMessage(role, m.Content))
			continue
		}

		attachments, _ := media.ToLLMAll(m.Attachments)
		if role != llm.RoleUser || len(attachments) == 0 {
			messages = append(messages, llm.NewMessage(role, m.Content))
			continue
		}

		messages = append(messages, llm.NewMessageWithAttachments(role, m.Content, attachments...))
	}

	if textOnly {
		input := req.Input
		if len(req.Attachments) > 0 {
			input += attachmentNotice(req.Attachments)
		}
		messages = append(messages, llm.NewMessage(llm.RoleUser, input))
		return messages
	}

	attachments, _ := media.ToLLMAll(req.Attachments)
	if len(attachments) == 0 {
		messages = append(messages, llm.NewMessage(llm.RoleUser, req.Input))
	} else {
		messages = append(messages, llm.NewMessageWithAttachments(llm.RoleUser, req.Input, attachments...))
	}

	return messages
}

// attachmentNotice décrit en texte les pièces jointes qu'un modèle
// texte-seul ne recevra pas : types MIME uniquement, jamais le contenu.
// En anglais : ce texte part vers le modèle (AGENTS.md).
func attachmentNotice(attachments []media.Media) string {
	types := make([]string, 0, len(attachments))
	for _, m := range attachments {
		types = append(types, m.MimeType)
	}

	return fmt.Sprintf(
		"\n\n[%d attachment(s) received (%s). You cannot view them yourself: delegate to a specialist that can see images, passing along the user's question. Never guess their content.]",
		len(attachments), strings.Join(types, ", "),
	)
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
