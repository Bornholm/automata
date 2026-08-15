// Package conversation relie l'ingress à l'agent généraliste : garantir
// l'existence de la conversation en base, charger l'historique récent,
// exécuter l'agent, puis persister le tour de conversation (message
// utilisateur et réponse). Voir PLAN.md Phase 6.
package conversation

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/bornholm/go-courier"
	"github.com/google/uuid"

	"github.com/bornholm/automata/internal/agent"
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
}

// NewHandler construit un Handler. historyLimit borne le nombre de messages
// rechargés comme historique ; une valeur <= 0 retombe sur
// defaultHistoryLimit.
func NewHandler(db *persistence.DB, a agent.Agent, historyLimit int) *Handler {
	if historyLimit <= 0 {
		historyLimit = defaultHistoryLimit
	}

	return &Handler{
		db:            db,
		conversations: persistence.NewConversationRepository(),
		messages:      persistence.NewMessageRepository(),
		agent:         a,
		historyLimit:  historyLimit,
		now:           time.Now,
	}
}

// Handle implémente ingress.Handler.
func (h *Handler) Handle(ctx context.Context, identity model.ExecutionIdentity, conv model.Conversation, msg courier.Message) (string, error) {
	text, err := courier.GetMessageMainContent(ctx, msg)
	if err != nil {
		return "", fmt.Errorf("conversation: lecture du contenu du message: %w", err)
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
			Content:           text,
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
