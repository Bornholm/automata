package conversation

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/llmclients"
	"github.com/bornholm/automata/internal/memory"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/observability"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/usage"
)

// defaultMaxSummaryChars borne le résumé persisté quand la configuration ne
// précise rien : assez pour retenir l'essentiel de semaines d'échanges, trop
// peu pour redevenir lui-même un problème de contexte.
const defaultMaxSummaryChars = 2000

// defaultMaxFacts borne le nombre de faits durables mémorisés par
// compaction quand la configuration ne précise rien : la compaction se
// déclenche tous les historyLimit messages environ, quelques faits par
// vague suffisent — c'est la consolidation périodique
// (internal/consolidation) qui gère la qualité du fonds au long cours.
const defaultMaxFacts = 5

// maxFactChars borne la taille d'un fait individuel : au-delà, ce n'est
// plus un fait mais un résumé, et il n'a rien à faire dans la mémoire à
// long terme.
const maxFactChars = 500

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

	// memories, s'il est renseigné (WithMemoryStore), active l'extraction
	// de faits durables : à chaque compaction, les messages condensés sont
	// aussi passés au LLM pour en extraire les faits qui méritent la
	// mémoire à long terme, stockés dans la portée de la conversation. nil
	// désactive l'extraction (comportement historique).
	memories memory.Store
	maxFacts int

	// episodes, s'il est renseigné (WithEpisodeStore), active la mémoire
	// épisodique : à chaque compaction, le fragment de conversation sur le
	// point d'être dilué dans le résumé est aussi conservé VERBATIM dans le
	// store d'épisodes, retrouvable ensuite par l'outil
	// search_conversation_history. nil désactive l'enregistrement.
	episodes memory.EpisodeStore
	// speakerName résout le nom affiché d'un principal pour étiqueter les
	// répliques d'un épisode. L'identifiant interne n'apparaît JAMAIS dans
	// le contenu d'un épisode (il serait ensuite restitué au modèle par la
	// recherche) : à défaut de nom résolu, c'est le rôle qui étiquette.
	speakerName func(principalID model.PrincipalID) string

	// clientResolver, s'il est renseigné, sert le modèle de compaction que
	// l'organisation a choisi ; le client du constructeur reste le repli.
	clientResolver ModelResolver
}

// ModelResolver choisit le modèle d'un rôle pour une organisation (voir
// llmclients.Resolver). Déclaré ici plutôt qu'importé pour que le paquet
// conversation reste indépendant du catalogue.
type ModelResolver interface {
	ResolveClient(ctx context.Context, role string, orgID model.OrgID) (llmclients.Resolved, error)
}

// WithClientResolver branche le catalogue de modèles : la compaction d'une
// organisation utilise alors le modèle qu'elle a choisi pour le rôle
// « compaction ». Retourne c pour permettre le chaînage.
func (c *Compactor) WithClientResolver(resolver ModelResolver) *Compactor {
	c.clientResolver = resolver
	return c
}

