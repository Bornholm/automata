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
	"strings"
	"time"

	"github.com/bornholm/go-courier"

	"github.com/bornholm/automata/internal/apperr"
	"github.com/bornholm/automata/internal/audio"
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

// typingRefreshInterval est la période de rafraîchissement de l'indicateur
// « en train d'écrire » pendant le traitement d'un message : WhatsApp efface
// cet état après une dizaine de secondes sans signal, or un tour peut durer
// plusieurs minutes (handleTimeout).
const typingRefreshInterval = 8 * time.Second

// maxBurstMessages borne la taille d'une rafale coalescée. Sans cette
// limite, un flux continu de messages réinitialiserait indéfiniment la
// fenêtre de silence (collectBurst) : le pipeline ne traiterait plus rien et
// le message fusionné grossirait sans borne. Atteindre la limite déclenche
// le traitement immédiat de la rafale en cours.
const maxBurstMessages = 10

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
	coalesceWindow    time.Duration
	// graceWindow est le délai total laissé pour adresser un média de
	// groupe (voir defaultMentionGrace et WithMentionGraceWindow).
	graceWindow time.Duration

	// linking active la liaison par jeton d'un expéditeur inconnu (socle
	// SaaS, voir linking.go et WithLinking).
	linking bool
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

			burst := p.collectBurst(ctx, messages, msg, self)
			if ctx.Err() != nil {
				return ctx.Err()
			}

			// Les mentions se relèvent sur la rafale entière avant tout
			// découpage : c'est ce qui permet à « @assistant » écrit juste
			// après un vocal de le rendre adressé (voir addressedOrigins).
			addressed := addressedOrigins(burst, self)

			for _, group := range groupBurst(burst, self) {
				p.processBatch(ctx, self, group, addressed)
			}
		}
	}
}

// WithCoalesceWindow active la coalescence des rafales : après un premier
// message, le pipeline attend que window s'écoule sans nouvelle arrivée
// avant de traiter, et fusionne les messages texte consécutifs d'un même
// expéditeur sur un même canal en un seul tour de conversation. Sur
// WhatsApp, trois messages courts envoyés coup sur coup forment presque
// toujours une seule pensée : sans coalescence, chacun déclencherait un
// tour LLM complet, trois réponses entremêlées et trois fois le coût.
// window <= 0 désactive la coalescence (comportement historique).
func (p *Pipeline) WithCoalesceWindow(window time.Duration) *Pipeline {
	p.coalesceWindow = window
	return p
}

// collectBurst rassemble first et tous les messages arrivant ensuite tant
// que la fenêtre de silence coalesceWindow n'est pas écoulée — chaque
// arrivée la réinitialise —, dans la limite de maxBurstMessages. Le prix de
// la coalescence est un délai fixe de coalesceWindow ajouté à chaque tour :
// c'est pour cela que la fenêtre se compte en secondes, pas en minutes.
func (p *Pipeline) collectBurst(ctx context.Context, messages chan courier.Message, first courier.Message, self courier.User) []courier.Message {
	batch := []courier.Message{first}
	if p.coalesceWindow <= 0 {
		return batch
	}

	timer := time.NewTimer(p.coalesceWindow)
	defer timer.Stop()

	// La prolongation de grâce ne s'accorde qu'une fois : sans cela, une
	// suite de vocaux non adressés repousserait le traitement indéfiniment.
	graceUsed := false

	for len(batch) < maxBurstMessages {
		select {
		case <-ctx.Done():
			return batch
		case msg, ok := <-messages:
			if !ok {
				return batch
			}

			batch = append(batch, msg)

			if !timer.Stop() {
				<-timer.C
			}
			timer.Reset(p.coalesceWindow)
		case <-timer.C:
			if !graceUsed && awaitsMention(batch, self) {
				// Le sursis se compte depuis le début de l'attente : une
				// fenêtre de coalescence déjà plus longue l'a épuisé.
				if remaining := p.effectiveGrace() - p.coalesceWindow; remaining > 0 {
					graceUsed = true
					timer.Reset(remaining)
					continue
				}
			}

			return batch
		}
	}

	return batch
}

