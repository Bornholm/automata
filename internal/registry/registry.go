// Package registry assemble les services applicatifs d'Automata
// (configuration, ingress, scheduler, agents, mémoire, persistance) et
// pilote leur cycle de vie.
//
// Depuis la Phase 6, le transport texte est branché sur un agent
// généraliste minimal adossé à GenAI (voir internal/agent,
// internal/conversation) : persistance, résolution d'identité et un
// pipeline ingress par fournisseur Courier déclaré. Les phases suivantes y
// brancheront progressivement les autres composants (délégation, mémoire,
// MCP, scheduler...).
package registry

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/provider/whatsapp"

	"github.com/bornholm/automata/internal/action"
	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/audio"
	"github.com/bornholm/automata/internal/authorization"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/conversation"
	"github.com/bornholm/automata/internal/identity"
	"github.com/bornholm/automata/internal/ingress"
	"github.com/bornholm/automata/internal/mcp"
	"github.com/bornholm/automata/internal/media"
	"github.com/bornholm/automata/internal/memory"
	"github.com/bornholm/automata/internal/observability"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/reminder"
	"github.com/bornholm/automata/internal/scheduler"
)

// mainAgentName est l'identifiant conventionnel de l'agent généraliste dans
// cfg.Agents (voir PLAN.md §12 et internal/config/testdata/valid/config.yaml).
const mainAgentName = "main"

