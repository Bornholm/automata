// Package conversation relie l'ingress à l'agent généraliste : garantir
// l'existence de la conversation en base, charger l'historique récent,
// exécuter l'agent, puis persister le tour de conversation (message
// utilisateur et réponse). Voir PLAN.md Phase 6.
package conversation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bornholm/go-courier"
	"github.com/google/uuid"

	"github.com/bornholm/automata/internal/action"
	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/audio"
	"github.com/bornholm/automata/internal/media"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/observability"
	"github.com/bornholm/automata/internal/persistence"
)

// defaultHistoryLimit borne le nombre de messages passés rechargés comme
// historique pour chaque exécution de l'agent : juste assez pour une
// conversation cohérente, sans faire grossir indéfiniment le contexte
// envoyé au LLM (PLAN.md Phase 6 : "persister uniquement l'historique
// nécessaire").
const defaultHistoryLimit = 20

const contentKindText = "text"

// persistedVoiceNotePlaceholder est le contenu neutre persisté en base pour
// un message vocal lorsque cfg.Audio.PersistTranscription est faux (valeur
// par défaut) : la transcription réelle n'est alors jamais écrite en base
// (PLAN.md §3.4).
const persistedVoiceNotePlaceholder = "[Message vocal transcrit pour traitement]"

// Handler implémente ingress.Handler : il orchestre chargement de
// l'historique, exécution de l'agent et persistance du tour de
// conversation.
type Handler struct {
	db                 *persistence.DB
	conversations      *persistence.ConversationRepository
	messages           *persistence.MessageRepository
	messageAttachments *persistence.MessageAttachmentRepository
	agent              agent.Agent
	actions            *action.Engine
	historyLimit       int
	now                func() time.Time
	audioCfg           audio.Config
	transcriber        audio.Transcriber
	// attachmentsCfg gouverne les pièces jointes non vocales. Sa valeur zéro
	// (Enabled: false) écarte toute pièce jointe, en le signalant à l'agent.
	attachmentsCfg media.Config
	// persistTranscription reflète cfg.Audio.PersistTranscription : décision
	// distincte du traitement audio lui-même (audio.Config), portant
	// uniquement sur ce qui est écrit dans la table messages (PLAN.md §3.4).
	persistTranscription bool
	// metrics peut être nil (PLAN.md Phase 20, registre de métriques
	// désactivé) : toutes ses méthodes sont alors no-op.
	metrics *observability.Metrics
}

// NewHandler construit un Handler. historyLimit borne le nombre de messages
// rechargés comme historique ; une valeur <= 0 retombe sur
// defaultHistoryLimit. audioCfg et transcriber gouvernent le traitement des
// notes vocales (PLAN.md §3.4, Phase 9) : lorsque audioCfg.Enabled est faux
// ou transcriber est nil, aucune tentative de transcription n'est faite et
// un message sans contenu textuel garde son comportement antérieur (chaîne
// vide transmise telle quelle). persistTranscription contrôle si le texte
// transcrit réel (true) ou une indication neutre (false, par défaut) est
// écrit dans la table messages. metrics observe la latence de
// transcription et la latence d'exécution de l'agent (PLAN.md §14.3).
func NewHandler(db *persistence.DB, a agent.Agent, actions *action.Engine, historyLimit int, audioCfg audio.Config, transcriber audio.Transcriber, persistTranscription bool, metrics *observability.Metrics) *Handler {
	if historyLimit <= 0 {
		historyLimit = defaultHistoryLimit
	}

	return &Handler{
		db:                   db,
		conversations:        persistence.NewConversationRepository(),
		messages:             persistence.NewMessageRepository(),
		messageAttachments:   persistence.NewMessageAttachmentRepository(),
		agent:                a,
		actions:              actions,
		historyLimit:         historyLimit,
		now:                  time.Now,
		audioCfg:             audioCfg,
		transcriber:          transcriber,
		persistTranscription: persistTranscription,
		metrics:              metrics,
	}
}

// WithAttachments active le traitement des pièces jointes non vocales
// (images, documents) selon cfg : extraction du message entrant, transmission
// au modèle, conservation pour le rejeu de l'historique, et renvoi des médias
// produits. Sans cet appel (comportement par défaut), toute pièce jointe est
// écartée et signalée à l'agent. Retourne h pour permettre le chaînage.
func (h *Handler) WithAttachments(cfg media.Config) *Handler {
	h.attachmentsCfg = cfg
	return h
}

