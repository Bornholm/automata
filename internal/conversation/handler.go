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
	"time"

	"github.com/bornholm/go-courier"
	"github.com/google/uuid"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/audio"
	"github.com/bornholm/automata/internal/model"
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
	db            *persistence.DB
	conversations *persistence.ConversationRepository
	messages      *persistence.MessageRepository
	agent         agent.Agent
	historyLimit  int
	now           func() time.Time
	audioCfg      audio.Config
	transcriber   audio.Transcriber
	// persistTranscription reflète cfg.Audio.PersistTranscription : décision
	// distincte du traitement audio lui-même (audio.Config), portant
	// uniquement sur ce qui est écrit dans la table messages (PLAN.md §3.4).
	persistTranscription bool
}

// NewHandler construit un Handler. historyLimit borne le nombre de messages
// rechargés comme historique ; une valeur <= 0 retombe sur
// defaultHistoryLimit. audioCfg et transcriber gouvernent le traitement des
// notes vocales (PLAN.md §3.4, Phase 9) : lorsque audioCfg.Enabled est faux
// ou transcriber est nil, aucune tentative de transcription n'est faite et
// un message sans contenu textuel garde son comportement antérieur (chaîne
// vide transmise telle quelle). persistTranscription contrôle si le texte
// transcrit réel (true) ou une indication neutre (false, par défaut) est
// écrit dans la table messages.
func NewHandler(db *persistence.DB, a agent.Agent, historyLimit int, audioCfg audio.Config, transcriber audio.Transcriber, persistTranscription bool) *Handler {
	if historyLimit <= 0 {
		historyLimit = defaultHistoryLimit
	}

	return &Handler{
		db:                   db,
		conversations:        persistence.NewConversationRepository(),
		messages:             persistence.NewMessageRepository(),
		agent:                a,
		historyLimit:         historyLimit,
		now:                  time.Now,
		audioCfg:             audioCfg,
		transcriber:          transcriber,
		persistTranscription: persistTranscription,
	}
}

// Handle implémente ingress.Handler.
func (h *Handler) Handle(ctx context.Context, identity model.ExecutionIdentity, conv model.Conversation, msg courier.Message) (string, error) {
	text, err := courier.GetMessageMainContent(ctx, msg)
	if err != nil {
		// Un message composé uniquement d'une pièce jointe (ex. une note
		// vocale, sans partie "main" texte) fait retourner courier.ErrNotFound
		// à GetMessageMainContent, pas une chaîne vide : ce cas doit être
		// traité comme un texte vide pour permettre le repli audio ci-dessous,
		// pas comme une erreur fatale.
		if !errors.Is(err, courier.ErrNotFound) {
			return "", fmt.Errorf("conversation: lecture du contenu du message: %w", err)
		}
		text = ""
	}

	// persistedContent est le contenu écrit en base pour ce message "user" :
	// par défaut identique au texte reçu, sauf pour une note vocale
	// transcrite sans conservation de la transcription (voir plus bas).
	persistedContent := text

	if text == "" && h.audioCfg.Enabled {
		voiceNote, found := audio.FindVoiceNote(msg)
		if found {
			transcribed, err := audio.ExtractText(ctx, h.audioCfg, h.transcriber, voiceNote)
			if err != nil {
				return "", fmt.Errorf("conversation: transcription de la note vocale: %w", err)
			}

			text = transcribed
			if h.persistTranscription {
				persistedContent = transcribed
			} else {
				persistedContent = persistedVoiceNotePlaceholder
			}
		}
	}

	var history []agent.Message

	err = h.db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := h.ensureConversation(ctx, tx, conv); err != nil {
			return err
		}

		records, err := h.messages.ListRecentByConversation(ctx, tx, conv.ID, h.historyLimit)
		if err != nil {
			return err
		}
		history = toAgentHistory(records)

		return h.messages.Insert(ctx, tx, persistence.Message{
			ID:                uuid.NewString(),
			ConversationID:    conv.ID,
			ExternalMessageID: string(msg.ID()),
			PrincipalID:       identity.PrincipalID,
			Role:              "user",
			Content:           persistedContent,
			ContentKind:       contentKindText,
			CreatedAt:         h.now().UTC().Format(time.RFC3339),
		})
	})
	if err != nil {
		return "", fmt.Errorf("conversation: enregistrement du message entrant: %w", err)
	}

	result, err := h.agent.Execute(ctx, agent.Request{
		Identity:     identity,
		Conversation: conv,
		History:      history,
		Input:        text,
	})
	if err != nil {
		return "", fmt.Errorf("conversation: exécution de l'agent: %w", err)
	}

	err = h.db.WithTx(ctx, func(tx *sql.Tx) error {
		return h.messages.Insert(ctx, tx, persistence.Message{
			ID:                uuid.NewString(),
			ConversationID:    conv.ID,
			ExternalMessageID: uuid.NewString(),
			PrincipalID:       identity.PrincipalID,
			Role:              "assistant",
			Content:           result.Reply,
			ContentKind:       contentKindText,
			CreatedAt:         h.now().UTC().Format(time.RFC3339),
		})
	})
	if err != nil {
		return "", fmt.Errorf("conversation: enregistrement de la réponse: %w", err)
	}

	return result.Reply, nil
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
