// Package ingress consomme les messages entrants d'un fournisseur Go
// Courier, résout leur identité applicative, applique la règle de mention
// des groupes (PLAN.md §3.3), déduplique les messages déjà traités et
// délègue le traitement métier à un Handler.
//
// Aucune logique LLM ne vit ici : ce package est un transport. La Phase 6
// branchera un Handler qui appelle réellement l'agent généraliste.
package ingress

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/bornholm/go-courier"

	"github.com/bornholm/automata/internal/apperr"
	"github.com/bornholm/automata/internal/identity"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
)

// Statuts terminaux enregistrés dans processed_messages.
const (
	statusProcessing = "processing"
	statusProcessed  = "processed"
	statusFailed     = "failed"
)

// Handler traite un message déjà résolu et autorisé, et retourne le contenu
// textuel de la réponse à envoyer. Une réponse vide n'entraîne aucun envoi.
type Handler interface {
	Handle(ctx context.Context, identity model.ExecutionIdentity, conversation model.Conversation, message courier.Message) (string, error)
}

// HandlerFunc adapte une fonction en Handler.
type HandlerFunc func(ctx context.Context, identity model.ExecutionIdentity, conversation model.Conversation, message courier.Message) (string, error)

// Handle implémente Handler.
func (f HandlerFunc) Handle(ctx context.Context, identity model.ExecutionIdentity, conversation model.Conversation, message courier.Message) (string, error) {
	return f(ctx, identity, conversation, message)
}

// FixedReplyHandler est le Handler de la Phase 5 : il répond toujours le
// même message fixe, sans appel LLM. Il sera remplacé par l'agent
// généraliste en Phase 6.
type FixedReplyHandler struct {
	Reply string
}

// Handle implémente Handler.
func (h FixedReplyHandler) Handle(ctx context.Context, identity model.ExecutionIdentity, conversation model.Conversation, message courier.Message) (string, error) {
	return h.Reply, nil
}

var _ Handler = FixedReplyHandler{}

// Pipeline consomme les messages entrants d'un unique courier.Provider
// (identifié par son nom logique, tel que déclaré dans courier.providers de
// la configuration).
type Pipeline struct {
	providerName      string
	provider          courier.Provider
	resolver          *identity.Resolver
	db                *persistence.DB
	processedMessages *persistence.ProcessedMessageRepository
	handler           Handler
	logger            *slog.Logger
}

// NewPipeline construit un Pipeline. handler ne doit jamais être nil ; en
// Phase 5, passer FixedReplyHandler.
func NewPipeline(providerName string, provider courier.Provider, resolver *identity.Resolver, db *persistence.DB, handler Handler, logger *slog.Logger) *Pipeline {
	if logger == nil {
		logger = slog.Default()
	}

	return &Pipeline{
		providerName:      providerName,
		provider:          provider,
		resolver:          resolver,
		db:                db,
		processedMessages: persistence.NewProcessedMessageRepository(),
		handler:           handler,
		logger:            logger,
	}
}

// Run démarre l'écoute du provider et traite les messages entrants jusqu'à
// la fermeture du canal ou l'annulation de ctx. Le provider doit implémenter
// courier.SelfProvider : sans identité "self", la règle de mention des
// groupes (§3.3) ne peut pas être appliquée de façon fiable, aussi Run
// échoue explicitement plutôt que de se comporter silencieusement de façon
// incorrecte.
func (p *Pipeline) Run(ctx context.Context) error {
	selfProvider, ok := p.provider.(courier.SelfProvider)
	if !ok {
		return fmt.Errorf("ingress: le fournisseur %q n'implémente pas courier.SelfProvider, requis pour appliquer la règle de mention des groupes", p.providerName)
	}

	self, err := selfProvider.Self(ctx)
	if err != nil {
		return fmt.Errorf("ingress: résolution de l'identité self du fournisseur %q: %w", p.providerName, err)
	}

	messages, err := p.provider.Listen(ctx)
	if err != nil {
		return fmt.Errorf("ingress: démarrage de l'écoute du fournisseur %q: %w", p.providerName, err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-messages:
			if !ok {
				return nil
			}

			p.processMessage(ctx, self, msg)
		}
	}
}