// Run démarre les services applicatifs et bloque jusqu'à l'annulation du
// contexte fourni, puis s'arrête proprement. cfg doit avoir été chargée et
// validée (voir config.Load).
func Run(ctx context.Context, logger *slog.Logger, cfg *config.Config) error {
	logger.InfoContext(ctx, "automata starting")

	if err := scheduler.ValidateSchedules(cfg); err != nil {
		return fmt.Errorf("registry: validation des schedules: %w", err)
	}

	// Registre de métriques et indicateur de disponibilité (readiness),
	// PLAN.md §14.3 et Phase 20. Construits inconditionnellement (coût
	// négligeable, purement en mémoire) : seul le démarrage du serveur HTTP
	// d'observabilité, plus bas, dépend de cfg.Observability.Enabled. ready
	// reste à sa valeur zéro (non prêt) tant que la persistance et les
	// pipelines ingress/scheduler ne sont pas tous démarrés.
	metrics := observability.NewMetrics()
	ready := &observability.Ready{}

	db, err := persistence.Open(ctx, cfg.Storage.Application)
	if err != nil {
		return fmt.Errorf("registry: ouverture de la persistance: %w", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			logger.ErrorContext(ctx, "registry: échec de la fermeture de la persistance", "error", err)
		}
	}()

	resolver, err := identity.NewResolver(cfg)
	if err != nil {
		return fmt.Errorf("registry: construction du résolveur d'identité: %w", err)
	}

	providers, err := buildCourierProviders(cfg)
	if err != nil {
		return fmt.Errorf("registry: construction des fournisseurs courier: %w", err)
	}

	memRes, err := buildMemory(ctx, cfg)
	if err != nil {
		return fmt.Errorf("registry: construction de la mémoire: %w", err)
	}
	defer memRes.close(logger)

	mcpManager := mcp.NewManager(cfg, logger).WithMetrics(metrics)
	defer func() {
		if err := mcpManager.Close(); err != nil {
			logger.ErrorContext(ctx, "registry: échec de la fermeture du gestionnaire mcp", "error", err)
		}
	}()

	authorizer := authorization.NewAuthorizer(cfg)

	actionOpts := []action.Option{action.WithAuditEvents(persistence.NewAuditEventRepository()), action.WithLogger(logger), action.WithMetrics(metrics)}
	if memRes.store != nil {
		actionOpts = append(actionOpts, action.WithMemoryStore(memRes.store))
	}
	actionEngine := action.NewEngine(db, authorizer, mcpManager, cfg, actionOpts...)

	// Récupération des plans/actions interrompus par un crash du processus
	// précédent (PLAN.md Phase 18), AVANT de traiter le moindre message ou
	// tick de scheduler : sinon une confirmation entrante ou une occurrence
	// planifiée pourrait cohabiter avec un plan resté bloqué en "executing".
	// Choix délibéré : une erreur ici n'est pas fatale au démarrage — elle
	// est journalisée clairement et signale un état à examiner via
	// "automata admin inspect", mais bloquer tout le processus reviendrait à
	// transformer un problème localisé (quelques plans bloqués, déjà
	// inertes et sans effet supplémentaire tant qu'ils ne sont pas
	// confirmables) en une indisponibilité totale du service.
	if err := actionEngine.RecoverInterrupted(ctx); err != nil {
		logger.ErrorContext(ctx, "registry: échec de la récupération des plans d'actions interrompus", "error", err)
	}

	handler, agents, err := buildConversationHandler(cfg, db, authorizer, memRes.store, mcpManager, actionEngine, metrics)
	if err != nil {
		return fmt.Errorf("registry: construction de l'agent généraliste: %w", err)
	}

	sched := scheduler.NewScheduler(cfg, scheduler.RealClock{}, db, agents, providers, actionEngine, logger).WithMetrics(metrics)

	var wg sync.WaitGroup

	for name, provider := range providers {
		pipeline := ingress.NewPipeline(name, provider, resolver, db, handler, logger, metrics).
			WithCoalesceWindow(cfg.Courier.EffectiveCoalesceWindow())

		wg.Add(1)
		go func(name string, pipeline *ingress.Pipeline) {
			defer wg.Done()

			if err := pipeline.Run(ctx); err != nil && ctx.Err() == nil {
				logger.ErrorContext(ctx, "registry: pipeline ingress arrêté en erreur", "provider", name, "error", err)
			}
		}(name, pipeline)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()

		if err := sched.Run(ctx); err != nil && ctx.Err() == nil {
			logger.ErrorContext(ctx, "registry: scheduler arrêté en erreur", "error", err)
		}
	}()

	// Livraison des rappels ponctuels créés conversationnellement
	// (internal/reminder) : indépendante du scheduler, qui ne connaît que
	// les schedules récurrents de la configuration.
	reminderDispatcher := reminder.NewDispatcher(db, providers, logger, metrics)

	wg.Add(1)
	go func() {
		defer wg.Done()

		if err := reminderDispatcher.Run(ctx); err != nil && ctx.Err() == nil {
			logger.ErrorContext(ctx, "registry: dispatcher de rappels arrêté en erreur", "error", err)
		}
	}()

	// La persistance est ouverte et les pipelines ingress/scheduler viennent
	// d'être démarrés (goroutines lancées ci-dessus) : le service peut
	// désormais répondre "prêt" (PLAN.md Phase 20, readiness). Les pipelines
	// eux-mêmes ne signalent pas individuellement leur démarrage effectif
	// (ex : connexion établie au fournisseur Courier) : ready reflète que le
	// processus a fini son câblage interne, pas que chaque dépendance
	// externe est déjà joignable — cohérent avec la distinction attendue
	// entre readiness applicative et liveness du processus.
	ready.Set(true)

	if cfg.Observability.Enabled {
		obsServer := observability.NewServer(cfg.Observability.Addr, metrics, ready, logger)

		wg.Add(1)
		go func() {
			defer wg.Done()

			if err := obsServer.Run(ctx); err != nil {
				logger.ErrorContext(ctx, "registry: serveur d'observabilité arrêté en erreur", "error", err)
			}
		}()
	}

	<-ctx.Done()

	logger.InfoContext(ctx, "automata stopping")

	wg.Wait()

	return nil
}

// buildCourierProviders construit un courier.Provider réel pour chaque
// fournisseur déclaré dans cfg.Courier.Providers. Seul le type "whatsapp"
// est supporté à ce stade (voir PLAN.md, Phase 5).
func buildCourierProviders(cfg *config.Config) (map[string]courier.Provider, error) {
	providers := make(map[string]courier.Provider, len(cfg.Courier.Providers))

	for name, cp := range cfg.Courier.Providers {
		switch cp.Type {
		case "whatsapp":
			sessionPath, ok := cp.Extra["session_path"].(string)
			if !ok || sessionPath == "" {
				return nil, fmt.Errorf("fournisseur courier %q: champ session_path requis et non vide", name)
			}

			providers[name] = whatsapp.NewProvider(whatsapp.WithDBPath(sessionPath))
		default:
			return nil, fmt.Errorf("fournisseur courier %q: type %q non supporté", name, cp.Type)
		}
	}

	return providers, nil
}