// Handle implémente ingress.Handler.
func (h *Handler) Handle(ctx context.Context, identity model.ExecutionIdentity, conv model.Conversation, msg courier.Message) (string, []media.Media, error) {
	text, err := courier.GetMessageMainContent(ctx, msg)
	if err != nil {
		// Un message composé uniquement d'une pièce jointe (ex. une note
		// vocale, sans partie "main" texte) fait retourner courier.ErrNotFound
		// à GetMessageMainContent, pas une chaîne vide : ce cas doit être
		// traité comme un texte vide pour permettre le repli audio ci-dessous,
		// pas comme une erreur fatale.
		if !errors.Is(err, courier.ErrNotFound) {
			return "", nil, fmt.Errorf("conversation: lecture du contenu du message: %w", err)
		}
		text = ""
	}

	// persistedContent est le contenu écrit en base pour ce message "user" :
	// par défaut identique au texte reçu, sauf pour une note vocale
	// transcrite sans conservation de la transcription (voir plus bas).
	persistedContent := text

	if text == "" && h.audioCfg.Enabled {
		voiceNote, found := audio.FindAudio(msg)
		if found {
			transcriptionStart := time.Now()
			transcribed, err := audio.ExtractText(ctx, h.audioCfg, h.transcriber, voiceNote)
			h.metrics.ObserveTranscriptionLatency(time.Since(transcriptionStart))
			if err != nil {
				return "", nil, fmt.Errorf("conversation: transcription de la note vocale: %w", err)
			}

			text = transcribed
			if h.persistTranscription {
				persistedContent = transcribed
			} else {
				persistedContent = persistedVoiceNotePlaceholder
			}
		}
	}

	// Pièces jointes non vocales du message courant. Celles qui sont écartées
	// (type refusé, trop volumineuses) ne disparaissent pas en silence : elles
	// sont annoncées à l'agent, qui peut alors l'expliquer plutôt que de
	// répondre à côté d'une image qu'il n'a jamais vue.
	attachmentsCfg := h.attachmentsCfg
	attachmentsCfg.SkipAudio = h.audioCfg.Enabled

	attachments, rejected := media.Extract(ctx, msg, attachmentsCfg)
	if len(rejected) > 0 {
		text = strings.TrimSpace(text + "\n\n[pièces jointes non transmises : " + strings.Join(rejected, " ; ") + "]")
		persistedContent = text
	}

	var history []agent.Message

	messageID := uuid.NewString()

	err = h.db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := h.ensureConversation(ctx, tx, conv); err != nil {
			return err
		}

		records, err := h.messages.ListRecentByConversation(ctx, tx, conv.ID, h.historyLimit)
		if err != nil {
			return err
		}

		history, err = h.buildHistory(ctx, tx, records)
		if err != nil {
			return err
		}

		if err := h.messages.Insert(ctx, tx, persistence.Message{
			ID:                messageID,
			ConversationID:    conv.ID,
			ExternalMessageID: string(msg.ID()),
			PrincipalID:       identity.PrincipalID,
			Role:              "user",
			Content:           persistedContent,
			ContentKind:       contentKindText,
			CreatedAt:         h.now().UTC().Format(time.RFC3339),
		}); err != nil {
			return err
		}

		return h.persistAttachments(ctx, tx, messageID, attachments)
	})
	if err != nil {
		return "", nil, fmt.Errorf("conversation: enregistrement du message entrant: %w", err)
	}

	// PLAN.md §10.4 : "confirmer"/"annuler" sont des commandes
	// conversationnelles littérales, jamais des décisions du modèle — la
	// même règle invariante qui interdit au LLM de décider de l'identité,
	// de la portée ou des permissions s'applique à la confirmation d'une
	// action sensible. Cette interception a lieu AVANT tout appel à
	// h.agent.Execute, et le LLM n'est jamais consulté pour ce cas.
	if h.actions != nil {
		if cmd, ok := action.ParseCommand(text); ok {
			reply, err := h.actions.HandleCommand(ctx, identity, conv, cmd)
			if err != nil {
				return "", nil, fmt.Errorf("conversation: traitement de la commande de confirmation: %w", err)
			}

			if err := h.persistAssistantReply(ctx, identity, conv, reply); err != nil {
				return "", nil, err
			}

			return reply, nil, nil
		}
	}

	agentStart := time.Now()
	result, err := h.agent.Execute(ctx, agent.Request{
		Identity:     identity,
		Conversation: conv,
		History:      history,
		Input:        text,
		Attachments:  attachments,
	})
	h.metrics.ObserveAgentLatency(time.Since(agentStart))
	if err != nil {
		return "", nil, fmt.Errorf("conversation: exécution de l'agent: %w", err)
	}

	reply := result.Reply

	if h.actions != nil && len(result.ProposedActions) > 0 {
		_, planText, err := h.actions.CreatePlan(ctx, identity, result.ProposedActions)
		if err != nil {
			return "", nil, fmt.Errorf("conversation: création du plan d'actions: %w", err)
		}
		reply = strings.TrimSpace(reply + "\n\n" + planText)
	}

	if err := h.persistAssistantReply(ctx, identity, conv, reply); err != nil {
		return "", nil, err
	}

	return reply, h.boundReplyAttachments(result.Attachments), nil
}