// processMessage traite un unique message entrant. Toute erreur est
// journalisée et n'interrompt jamais la boucle appelante (voir AGENTS.md :
// le pipeline continue avec le message suivant).
func (p *Pipeline) processMessage(ctx context.Context, self courier.User, msg courier.Message) {
	externalUserID := string(msg.From().ID())
	channelID := string(msg.Channel().ChannelID())
	messageID := string(msg.ID())

	execIdentity, conversation, err := p.resolver.ResolveMessage(ctx, p.providerName, externalUserID, channelID)
	if err != nil {
		if errors.Is(err, apperr.ErrUnknownOrigin) || errors.Is(err, apperr.ErrUnknownChannel) || errors.Is(err, apperr.ErrUnauthorized) {
			p.logger.InfoContext(ctx, "ingress: message ignoré (identité non résolue ou non autorisée)",
				"provider", p.providerName,
				"channel_id", channelID,
			)
			return
		}

		p.logger.ErrorContext(ctx, "ingress: échec de résolution d'identité",
			"provider", p.providerName,
			"channel_id", channelID,
			"error", err,
		)
		return
	}

	logCtx := []any{
		"org_id", execIdentity.OrgID,
		"principal_id", execIdentity.PrincipalID,
		"conversation_id", execIdentity.ConversationID,
		"provider", p.providerName,
		"channel_id", channelID,
	}

	if conversation.Kind == model.ChannelGroup {
		if !courier.IsMentioned(msg, self.ID()) {
			p.logger.InfoContext(ctx, "ingress: message de groupe ignoré (assistant non mentionné)", logCtx...)
			return
		}
	}

	duplicate, err := p.markProcessing(ctx, messageID)
	if err != nil {
		p.logger.ErrorContext(ctx, "ingress: échec de la déduplication du message", append(logCtx, "error", err)...)
		return
	}

	if duplicate {
		p.logger.InfoContext(ctx, "ingress: message déjà traité, ignoré", logCtx...)
		return
	}

	reply, err := p.handler.Handle(ctx, execIdentity, conversation, msg)
	if err != nil {
		p.logger.ErrorContext(ctx, "ingress: échec du traitement du message", append(logCtx, "error", err)...)
		p.markFinal(ctx, messageID, statusFailed, logCtx)
		return
	}

	if reply != "" {
		outgoing := courier.NewMessage(
			courier.RandomMessageID(),
			msg.Channel(),
			self,
			courier.WithMessageMainPart(reply),
		)

		if err := p.provider.Send(ctx, outgoing); err != nil {
			p.logger.ErrorContext(ctx, "ingress: échec de l'envoi de la réponse", append(logCtx, "error", err)...)
			p.markFinal(ctx, messageID, statusFailed, logCtx)
			return
		}
	}

	p.markFinal(ctx, messageID, statusProcessed, logCtx)
}

// markProcessing enregistre (provider, messageID) comme en cours de
// traitement au sein d'une transaction, ou signale qu'il l'était déjà
// (duplicate == true).
func (p *Pipeline) markProcessing(ctx context.Context, messageID string) (duplicate bool, err error) {
	txErr := p.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, found, err := p.processedMessages.FindByProviderAndExternalMessageID(ctx, tx, p.providerName, messageID)
		if err != nil {
			return err
		}

		if found {
			duplicate = true
			return nil
		}

		return p.processedMessages.Insert(ctx, tx, persistence.ProcessedMessage{
			Provider:          p.providerName,
			ExternalMessageID: messageID,
			ProcessedAt:       time.Now().UTC().Format(time.RFC3339),
			Status:            statusProcessing,
		})
	})
	if txErr != nil {
		return false, txErr
	}

	return duplicate, nil
}

// markFinal enregistre le statut final d'un message déjà marqué comme en
// cours de traitement. Un échec de cette mise à jour est journalisé mais
// n'interrompt pas le pipeline.
func (p *Pipeline) markFinal(ctx context.Context, messageID, status string, logCtx []any) {
	err := p.db.WithTx(ctx, func(tx *sql.Tx) error {
		return p.processedMessages.UpdateStatus(ctx, tx, p.providerName, messageID, status)
	})
	if err != nil {
		p.logger.ErrorContext(ctx, "ingress: échec de l'enregistrement du statut final du message", append(logCtx, "status", status, "error", err)...)
	}
}
