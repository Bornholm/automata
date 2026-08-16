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
	"github.com/bornholm/automata/internal/media"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/observability"
	"github.com/bornholm/automata/internal/persistence"
)

// Statuts terminaux enregistrés dans processed_messages.
const (
	statusProcessing = "processing"
	statusProcessed  = "processed"
	statusFailed     = "failed"
)

// handleTimeout borne la durée totale du traitement métier d'un message
// (Handler.Handle, qui appelle in fine le LLM, les outils MCP et la
// transcription audio). Sans cette limite, le ctx transmis à
// Pipeline.processMessage n'est borné que par l'arrêt du processus (voir
// cmd/automata/main.go, context issu de signal.NotifyContext) : un
// fournisseur LLM ou MCP qui ne répond jamais bloquerait indéfiniment le
// traitement de TOUS les messages suivants du même fournisseur, la boucle
// de Pipeline.Run étant strictement séquentielle (PLAN.md Phase 19, point
// 8 "timeouts réseau"). 5 minutes reste large devant les timeouts plus
// courts déjà en place pour chaque appel individuel (audio.Config.Timeout,
// mcp.Limits.ToolTimeout), pour ne jamais couper un tour légitime
// enchaînant plusieurs appels d'outils.
const handleTimeout = 5 * time.Minute

// sendTimeout borne l'envoi de la réponse au fournisseur courier, un appel
// réseau distinct du traitement métier.
const sendTimeout = 30 * time.Second

// Handler traite un message déjà résolu et autorisé, et retourne le contenu
// de la réponse à envoyer : son texte, et les éventuels médias à y joindre
// (image produite par un outil, document généré).
//
// Une réponse entièrement vide — ni texte, ni média — n'entraîne aucun envoi.
type Handler interface {
	Handle(ctx context.Context, identity model.ExecutionIdentity, conversation model.Conversation, message courier.Message) (string, []media.Media, error)
}

// HandlerFunc adapte une fonction en Handler.
type HandlerFunc func(ctx context.Context, identity model.ExecutionIdentity, conversation model.Conversation, message courier.Message) (string, []media.Media, error)

// Handle implémente Handler.
func (f HandlerFunc) Handle(ctx context.Context, identity model.ExecutionIdentity, conversation model.Conversation, message courier.Message) (string, []media.Media, error) {
	return f(ctx, identity, conversation, message)
}

// FixedReplyHandler est le Handler de la Phase 5 : il répond toujours le
// même message fixe, sans appel LLM. Il sera remplacé par l'agent
// généraliste en Phase 6.
type FixedReplyHandler struct {
	Reply string
}

// Handle implémente Handler.
func (h FixedReplyHandler) Handle(ctx context.Context, identity model.ExecutionIdentity, conversation model.Conversation, message courier.Message) (string, []media.Media, error) {
	return h.Reply, nil, nil
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
	metrics           *observability.Metrics
}

// NewPipeline construit un Pipeline. handler ne doit jamais être nil ; en
// Phase 5, passer FixedReplyHandler. metrics peut être nil (registre de
// métriques désactivé, PLAN.md Phase 20) : toutes ses méthodes sont alors
// no-op.
func NewPipeline(providerName string, provider courier.Provider, resolver *identity.Resolver, db *persistence.DB, handler Handler, logger *slog.Logger, metrics *observability.Metrics) *Pipeline {
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
		metrics:           metrics,
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
	p.metrics.IncMessagesReceived()

	externalUserID := string(msg.From().ID())
	channelID := string(msg.Channel().ChannelID())
	messageID := string(msg.ID())

	execIdentity, conversation, err := p.resolver.ResolveMessage(ctx, p.providerName, externalUserID, channelID)
	if err != nil {
		if errors.Is(err, apperr.ErrUnknownOrigin) || errors.Is(err, apperr.ErrUnknownChannel) || errors.Is(err, apperr.ErrUnauthorized) {
			p.metrics.IncUnknownOrigin()

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

	// Champs de corrélation communs à tous les logs de ce message (PLAN.md
	// §14.2). Uniquement des identifiants : jamais le contenu du message, ni
	// une transcription, ni une pièce jointe.
	logCtx := []any{
		"trigger", model.TriggerMessage,
		"org_id", execIdentity.OrgID,
		"principal_id", execIdentity.PrincipalID,
		"conversation_id", execIdentity.ConversationID,
		"provider", p.providerName,
		"channel_id", channelID,
	}

	if conversation.Kind == model.ChannelGroup {
		if !courier.IsMentioned(msg, self.ID()) {
			p.metrics.IncMessagesIgnoredNoMention()
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
		p.metrics.IncDuplicateMessage()
		p.logger.InfoContext(ctx, "ingress: message déjà traité, ignoré", logCtx...)
		return
	}

	handleCtx, cancelHandle := context.WithTimeout(ctx, handleTimeout)
	reply, attachments, err := p.handler.Handle(handleCtx, execIdentity, conversation, msg)
	cancelHandle()
	if err != nil {
		p.logger.ErrorContext(ctx, "ingress: échec du traitement du message", append(logCtx, "error", err)...)
		p.markFinal(ctx, messageID, statusFailed, logCtx)
		return
	}

	if reply != "" || len(attachments) > 0 {
		options := []courier.BaseMessageOptionFunc{courier.WithMessageMainPart(reply)}
		for _, m := range attachments {
			options = append(options, courier.WithMessagePart(media.ToCourier(m)))
		}

		outgoing := courier.NewMessage(
			courier.RandomMessageID(),
			msg.Channel(),
			self,
			options...,
		)

		sendCtx, cancelSend := context.WithTimeout(ctx, sendTimeout)
		err := p.provider.Send(sendCtx, outgoing)
		cancelSend()
		if err != nil {
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