// defaultMentionGrace est le délai total laissé à quelqu'un pour adresser un
// média à l'assistant dans un groupe.
//
// Un vocal ne peut porter aucune mention : sur WhatsApp, un message audio n'a
// pas de légende. Le geste possible est d'envoyer le vocal puis d'écrire
// « @assistant » — mais taper cette mention prend bien plus que la fenêtre de
// coalescence, qui se compte en secondes. Sans ce sursis, le vocal serait
// jugé non adressé et jeté avant que la mention n'arrive.
//
// Le sursis ne coûte rien au cas courant : il ne s'ouvre que pour un média
// reçu en groupe et encore non adressé (voir awaitsMention). Une conversation
// privée, un message mentionné, un simple texte de groupe partent à la
// fenêtre habituelle.
const defaultMentionGrace = 15 * time.Second

// WithMentionGraceWindow règle le sursis accordé à un média de groupe encore
// non adressé. window <= 0 le désactive : un vocal non mentionné dans la
// fenêtre de coalescence est alors ignoré comme n'importe quel message.
func (p *Pipeline) WithMentionGraceWindow(window time.Duration) *Pipeline {
	p.graceWindow = window
	return p
}

// effectiveGrace retourne le sursis à appliquer, la valeur par défaut tant
// qu'aucune n'a été réglée.
func (p *Pipeline) effectiveGrace() time.Duration {
	if p.graceWindow == 0 {
		return defaultMentionGrace
	}
	return p.graceWindow
}

// awaitsMention indique si la rafale contient un média de groupe qu'aucune
// mention du même expéditeur ne rend encore adressé à l'assistant — le seul
// cas où il vaut la peine d'attendre encore.
func awaitsMention(batch []courier.Message, self courier.User) bool {
	if self == nil {
		return false
	}

	addressed := addressedOrigins(batch, self)

	for _, msg := range batch {
		if msg.Channel() == nil || msg.Channel().Kind() != courier.ChannelKindGroup {
			continue
		}
		if coalescable(msg) {
			// Du texte : s'il devait être adressé, il porterait sa mention.
			continue
		}
		if !addressed[originOf(msg)] {
			return true
		}
	}

	return false
}

// origin identifie un expéditeur sur un canal : la granularité à laquelle
// une mention rend adressés les messages voisins.
type origin struct {
	channelID courier.ChannelID
	userID    courier.UserID
}

func originOf(msg courier.Message) origin {
	o := origin{userID: msg.From().ID()}
	if msg.Channel() != nil {
		o.channelID = msg.Channel().ChannelID()
	}
	return o
}

// addressedOrigins relève les expéditeurs qui ont mentionné l'assistant
// quelque part dans la rafale.
//
// La portée est la rafale entière, pas le seul groupe fusionné : un vocal
// forme toujours son propre groupe (mergeBurst ne joint que du texte et
// perdrait la pièce jointe), donc une mention écrite juste après ne le
// couvrirait jamais si l'on s'en tenait au groupe. À l'échelle de la rafale,
// « le vocal, puis @assistant » se lit comme le geste unique qu'il est.
func addressedOrigins(batch []courier.Message, self courier.User) map[origin]bool {
	addressed := map[origin]bool{}
	if self == nil {
		return addressed
	}

	for _, msg := range batch {
		if courier.IsMentioned(msg, self.ID()) {
			addressed[originOf(msg)] = true
		}
	}

	return addressed
}