// buildConversationHandler construit le ingress.Handler de l'agent
// généraliste "main" (PLAN.md Phase 6, Phase 7) : depuis la Phase 7, la
// construction du client LLM et la composition du system prompt (règles
// invariantes + personnalité configurée + capacités) de CHAQUE agent
// déclaré dans la configuration sont déléguées à agent.NewRegistry
// (internal/agent/registry.go), pas seulement celles de "main". Cette
// fonction se contente d'en extraire l'agent "main" pour le brancher sur un
// conversation.Handler qui persiste l'historique dans db.
//
// Depuis la Phase 9 (PLAN.md §3.4), elle construit également le
// audio.Transcriber utilisé pour transcrire les notes vocales lorsque
// cfg.Audio.Enabled est vrai, à partir du client LLM référencé par
// cfg.Audio.TranscriptionClient. Rien n'est construit si l'audio est
// désactivé : le comportement existant (message vide transmis tel quel) est
// préservé.
// Depuis la Phase 16, elle retourne également le *agent.Registry construit,
// réutilisé tel quel par internal/scheduler pour exécuter les tâches
// planifiées (PLAN.md §11) : un seul registre d'agents par instance,
// jamais reconstruit.
func buildConversationHandler(cfg *config.Config, db *persistence.DB, authorizer *authorization.Authorizer, memStore *memory.AmoxtliStore, mcpManager *mcp.Manager, actionEngine *action.Engine, metrics *observability.Metrics) (ingress.Handler, *agent.Registry, error) {
	memoryTools := buildMemoryTools(cfg, memStore, metrics)

	// Les outils de rappels partagent la base applicative et l'Authorizer de
	// l'instance ; agent.NewRegistryWithMemory ne les attache qu'aux agents
	// déclarant reminders: true.
	reminderTools := agent.ReminderTools{
		DB:         db,
		Repo:       persistence.NewReminderRepository(),
		Authorizer: authorizer,
		Metrics:    metrics,
	}

	agents, err := agent.NewRegistryWithMemory(cfg, memoryTools, reminderTools, mcpManager, metrics)
	if err != nil {
		return nil, nil, fmt.Errorf("construction du registre d'agents: %w", err)
	}

	mainAgent, err := agents.Get(mainAgentName)
	if err != nil {
		return nil, nil, fmt.Errorf("récupération de l'agent %q: %w", mainAgentName, err)
	}

	audioCfg := audio.Config{}
	var transcriber audio.Transcriber

	if cfg.Audio.Enabled {
		llmClientCfg, ok := cfg.LLMClients[cfg.Audio.TranscriptionClient]
		if !ok {
			return nil, nil, fmt.Errorf("audio: client llm %q (référencé par audio.transcription_client) introuvable dans la configuration", cfg.Audio.TranscriptionClient)
		}

		transcriptionClient, err := agent.BuildTranscriptionClient(context.Background(), llmClientCfg)
		if err != nil {
			return nil, nil, fmt.Errorf("audio: construction du client de transcription %q: %w", cfg.Audio.TranscriptionClient, err)
		}

		audioCfg = audio.Config{
			Enabled: true,
			MaxSize: int64(cfg.Audio.MaxSize.Bytes()),
			Timeout: cfg.Audio.Timeout.Duration(),
		}
		transcriber = audio.NewGenAITranscriber(transcriptionClient)
	}

	handler := conversation.NewHandler(db, mainAgent, actionEngine, 0, audioCfg, transcriber, cfg.Audio.PersistTranscription, metrics).
		WithAttachments(media.Config{
			Enabled:       cfg.Attachments.Enabled,
			MaxSize:       int64(cfg.Attachments.MaxSize.Bytes()),
			MaxCount:      cfg.Attachments.MaxCount,
			AcceptedTypes: cfg.Attachments.AcceptedTypes,
			MaxHistory:    cfg.Attachments.MaxHistory,
			MaxReply:      cfg.Attachments.MaxReply,
		})

	return handler, agents, nil
}
