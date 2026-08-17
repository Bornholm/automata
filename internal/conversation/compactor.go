package conversation

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/observability"
	"github.com/bornholm/automata/internal/persistence"
)

// defaultMaxSummaryChars borne le résumé persisté quand la configuration ne
// précise rien : assez pour retenir l'essentiel de semaines d'échanges, trop
// peu pour redevenir lui-même un problème de contexte.
const defaultMaxSummaryChars = 2000

// compactionPrompt encadre l'appel de résumé. Aucune donnée de sécurité ne
// transite ici : le modèle ne reçoit que des messages qu'il a déjà vus en
// conversation, et ne produit qu'un texte réinjecté comme contexte.
const compactionPrompt = `Tu condenses l'historique d'une conversation entre un assistant personnel et ses utilisateurs.

À partir de l'ancien résumé (éventuellement vide) et des nouveaux messages, produis un unique résumé à jour, en français, à la troisième personne.

Conserve en priorité : les faits durables (préférences, décisions, engagements, dates), les demandes encore en cours, et le fil général des échanges. Abandonne les salutations et les détails sans lendemain. N'invente rien.

Réponds uniquement par le résumé, sans préambule ni commentaire.`

// Compactor condense les messages plus anciens que la fenêtre d'historique
// en un résumé roulant par conversation (table conversation_summaries).
//
// La compaction s'exécute de façon SYNCHRONE en tête de tour (voir
// Handler.Handle) : le worker étant strictement séquentiel, une goroutine
// d'arrière-plan n'apporterait que des courses ; le coût — un appel LLM sur
// une minorité de tours — est absorbé par l'indicateur « en train
// d'écrire » déjà affiché pendant le traitement.
type Compactor struct {
	db              *persistence.DB
	client          llm.ChatCompletionClient
	messages        *persistence.MessageRepository
	summaries       *persistence.ConversationSummaryRepository
	historyLimit    int
	maxSummaryChars int
	logger          *slog.Logger
	metrics         *observability.Metrics
	now             func() time.Time
}

// NewCompactor construit un Compactor. historyLimit doit être la même
// valeur effective que celle du Handler (0 applique le même défaut) : c'est
// elle qui sépare les messages rejoués verbatim de ceux candidats à la
// compaction. maxSummaryChars à 0 applique defaultMaxSummaryChars.
func NewCompactor(db *persistence.DB, client llm.ChatCompletionClient, historyLimit, maxSummaryChars int, logger *slog.Logger, metrics *observability.Metrics) *Compactor {
	if historyLimit <= 0 {
		historyLimit = defaultHistoryLimit
	}
	if maxSummaryChars <= 0 {
		maxSummaryChars = defaultMaxSummaryChars
	}
	if logger == nil {
		logger = slog.Default()
	}

	return &Compactor{
		db:              db,
		client:          client,
		messages:        persistence.NewMessageRepository(),
		summaries:       persistence.NewConversationSummaryRepository(),
		historyLimit:    historyLimit,
		maxSummaryChars: maxSummaryChars,
		logger:          logger,
		metrics:         metrics,
		now:             time.Now,
	}
}

// WithClock remplace l'horloge (tests).
func (c *Compactor) WithClock(now func() time.Time) *Compactor {
	c.now = now
	return c
}

// CompactIfNeeded condense, si nécessaire, les messages débordant de la
// fenêtre d'historique de la conversation. Le seuil de déclenchement est le
// DOUBLE de la fenêtre : en dessous, rien à faire ; au-delà, tous les
// messages non couverts sauf les historyLimit plus récents sont résumés.
// Déclencher au double plutôt qu'à chaque débordement amortit le coût : un
// appel LLM tous les historyLimit messages environ, pas un par tour.
func (c *Compactor) CompactIfNeeded(ctx context.Context, conversationID model.ConversationID) error {
	var (
		summary persistence.ConversationSummary
		found   bool
		batch   []persistence.Message
		lastRow int64
	)

	err := c.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		summary, found, err = c.summaries.Get(ctx, tx, conversationID)
		if err != nil {
			return err
		}

		uncovered, err := c.messages.CountByConversationAfterRowID(ctx, tx, conversationID, summary.LastMessageRowID)
		if err != nil {
			return err
		}

		if uncovered < int64(c.historyLimit*2) {
			return nil
		}

		toCompact := int(uncovered) - c.historyLimit
		batch, lastRow, err = c.messages.ListOldestByConversationAfterRowID(ctx, tx, conversationID, summary.LastMessageRowID, toCompact)
		return err
	})
	if err != nil {
		return fmt.Errorf("conversation: préparation de la compaction de %q: %w", conversationID, err)
	}

	if len(batch) == 0 {
		return nil
	}

	updated, err := c.summarize(ctx, summary.Summary, batch)
	if err != nil {
		return fmt.Errorf("conversation: résumé de la conversation %q: %w", conversationID, err)
	}

	next := persistence.ConversationSummary{
		ConversationID:   conversationID,
		Summary:          updated,
		LastMessageRowID: lastRow,
		MessagesCovered:  summary.MessagesCovered + int64(len(batch)),
		UpdatedAt:        c.now().UTC().Format(time.RFC3339),
	}
	if !found {
		next.MessagesCovered = int64(len(batch))
	}

	err = c.db.WithTx(ctx, func(tx *sql.Tx) error {
		return c.summaries.Upsert(ctx, tx, next)
	})
	if err != nil {
		return fmt.Errorf("conversation: enregistrement du résumé de %q: %w", conversationID, err)
	}

	c.metrics.IncConversationCompacted()
	// Uniquement des identifiants et des comptes : jamais le résumé ni les
	// messages.
	c.logger.InfoContext(ctx, "conversation: historique compacté",
		"conversation_id", conversationID,
		"messages_compacted", len(batch),
		"messages_covered_total", next.MessagesCovered,
	)

	return nil
}

// summarize produit le résumé fusionnant previous et batch, borné à
// maxSummaryChars runes.
func (c *Compactor) summarize(ctx context.Context, previous string, batch []persistence.Message) (string, error) {
	var b strings.Builder

	if previous != "" {
		b.WriteString("## Ancien résumé\n\n")
		b.WriteString(previous)
		b.WriteString("\n\n")
	}

	b.WriteString("## Nouveaux messages à intégrer\n\n")
	for _, m := range batch {
		fmt.Fprintf(&b, "%s (%s): %s\n", m.Role, m.PrincipalID, m.Content)
	}

	fmt.Fprintf(&b, "\nLongueur maximale du résumé : %d caractères.", c.maxSummaryChars)

	response, err := c.client.ChatCompletion(ctx,
		llm.WithMessages(
			llm.NewMessage(llm.RoleSystem, compactionPrompt),
			llm.NewMessage(llm.RoleUser, b.String()),
		),
	)
	if err != nil {
		return "", err
	}

	summary := strings.TrimSpace(response.Message().Content())

	if runes := []rune(summary); len(runes) > c.maxSummaryChars {
		summary = string(runes[:c.maxSummaryChars])
	}

	return summary, nil
}