// groupBurst découpe une rafale en groupes de messages fusionnables : les
// messages CONSÉCUTIFS d'un même expéditeur sur un même canal, purement
// textuels, forment un groupe ; tout autre message (pièce jointe, audio,
// réponse à un message précis, autre origine) constitue son propre groupe.
// Ne regrouper que les runs consécutifs préserve strictement l'ordre
// d'arrivée : un texte, une image, puis un texte donnent trois tours, jamais
// une fusion des deux textes qui enjamberait l'image.
func groupBurst(msgs []courier.Message, self courier.User) [][]courier.Message {
	var groups [][]courier.Message

	for _, msg := range msgs {
		if len(groups) > 0 {
			current := groups[len(groups)-1]
			prev := current[len(current)-1]
			if sameOrigin(prev, msg) &&
				((coalescable(prev) && coalescable(msg)) || captions(prev, msg, self)) {
				groups[len(groups)-1] = append(current, msg)
				continue
			}
		}

		groups = append(groups, []courier.Message{msg})
	}

	return groups
}

// captions indique si l'un de ces deux messages consécutifs sert de légende à
// l'autre : un média de groupe et le texte qui l'adresse à l'assistant.
//
// C'est la seule fusion qui traverse la frontière texte/média, et elle
// n'existe que pour un geste précis. Un vocal ne peut porter aucune mention —
// sur WhatsApp un message audio n'a pas de légende —, alors on l'envoie et on
// écrit « @assistant » juste après. Les deux messages sont une seule phrase :
// les traiter séparément donnerait deux tours, deux réponses et deux fois le
// coût, pour une seule demande.
//
// Hors de ce cas, un média casse la rafale comme avant : en conversation
// privée, ou sans mention, l'ordre d'arrivée reste strictement préservé.
func captions(a, b courier.Message, self courier.User) bool {
	if self == nil {
		return false
	}

	media, text := a, b
	if coalescable(a) {
		media, text = b, a
	}

	if coalescable(media) || !coalescable(text) {
		// Deux médias, ou deux textes : ce n'est pas ce cas de figure.
		return false
	}
	if media.Channel() == nil || media.Channel().Kind() != courier.ChannelKindGroup {
		return false
	}

	return courier.IsMentioned(text, self.ID())
}

// coalescable indique si un message peut être fusionné avec ses voisins de
// rafale : uniquement du texte (toutes les parts sont la part principale) et
// pas une réponse à un message précis — fusionner une réponse lui ferait
// perdre son fil.
func coalescable(msg courier.Message) bool {
	if _, ok := courier.InReplyTo(msg); ok {
		return false
	}

	for _, part := range msg.Parts() {
		if part.Name() != courier.PartMain {
			return false
		}
	}

	return true
}

// sameOrigin indique si deux messages viennent du même expéditeur sur le
// même canal.
func sameOrigin(a, b courier.Message) bool {
	return a.Channel().ChannelID() == b.Channel().ChannelID() && a.From().ID() == b.From().ID()
}

// mergeBurst fusionne les messages texte d'un groupe en un unique message
// portant leurs contenus joints (dans l'ordre d'arrivée) et l'union de leurs
// mentions. L'identité du message fusionné (ID, canal, expéditeur, date)
// est celle du dernier message du groupe ; la déduplication, elle, porte sur
// chaque identifiant d'origine (voir processBatch).
func mergeBurst(ctx context.Context, msgs []courier.Message) (courier.Message, error) {
	texts := make([]string, 0, len(msgs))
	var mentions []courier.Mention

	for _, msg := range msgs {
		content, err := courier.GetMessageMainContent(ctx, msg)
		if err != nil {
			if errors.Is(err, courier.ErrNotFound) {
				// Message sans part principale : rien à joindre.
				continue
			}
			return nil, fmt.Errorf("lecture du contenu du message %q: %w", msg.ID(), err)
		}

		if content != "" {
			texts = append(texts, content)
		}

		mentions = append(mentions, courier.Mentions(msg)...)
	}

	last := msgs[len(msgs)-1]
	options := []courier.BaseMessageOptionFunc{
		courier.WithMessageMainPart(strings.Join(texts, "\n")),
		courier.WithMessageSentAt(last.SentAt()),
	}
	if len(mentions) > 0 {
		options = append(options, courier.WithMessageMentions(mentions...))
	}

	// Les parts autres que la principale — vocal, image, document — suivent
	// le message fusionné. Les oublier ici reviendrait à jeter la pièce
	// jointe au moment même où la mention voisine vient de la rendre
	// adressée (voir captions).
	for _, msg := range msgs {
		for _, part := range msg.Parts() {
			if part.Name() != courier.PartMain {
				options = append(options, courier.WithMessagePart(part))
			}
		}
	}

	return courier.NewMessage(last.ID(), last.Channel(), last.From(), options...), nil
}