// boundReplyAttachments borne le nombre de médias joints à une réponse
// (attachments.max_reply) : un outil prolixe ne doit pas inonder la
// conversation de l'utilisateur. Une limite <= 0 laisse passer tout ce qui a
// été produit.
func (h *Handler) boundReplyAttachments(medias []media.Media) []media.Media {
	if len(medias) == 0 || h.attachmentsCfg.MaxReply <= 0 || len(medias) <= h.attachmentsCfg.MaxReply {
		return medias
	}

	return medias[:h.attachmentsCfg.MaxReply]
}

// persistAttachments enregistre les pièces jointes du message messageID, dans
// leur ordre de réception, afin de pouvoir être rejouées dans l'historique.
func (h *Handler) persistAttachments(ctx context.Context, tx *sql.Tx, messageID string, medias []media.Media) error {
	now := h.now().UTC().Format(time.RFC3339)

	for i, m := range medias {
		if err := h.messageAttachments.Insert(ctx, tx, persistence.MessageAttachment{
			ID:        uuid.NewString(),
			MessageID: messageID,
			Position:  i,
			Kind:      string(m.Kind),
			MimeType:  m.MimeType,
			Filename:  m.Filename,
			Caption:   m.Caption,
			Data:      m.Data,
			CreatedAt: now,
		}); err != nil {
			return err
		}
	}

	return nil
}

// buildHistory convertit les messages persistés en historique d'agent, en y
// rejoignant les pièces jointes conservées.
//
// Le nombre de pièces jointes rejouées est borné par
// attachments.max_history, les plus récentes d'abord : sans cette borne, une
// conversation riche en images ferait croître indéfiniment la taille — et le
// coût — de chaque requête au modèle.
func (h *Handler) buildHistory(ctx context.Context, tx *sql.Tx, records []persistence.Message) ([]agent.Message, error) {
	history := toAgentHistory(records)

	if h.attachmentsCfg.MaxHistory <= 0 || len(records) == 0 {
		return history, nil
	}

	messageIDs := make([]string, 0, len(records))
	for _, m := range records {
		// Les messages de l'assistant ne portent jamais de pièce jointe
		// persistée, inutile de les interroger.
		if m.Role == "user" {
			messageIDs = append(messageIDs, m.ID)
		}
	}

	byMessage, err := h.messageAttachments.ListByMessageIDs(ctx, tx, messageIDs, h.attachmentsCfg.MaxHistory)
	if err != nil {
		return nil, err
	}

	for i, record := range records {
		stored := byMessage[record.ID]
		if len(stored) == 0 {
			continue
		}

		medias := make([]media.Media, 0, len(stored))
		for _, a := range stored {
			medias = append(medias, media.Media{
				Kind:     media.Kind(a.Kind),
				MimeType: a.MimeType,
				Filename: a.Filename,
				Caption:  a.Caption,
				Data:     a.Data,
			})
		}

		history[i].Attachments = medias
	}

	return history, nil
}

// persistAssistantReply enregistre reply comme message "assistant" de la
// conversation conv.
func (h *Handler) persistAssistantReply(ctx context.Context, identity model.ExecutionIdentity, conv model.Conversation, reply string) error {
	err := h.db.WithTx(ctx, func(tx *sql.Tx) error {
		return h.messages.Insert(ctx, tx, persistence.Message{
			ID:                uuid.NewString(),
			ConversationID:    conv.ID,
			ExternalMessageID: uuid.NewString(),
			PrincipalID:       identity.PrincipalID,
			Role:              "assistant",
			Content:           reply,
			ContentKind:       contentKindText,
			CreatedAt:         h.now().UTC().Format(time.RFC3339),
		})
	})
	if err != nil {
		return fmt.Errorf("conversation: enregistrement de la réponse: %w", err)
	}
	return nil
}

// ensureConversation insère conv si elle n'existe pas encore, identifiée par
// (provider, external_channel_id).
func (h *Handler) ensureConversation(ctx context.Context, tx *sql.Tx, conv model.Conversation) error {
	_, found, err := h.conversations.FindByProviderAndExternalChannelID(ctx, tx, conv.Provider, conv.ChannelID)
	if err != nil {
		return err
	}
	if found {
		return nil
	}

	now := h.now().UTC().Format(time.RFC3339)

	return h.conversations.Insert(ctx, tx, persistence.Conversation{
		ID:                conv.ID,
		OrgID:             conv.OrgID,
		Provider:          conv.Provider,
		ExternalChannelID: conv.ChannelID,
		Kind:              conv.Kind,
		Scope:             conv.Scope,
		ScopeID:           conv.ScopeID,
		CreatedAt:         now,
		UpdatedAt:         now,
	})
}

// toAgentHistory convertit les messages persistés en historique applicatif
// pour l'agent.
func toAgentHistory(records []persistence.Message) []agent.Message {
	history := make([]agent.Message, 0, len(records))
	for _, m := range records {
		history = append(history, agent.Message{
			Role:    m.Role,
			Content: m.Content,
		})
	}
	return history
}