// clientFor retourne le modèle à utiliser pour cette organisation. Une
// résolution en échec n'est jamais fatale : la compaction repart sur le
// client de la configuration plutôt que de laisser l'historique grossir.
func (c *Compactor) clientFor(ctx context.Context, orgID model.OrgID) llm.ChatCompletionClient {
	if c.clientResolver == nil {
		return c.client
	}

	resolved, err := c.clientResolver.ResolveClient(ctx, llmclients.RoleCompaction, orgID)
	if err != nil {
		c.logger.WarnContext(ctx, "conversation: modèle de compaction non résolu, repli sur la configuration",
			"org", string(orgID), "error", err)
		return c.client
	}

	return resolved.Client
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
		messages:        persistence.NewMessageRepository(db.Cipher()),
		summaries:       persistence.NewConversationSummaryRepository(db.Cipher()),
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

// WithMemoryStore active l'extraction de faits durables vers store à
// chaque compaction (conversation.compaction.extract_facts). maxFacts à 0
// applique defaultMaxFacts.
func (c *Compactor) WithMemoryStore(store memory.Store, maxFacts int) *Compactor {
	if maxFacts <= 0 {
		maxFacts = defaultMaxFacts
	}
	c.memories = store
	c.maxFacts = maxFacts
	return c
}

// WithEpisodeStore active la conservation verbatim des fragments condensés
// (conversation.compaction.record_episodes) : chaque vague de compaction
// enregistre un épisode dans store, étiqueté par speakerName.
func (c *Compactor) WithEpisodeStore(store memory.EpisodeStore, speakerName func(principalID model.PrincipalID) string) *Compactor {
	c.episodes = store
	c.speakerName = speakerName
	return c
}

// CompactIfNeeded condense, si nécessaire, les messages débordant de la
// fenêtre d'historique de la conversation. Le seuil de déclenchement est le
// DOUBLE de la fenêtre : en dessous, rien à faire ; au-delà, tous les
// messages non couverts sauf les historyLimit plus récents sont résumés.
// Déclencher au double plutôt qu'à chaque débordement amortit le coût : un
// appel LLM tous les historyLimit messages environ, pas un par tour.
//
// identity et conv servent exclusivement à l'extraction de faits durables
// (WithMemoryStore) : la portée mémoire est TOUJOURS celle de la
// conversation (conv.Scope/ScopeID/OrgID), jamais décidée par le LLM, et le
// principal du tour courant est enregistré comme auteur — le même contrat
// que l'outil conversationnel remember (internal/agent, writeScope).
func (c *Compactor) CompactIfNeeded(ctx context.Context, identity model.ExecutionIdentity, conv model.Conversation) error {
	// Comptabilité d'usage : la compaction est une tâche de fond facturée à
	// l'organisation de la conversation, pas au principal dont le message a
	// déclenché le seuil (PrincipalID vide, voir internal/usage.Record).
	ctx = usage.ContextWithAttribution(ctx, usage.Attribution{
		OrgID:          string(conv.OrgID),
		ConversationID: string(conv.ID),
		Component:      usage.ComponentCompaction,
	})

	conversationID := conv.ID

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

	updated, err := c.summarize(ctx, c.clientFor(ctx, conv.OrgID), summary.Summary, batch)
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

	// La conservation d'un épisode n'est jamais bloquante : comme pour les
	// faits, le résumé est déjà persisté, un échec ne coûte que la version
	// verbatim de cette vague.
	if c.episodes != nil {
		if err := c.recordEpisode(ctx, conv, batch); err != nil {
			c.logger.WarnContext(ctx, "conversation: enregistrement de l'épisode en échec",
				"conversation_id", conversationID, "error", err)
		}
	}

	// L'extraction de faits durables n'est jamais bloquante : le résumé est
	// déjà persisté, un échec ici (LLM indisponible, JSON illisible) coûte
	// au pire quelques faits non mémorisés — ils restent présents dans le
	// résumé roulant.
	if c.memories != nil {
		if err := c.extractFacts(ctx, identity, conv, batch); err != nil {
			c.logger.WarnContext(ctx, "conversation: extraction de faits durables en échec",
				"conversation_id", conversationID, "error", err)
		}
	}

	return nil
}

// recordEpisode conserve verbatim le fragment condensé, dans la portée de
// la conversation — même contrat que extractFacts : jamais en portée org,
// jamais décidé par le LLM. Aucun appel LLM ici : le contenu est la
// transcription brute des messages, horodatée et étiquetée par nom affiché.
func (c *Compactor) recordEpisode(ctx context.Context, conv model.Conversation, batch []persistence.Message) error {
	if conv.Scope != model.ScopePersonal && conv.Scope != model.ScopeGroup {
		return nil
	}

	var (
		b        strings.Builder
		from, to time.Time
	)

	for _, m := range batch {
		stamp := m.CreatedAt
		if ts, err := time.Parse(time.RFC3339, m.CreatedAt); err == nil {
			stamp = ts.UTC().Format("2006-01-02 15:04")
			if from.IsZero() || ts.Before(from) {
				from = ts
			}
			if ts.After(to) {
				to = ts
			}
		}
		fmt.Fprintf(&b, "[%s] %s: %s\n", stamp, c.speakerLabel(m), redactProfileLinks(m.Content))
	}

	_, err := c.episodes.RecordEpisode(ctx, memory.NewEpisode{
		Content:        b.String(),
		OrgID:          conv.OrgID,
		Scope:          conv.Scope,
		ScopeID:        conv.ScopeID,
		ConversationID: conv.ID,
		From:           from,
		To:             to,
	})
	if err != nil {
		return err
	}

	c.metrics.IncEpisodeRecorded()
	// Uniquement des identifiants et des comptes : jamais le contenu.
	c.logger.InfoContext(ctx, "conversation: épisode mémorisé",
		"conversation_id", conv.ID, "messages", len(batch))

	return nil
}

// speakerLabel étiquette une réplique : nom affiché du principal si résolu,
// sinon le rôle ("user", "assistant"). Jamais l'identifiant interne.
func (c *Compactor) speakerLabel(m persistence.Message) string {
	if m.Role != "user" {
		return m.Role
	}
	if c.speakerName != nil {
		if name := c.speakerName(m.PrincipalID); name != "" {
			return name
		}
	}
	return m.Role
}

// factExtractionPrompt encadre l'extraction de faits durables depuis les
// messages sur le point d'être condensés. Comme compactionPrompt, seuls des
// messages déjà vus en conversation transitent ici.
const factExtractionPrompt = `Tu extrais les faits durables d'un fragment de conversation entre un assistant personnel et ses utilisateurs, juste avant que ces messages soient condensés en résumé.

Un fait durable est une information qui restera vraie et utile dans plusieurs semaines : préférence stable d'une personne, décision prise, engagement, date ou échéance importante, information factuelle sur une personne ou un projet. Ne retiens JAMAIS les demandes ponctuelles, les salutations, les états passagers ni le simple fil des échanges.

Réponds UNIQUEMENT par un tableau JSON de chaînes de caractères : un fait par chaîne, formulé en français, à la troisième personne, autonome et compréhensible sans aucun contexte. Réponds [] si aucun fait durable n'est présent. Aucun commentaire, aucun balisage.`

// extractFacts demande au LLM les faits durables du batch condensé et les
// mémorise dans la portée de la conversation. Jamais en portée org : la
// mémoire organisationnelle n'est alimentée que par des écritures
// explicitement autorisées, pas par un mécanisme automatique.
func (c *Compactor) extractFacts(ctx context.Context, identity model.ExecutionIdentity, conv model.Conversation, batch []persistence.Message) error {
	if conv.Scope != model.ScopePersonal && conv.Scope != model.ScopeGroup {
		return nil
	}

	var b strings.Builder
	for _, m := range batch {
		fmt.Fprintf(&b, "%s (%s): %s\n", m.Role, m.PrincipalID, redactProfileLinks(m.Content))
	}

	response, err := c.clientFor(ctx, conv.OrgID).ChatCompletion(ctx,
		llm.WithMessages(
			llm.NewMessage(llm.RoleSystem, factExtractionPrompt),
			llm.NewMessage(llm.RoleUser, b.String()),
		),
	)
	if err != nil {
		return fmt.Errorf("appel du client llm: %w", err)
	}

	facts, err := parseFacts(response.Message().Content())
	if err != nil {
		return err
	}

	if len(facts) > c.maxFacts {
		facts = facts[:c.maxFacts]
	}

	stored := 0
	for _, fact := range facts {
		if runes := []rune(fact); len(runes) > maxFactChars {
			fact = string(runes[:maxFactChars])
		}

		_, err := c.memories.Remember(ctx, memory.NewMemory{
			Content:              fact,
			Scope:                conv.Scope,
			ScopeID:              conv.ScopeID,
			OrgID:                conv.OrgID,
			OwnerPrincipalID:     identity.PrincipalID,
			CreatedBy:            identity.PrincipalID,
			SourceConversationID: conv.ID,
			Origin:               "compaction",
		})
		if err != nil {
			// On continue avec les faits suivants : un échec d'indexation
			// isolé ne doit pas perdre toute la vague.
			c.logger.WarnContext(ctx, "conversation: mémorisation d'un fait en échec",
				"conversation_id", conv.ID, "error", err)
			continue
		}
		stored++
	}

	if stored > 0 {
		c.metrics.AddMemoriesExtracted(stored)
		// Uniquement un compte : jamais le contenu des faits.
		c.logger.InfoContext(ctx, "conversation: faits durables mémorisés",
			"conversation_id", conv.ID, "facts", stored)
	}

	return nil
}

// parseFacts décode la réponse du LLM en liste de faits, en tolérant un
// éventuel bloc de code Markdown autour du JSON (certains modèles en
// ajoutent malgré la consigne) et en écartant les chaînes vides.
func parseFacts(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	var facts []string
	if err := json.Unmarshal([]byte(raw), &facts); err != nil {
		return nil, fmt.Errorf("réponse d'extraction illisible: %w", err)
	}

	out := facts[:0]
	for _, fact := range facts {
		if fact = strings.TrimSpace(fact); fact != "" {
			out = append(out, fact)
		}
	}

	return out, nil
}

// summarize produit le résumé fusionnant previous et batch, borné à
// maxSummaryChars runes.
// client est le modèle retenu pour ce tour de compaction : il dépend de
// l'organisation, il est donc passé plutôt que relu du champ.
func (c *Compactor) summarize(ctx context.Context, client llm.ChatCompletionClient, previous string, batch []persistence.Message) (string, error) {
	var b strings.Builder

	if previous != "" {
		b.WriteString("## Ancien résumé\n\n")
		b.WriteString(previous)
		b.WriteString("\n\n")
	}

	b.WriteString("## Nouveaux messages à intégrer\n\n")
	for _, m := range batch {
		fmt.Fprintf(&b, "%s (%s): %s\n", m.Role, m.PrincipalID, redactProfileLinks(m.Content))
	}

	fmt.Fprintf(&b, "\nLongueur maximale du résumé : %d caractères.", c.maxSummaryChars)

	response, err := client.ChatCompletion(ctx,
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