// processMessage traite un unique message entrant. Toute erreur est
// journalisée et n'interrompt jamais la boucle appelante (voir AGENTS.md :
// le pipeline continue avec le message suivant). msgs contient soit un
// message isolé, soit un groupe fusionnable produit par groupBurst : des
// messages texte consécutifs d'un même expéditeur sur un même canal, à
// traiter comme un seul tour de conversation.
// addressed relève, pour la rafale entière, les expéditeurs ayant mentionné
// l'assistant ; il peut être nil quand le groupe se suffit à lui-même.
func (p *Pipeline) processBatch(ctx context.Context, self courier.User, msgs []courier.Message, addressed map[origin]bool) {
	for range msgs {
		p.metrics.IncMessagesReceived()
	}

	// Tous les messages d'un groupe partagent expéditeur et canal
	// (sameOrigin) : la résolution d'identité du premier vaut pour tous.
	first := msgs[0]
	externalUserID := string(first.From().ID())
	channelID := string(first.Channel().ChannelID())

	execIdentity, conversation, err := p.resolver.ResolveMessage(ctx, p.providerName, externalUserID, channelID)
	if err != nil {
		if errors.Is(err, apperr.ErrUnknownOrigin) || errors.Is(err, apperr.ErrUnknownChannel) || errors.Is(err, apperr.ErrUnauthorized) {
			// Avant d'ignorer un inconnu : son message porte peut-être un
			// jeton de liaison (socle SaaS, voir linking.go). Le
			// rattachement effectué, le message courant sert uniquement à
			// le déclencher — le tour suivant sera résolu normalement.
			linkLogCtx := []any{"provider", p.providerName, "channel_id", channelID}
			if result, attempted, linkErr := p.tryLink(ctx, first, externalUserID, channelID, linkLogCtx); attempted {
				if linkErr != nil {
					p.logger.ErrorContext(ctx, "ingress: échec de la liaison par jeton", append(linkLogCtx, "error", linkErr)...)
				}
				if result.Reply != "" && ctx.Err() == nil {
					p.send(ctx, courier.NewMessage(
						courier.RandomMessageID(),
						first.Channel(),
						self,
						courier.WithMessageMainPart(result.Reply),
					), linkLogCtx)
				}
				return
			}

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
				"channel_kind", string(first.Channel().Kind()),
				"channel_name", first.Channel().Name(),
				"user_id", externalUserID,
				"user_name", first.From().DisplayName(),
				// Le motif exact distingue trois situations qui n'appellent
				// pas le même geste : un inconnu à rattacher, un canal non
				// lié, ou quelqu'un de connu mais sans compte dans
				// l'organisation du canal. L'erreur ne porte que des
				// identifiants, jamais le message.
				"raison", err.Error(),
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
		// Une seule mention du même expéditeur suffit, où qu'elle se trouve
		// dans la rafale : les messages voisins sont le contexte de
		// celle-ci. Sans aucune mention, tout le groupe est ignoré, comme
		// chaque message l'aurait été isolément.
		mentioned := false
		for _, msg := range msgs {
			if courier.IsMentioned(msg, self.ID()) || addressed[originOf(msg)] {
				mentioned = true
				break
			}
		}

		if !mentioned {
			// Exception : un VOCAL ne peut porter aucune mention — sur
			// WhatsApp, un audio n'a pas de légende. La mention se cherche
			// alors dans son contenu : le message continue jusqu'au handler
			// avec le marqueur de mention vocale, et le tour s'arrête après
			// la transcription si le nom de l'assistant n'y figure pas.
			// Coût assumé : chaque vocal du groupe est transcrit. Rien
			// n'en est conservé quand la mention manque.
			if batchHasAudio(msgs) {
				ctx = model.ContextWithVoiceMentionRequired(ctx, voiceMentionName(self))
			} else {
				for range msgs {
					p.metrics.IncMessagesIgnoredNoMention()
				}
				p.logger.InfoContext(ctx, "ingress: message de groupe ignoré (assistant non mentionné)", logCtx...)
				return
			}
		}
	}

	// Déduplication de chaque message d'origine, jamais du message fusionné :
	// la redélivrance d'un seul message d'une rafale déjà traitée doit être
	// reconnue individuellement.
	fresh := make([]courier.Message, 0, len(msgs))
	for _, msg := range msgs {
		duplicate, err := p.markProcessing(ctx, string(msg.ID()))
		if err != nil {
			p.logger.ErrorContext(ctx, "ingress: échec de la déduplication du message", append(logCtx, "error", err)...)
			continue
		}

		if duplicate {
			p.metrics.IncDuplicateMessage()
			p.logger.InfoContext(ctx, "ingress: message déjà traité, ignoré", logCtx...)
			continue
		}

		fresh = append(fresh, msg)
	}

	if len(fresh) == 0 {
		return
	}

	msg := fresh[0]
	if len(fresh) > 1 {
		merged, err := mergeBurst(ctx, fresh)
		if err != nil {
			// Fusion impossible (lecture d'une part échouée) : traiter
			// chaque message séquentiellement plutôt que d'en perdre un.
			p.logger.WarnContext(ctx, "ingress: fusion de rafale impossible, traitement individuel", append(logCtx, "error", err)...)
			for _, m := range fresh {
				p.handleResolved(ctx, self, execIdentity, conversation, m, []string{string(m.ID())}, logCtx)
			}
			return
		}

		p.metrics.IncMessagesCoalesced(len(fresh) - 1)
		p.logger.InfoContext(ctx, "ingress: rafale fusionnée en un tour", append(logCtx, "messages", len(fresh))...)
		msg = merged
	}

	messageIDs := make([]string, len(fresh))
	for i, m := range fresh {
		messageIDs[i] = string(m.ID())
	}

	p.handleResolved(ctx, self, execIdentity, conversation, msg, messageIDs, logCtx)
}

// handleResolved exécute le traitement métier d'un message déjà résolu,
// autorisé et marqué en cours de traitement, puis envoie la réponse et
// enregistre le statut final de chaque identifiant de messageIDs (plusieurs
// pour un message fusionné depuis une rafale).
func (p *Pipeline) handleResolved(ctx context.Context, self courier.User, execIdentity model.ExecutionIdentity, conversation model.Conversation, msg courier.Message, messageIDs []string, logCtx []any) {
	// Pas d'indicateur de saisie tant qu'une mention vocale n'est pas
	// confirmée : « Automata est en train d'écrire » à chaque vocal du
	// groupe révélerait qu'il les écoute tous — y compris ceux qui ne lui
	// sont pas adressés et dont rien n'est conservé.
	stopTyping := func() {}
	if _, verifying := model.VoiceMentionRequired(ctx); !verifying {
		stopTyping = p.startTyping(ctx, msg.Channel().ChannelID(), logCtx)
	}

	handleCtx, cancelHandle := context.WithTimeout(ctx, handleTimeout)
	reply, attachments, err := p.handler.Handle(handleCtx, execIdentity, conversation, msg)
	cancelHandle()

	// Arrêt AVANT l'envoi de la réponse : l'indicateur couvre la réflexion,
	// pas la livraison, et un envoi efface de toute façon l'état côté
	// fournisseur.
	stopTyping()
	if err != nil {
		// Un échec qui vient de ce que la personne a envoyé (note vocale
		// inaudible, format inconnu) n'est pas une panne : elle reçoit une
		// explication utile plutôt que « réessaie dans quelques instants »,
		// et le journal l'enregistre en avertissement, pas en erreur — sans
		// quoi l'alerting se déclencherait sur un micro mal placé.
		reply, explained := apperr.UserReply(err)
		if explained {
			p.logger.WarnContext(ctx, "ingress: message non exploitable, explication envoyée", append(logCtx, "error", err)...)
		} else {
			reply = FallbackReply
			p.logger.ErrorContext(ctx, "ingress: échec du traitement du message", append(logCtx, "error", err)...)
		}

		p.markFinalAll(ctx, messageIDs, statusFailed, logCtx)

		// Réponse envoyée sauf à l'arrêt du processus : un échec dû à
		// l'annulation du contexte parent n'est pas une panne à signaler, et
		// l'envoi échouerait de toute façon. Best effort : son propre échec
		// est déjà journalisé par send.
		if ctx.Err() == nil {
			p.send(ctx, courier.NewMessage(
				courier.RandomMessageID(),
				msg.Channel(),
				self,
				courier.WithMessageMainPart(reply),
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
			p.markFinalAll(ctx, messageIDs, statusFailed, logCtx)
			return
		}
	}

	p.markFinalAll(ctx, messageIDs, statusProcessed, logCtx)
}

// startTyping affiche l'indicateur « en train d'écrire » sur le canal et le
// rafraîchit périodiquement jusqu'à l'appel de stop, si le fournisseur le
// supporte (courier.StatusProvider) — sinon stop est un no-op. Purement
// cosmétique : aucun échec ici n'affecte le traitement du message, les
// erreurs sont journalisées en debug seulement.
func (p *Pipeline) startTyping(ctx context.Context, channelID courier.ChannelID, logCtx []any) (stop func()) {
	statusProvider, ok := p.provider.(courier.StatusProvider)
	if !ok {
		return func() {}
	}

	typingCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)

		ticker := time.NewTicker(typingRefreshInterval)
		defer ticker.Stop()

		for {
			if err := statusProvider.SetStatus(typingCtx, courier.StatusTyping, channelID); err != nil {
				if typingCtx.Err() == nil {
					p.logger.DebugContext(ctx, "ingress: échec de l'indicateur de saisie", append(logCtx, "error", err)...)
				}
				return
			}

			select {
			case <-typingCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	return func() {
		cancel()
		<-done

		// Effacement explicite, sauf à l'arrêt du processus : l'état
		// disparaîtrait de lui-même côté fournisseur, inutile de retarder
		// l'extinction pour ça.
		if ctx.Err() != nil {
			return
		}

		idleCtx, cancelIdle := context.WithTimeout(ctx, 5*time.Second)
		defer cancelIdle()
		if err := statusProvider.SetStatus(idleCtx, courier.StatusIdle, channelID); err != nil {
			p.logger.DebugContext(ctx, "ingress: échec de l'effacement de l'indicateur de saisie", append(logCtx, "error", err)...)
		}
	}
}

// markFinalAll enregistre le statut final de chaque message d'une rafale.
func (p *Pipeline) markFinalAll(ctx context.Context, messageIDs []string, status string, logCtx []any) {
	for _, id := range messageIDs {
		p.markFinal(ctx, id, status, logCtx)
	}
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

// batchHasAudio indique si la rafale contient au moins un message vocal.
func batchHasAudio(msgs []courier.Message) bool {
	for _, msg := range msgs {
		if _, found := audio.FindAudio(msg); found {
			return true
		}
	}
	return false
}

// voiceMentionName est le nom à chercher dans une transcription : le nom
// affiché de l'assistant, celui par lequel on l'appelle à voix haute.
func voiceMentionName(self courier.User) string {
	if self != nil && strings.TrimSpace(self.DisplayName()) != "" {
		return strings.TrimSpace(self.DisplayName())
	}
	return "Automata"
}
