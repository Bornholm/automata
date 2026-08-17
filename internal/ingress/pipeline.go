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

// sendTimeout borne chaque tentative d'envoi de la réponse au fournisseur
// courier, un appel réseau distinct du traitement métier.
const sendTimeout = 30 * time.Second

// sendMaxAttempts et sendRetryBaseDelay gouvernent le retry de l'envoi :
// une micro-coupure réseau au moment du Send perdrait sinon la réponse d'un
// tour pourtant réussi (et facturé). Le backoff est doublé entre chaque
// tentative (1 s puis 2 s), soit 3 s d'attente au pire, négligeable devant
// handleTimeout. Le scheduler a son propre mécanisme, persisté celui-là
// (delivery_attempts) : ici la réponse n'existe qu'en mémoire, un retry
// immédiat borné suffit.
const (
	sendMaxAttempts    = 3
	sendRetryBaseDelay = 1 * time.Second
)

// FallbackReply est envoyé à l'utilisateur quand le traitement de son
// message échoue définitivement (erreur du Handler, retries LLM épuisés).
// Sans elle, panne et silence délibéré seraient indistinguables pour la
// personne qui a écrit. Le texte ne révèle volontairement rien de la cause :
// les détails restent dans les journaux (jamais de contenu privé, mais pas
// non plus d'erreur interne exposée côté canal).
const FallbackReply = "Désolé, je n'ai pas réussi à traiter ce message. Réessaie dans quelques instants."

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

			// Ce log est la seule source des identifiants à déclarer dans
			// identities et channels : un identifiant de groupe ou de
			// conversation privée est attribué par le fournisseur et ne peut
			// pas être connu avant qu'un premier message n'en provienne. On
			// journalise donc de quoi remplir la configuration — identifiants
			// et libellés d'affichage, jamais le contenu du message.
			p.logger.InfoContext(ctx, "ingress: message ignoré (identité non résolue ou non autorisée)",
				"provider", p.providerName,
				"channel_id", channelID,
				"channel_kind", string(msg.Channel().Kind()),
				"channel_name", msg.Channel().Name(),
				"user_id", externalUserID,
				"user_name", msg.From().DisplayName(),
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

		// Réponse de repli, sauf à l'arrêt du processus : un échec dû à
		// l'annulation du contexte parent n'est pas une panne à signaler, et
		// l'envoi échouerait de toute façon. Best effort : son propre échec
		// est déjà journalisé par send.
		if ctx.Err() == nil {
			p.send(ctx, courier.NewMessage(
				courier.RandomMessageID(),
				msg.Channel(),
				self,
				courier.WithMessageMainPart(FallbackReply),
			), logCtx)
		}
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

		if err := p.send(ctx, outgoing, logCtx); err != nil {
			p.markFinal(ctx, messageID, statusFailed, logCtx)
			return
		}
	}

	p.markFinal(ctx, messageID, statusProcessed, logCtx)
}

// send transmet outgoing au fournisseur, en réessayant jusqu'à
// sendMaxAttempts fois avec backoff doublé sur toute erreur — chaque
// tentative bornée par sendTimeout. L'envoi d'un message est idempotent côté
// application (aucune écriture locale) ; le pire cas d'un retry après un
// envoi réellement parti est un doublon visible sur le canal, préférable à
// une réponse perdue. L'échec final est journalisé ici ; l'appelant décide
// du statut à persister.
func (p *Pipeline) send(ctx context.Context, outgoing courier.Message, logCtx []any) error {
	var err error
	for attempt := 1; attempt <= sendMaxAttempts; attempt++ {
		sendCtx, cancelSend := context.WithTimeout(ctx, sendTimeout)
		err = p.provider.Send(sendCtx, outgoing)
		cancelSend()
		if err == nil {
			return nil
		}

		if ctx.Err() != nil || attempt == sendMaxAttempts {
			break
		}

		delay := sendRetryBaseDelay << (attempt - 1)
		p.logger.WarnContext(ctx, "ingress: échec d'envoi, nouvelle tentative",
			append(logCtx, "attempt", attempt, "max_attempts", sendMaxAttempts, "delay", delay.String(), "error", err)...)

		select {
		case <-ctx.Done():
			p.logger.ErrorContext(ctx, "ingress: échec de l'envoi de la réponse", append(logCtx, "error", err)...)
			return err
		case <-time.After(delay):
		}
	}

	p.logger.ErrorContext(ctx, "ingress: échec de l'envoi de la réponse", append(logCtx, "attempts", sendMaxAttempts, "error", err)...)
	return err
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
