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
	"time"

	"github.com/bornholm/go-courier"

	"github.com/bornholm/automata/internal/action"
	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/audio"
	"github.com/bornholm/automata/internal/authorization"
	"github.com/bornholm/automata/internal/backup"
	"github.com/bornholm/automata/internal/billing"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/consolidation"
	"github.com/bornholm/automata/internal/conversation"
	"github.com/bornholm/automata/internal/identity"
	"github.com/bornholm/automata/internal/ingress"
	"github.com/bornholm/automata/internal/llmclients"
	"github.com/bornholm/automata/internal/mcp"
	"github.com/bornholm/automata/internal/media"
	"github.com/bornholm/automata/internal/memory"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/observability"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/platform"
	"github.com/bornholm/automata/internal/plugin"
	"github.com/bornholm/automata/internal/privacy"
	"github.com/bornholm/automata/internal/reminder"
	"github.com/bornholm/automata/internal/scheduler"
	"github.com/bornholm/automata/internal/secretbox"
	"github.com/bornholm/automata/internal/skills"
	"github.com/bornholm/automata/internal/usage"
	"github.com/bornholm/automata/internal/web"
)

// mainAgentName est l'identifiant conventionnel de l'agent généraliste dans
// cfg.Agents (voir PLAN.md §12 et internal/config/testdata/valid/config.yaml).
const mainAgentName = "main"

// Run démarre les services applicatifs et bloque jusqu'à l'annulation du
// contexte fourni, puis s'arrête proprement. cfg doit avoir été chargée et
// validée (voir config.Load).
func Run(ctx context.Context, logger *slog.Logger, cfg *config.Config) error {
	logger.InfoContext(ctx, "automata starting")

	// Journal des étapes de démarrage. Le serveur web n'écoute qu'à la
	// toute fin : sans ces jalons, un démarrage qui traîne ou qui reste
	// bloqué ne se distingue pas d'un processus mort, et le diagnostic
	// se réduit à deviner. Chaque étape porte sa durée.
	startedAt := time.Now()
	step := func(name string) {
		logger.InfoContext(ctx, "registry: étape de démarrage",
			"step", name, "elapsed", time.Since(startedAt).Round(time.Millisecond).String())
	}

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

	// Avant d'ouvrir quoi que ce soit : le verrou d'instance. Sans lui,
	// un second processus sur les mêmes données s'arrête plus loin, à
	// l'ouverture de l'index bleve, dans une attente silencieuse et sans
	// fin.
	lock, err := lockDataDir(cfg.Storage.Application.Path)
	if err != nil {
		return err
	}
	defer lock.release()

	db, err := persistence.OpenWithEncryption(ctx, cfg.Storage.Application, cfg.Storage.EncryptionKey)
	if err != nil {
		return fmt.Errorf("registry: ouverture de la persistance: %w", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			logger.ErrorContext(ctx, "registry: échec de la fermeture de la persistance", "error", err)
		}
	}()

	// La comptabilité d'usage (internal/usage) voyage par le contexte : le
	// recorder attaché ici est hérité par tous les contextes dérivés —
	// pipelines ingress, scheduler, dispatcher de rappels, consolidateur —
	// et lu par les décorateurs de clients LLM au moment de chaque appel.
	ctx = usage.ContextWithRecorder(ctx, newDBUsageRecorder(db, logger, metrics))

	// Tenants enregistrés en ligne (socle SaaS) : résolution d'identité de
	// repli, rôles des membres et génération de liens de profil. La
	// configuration reste prioritaire partout.
	step("persistance ouverte")

	// Bibliothèque de compétences : semis des compétences embarquées (les
	// noms absents seulement) puis fournisseur relu par tour. Un semis en
	// échec est fatal : c'est un défaut du dépôt, pas une condition
	// d'exploitation.
	if err := skills.Seed(ctx, db, logger); err != nil {
		return fmt.Errorf("registry: %w", err)
	}
	skillsProvider := skills.NewProvider(db, logger)

	step("compétences semées")

	tenants := newTenantSource(db, cfg.Web.BaseURL)

	resolver, err := identity.NewResolver(cfg)
	if err != nil {
		return fmt.Errorf("registry: construction du résolveur d'identité: %w", err)
	}
	resolver = resolver.WithDynamicSource(tenants)

	// Chiffrement des configurations de comptes de messagerie : les
	// comptes vivent en base et se gèrent dans l'administration — plus
	// aucun semis depuis le fichier.
	secrets, err := secretbox.New(cfg.Web.SessionSecret)
	if err != nil && cfg.Web.Enabled {
		return fmt.Errorf("registry: dérivation de la clé de chiffrement: %w", err)
	}

	// Catalogue des modèles : LA source des clients ET des rôles — quel
	// modèle sert quel agent et quelle fonction. Il n'y a pas de semis :
	// une instance neuve se règle dans l'administration, et chaque rôle
	// sans modèle se signale à l'usage plutôt que de se taire.
	llmBox, err := secretbox.NewLLMClients(cfg.Web.SessionSecret)
	if err != nil {
		return fmt.Errorf("registry: dérivation de la clé du catalogue de modèles: %w", err)
	}

	modelStore := llmclients.NewStore(db, llmBox)
	pool := llmclients.NewPool(modelStore, agent.BuildLLMClient, logger)
	images := llmclients.NewImagePool(modelStore, agent.BuildImageGenerationClient, logger)
	var clientResolver agent.ClientResolver = llmclients.NewResolver(pool, modelStore, logger).
		WithImagePool(images)

	step("catalogue de modèles prêt")

	memRes, err := buildMemory(ctx, cfg, modelStore)
	if err != nil {
		return fmt.Errorf("registry: construction de la mémoire: %w", err)
	}
	defer memRes.close(logger)

	step("mémoire prête")

	mcpManager := mcp.NewManager(cfg, logger).WithMetrics(metrics)
	defer func() {
		if err := mcpManager.Close(); err != nil {
			logger.ErrorContext(ctx, "registry: échec de la fermeture du gestionnaire mcp", "error", err)
		}
	}()

	authorizer := authorization.NewAuthorizer(cfg).WithMemberRoles(tenants)

	// Système de plugins : découverte et lancement des sous-processus.
	// Un échec de chargement n'est jamais fatal pour l'instance — le
	// plugin fautif est ignoré et visible en erreur dans l'administration.
	var pluginManager *plugin.Manager
	if cfg.Plugins.Enabled {
		pluginBox, err := secretbox.NewPlugins(cfg.Web.SessionSecret)
		if err != nil {
			return fmt.Errorf("registry: dérivation de la clé des plugins: %w", err)
		}

		// Clé du casier personnel, distincte de celle des secrets de
		// plugins. Son absence n'empêche pas de démarrer : le magasin
		// d'objets ordinaire continue de servir, seul le casier est
		// indisponible — et il le dit alors clairement à l'usage.
		objectBox, err := secretbox.NewPluginObjects(cfg.Web.SessionSecret)
		if err != nil {
			logger.WarnContext(ctx, "registry: casier personnel indisponible", "error", err)
			objectBox = nil
		}

		agentNames := make([]string, 0, len(cfg.Agents))
		for name := range cfg.Agents {
			agentNames = append(agentNames, name)
		}

		pluginHost := plugin.NewHostService(db, pluginBox).
			WithObjectStore(cfg.Web.BaseURL, plugin.ObjectStoreLimits{
				MaxObjectBytes: int64(cfg.Plugins.ObjectStoreMaxObjectBytes),
				MaxMemberBytes: int64(cfg.Plugins.ObjectStoreMaxMemberBytes),
			}).
			WithObjectSealing(objectBox).
			// Les deux fabriques viennent du paquet web : le secret de
			// signature ne traverse jamais internal/plugin.
			WithPreviewMinter(web.DraftPreviewMinter(cfg.Web.SessionSecret, cfg.Web.BaseURL)).
			WithFileLinkMinter(web.FileLinkMinter(cfg.Web.SessionSecret, cfg.Web.BaseURL))
		pluginManager = plugin.NewManager(cfg.Plugins, pluginHost, agentNames)
		if err := pluginManager.Start(ctx); err != nil {
			logger.WarnContext(ctx, "registry: démarrage des plugins incomplet", "error", err)
		}
		defer pluginManager.Shutdown()

		step("plugins chargés")
	}

	// Provider des sous-agents de plugins et exécuteurs de leurs actions
	// confirmées. Nil quand le système est désactivé : le registre
	// d'agents est nil-safe.
	pluginProvider, err := newPluginSpecialistProvider(cfg, pluginManager, db, skillsProvider, clientResolver, logger)
	if err != nil {
		return fmt.Errorf("registry: client llm des sous-agents de plugins: %w", err)
	}
	if pluginManager != nil {
		// Activer un plugin pour une organisation accorde à ses membres
		// les permissions du domaine du plugin — c'est la décision
		// d'administration ; la confirmation humaine reste la porte des
		// écritures.
		authorizer = authorizer.WithPluginDomains(pluginDomainSource{manager: pluginManager, db: db})
	}

	actionOpts := []action.Option{action.WithAuditEvents(persistence.NewAuditEventRepository()), action.WithLogger(logger), action.WithMetrics(metrics)}
	if memRes.store != nil {
		actionOpts = append(actionOpts, action.WithMemoryStore(memRes.store))
	}
	if pluginManager != nil {
		for _, name := range pluginManager.Loaded() {
			actionOpts = append(actionOpts, action.WithExecutor("plugin:"+name,
				plugin.NewActionExecutor(pluginManager, db, name)))
		}
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

	step("moteur d'actions prêt")

	handler, agents, taskAgents, err := buildConversationHandler(cfg, db, authorizer, memRes.store, mcpManager, actionEngine, tenants, pluginProvider, newPluginEventStoreResolver(pluginManager, db), skillsProvider, clientResolver, modelStore, metrics, logger)
	if err != nil {
		return fmt.Errorf("registry: construction de l'agent généraliste: %w", err)
	}

	// Le gestionnaire porte un pipeline d'ingestion par compte actif, et
	// les fournisseurs que le scheduler et les rappels utilisent pour
	// émettre : tous suivent donc les ajouts et retraits à chaud.
	platforms := platform.NewManager(db, secrets,
		func(id, providerType string, providerConfig map[string]any, qrHandler func(code string, linked bool)) (courier.Provider, error) {
			return buildManagedProvider(id, providerType, providerConfig, qrHandler)
		},
		func(pipelineCtx context.Context, id string, provider courier.Provider) error {
			return ingress.NewPipeline(id, provider, resolver, db, handler, logger, metrics).
				WithCoalesceWindow(cfg.Courier.EffectiveCoalesceWindow()).
				// Un inconnu porteur d'un jeton valide se rattache lui-même
				// (socle SaaS) : sans serveur web configuré, aucun jeton n'a
				// pu être émis, la liaison reste donc inutile.
				WithLinking(cfg.Web.Enabled).
				Run(pipelineCtx)
		},
		logger)

	sched := scheduler.NewScheduler(cfg, scheduler.RealClock{}, db, agents, platforms, actionEngine, logger).WithMetrics(metrics)

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()

		if err := platforms.Run(ctx); err != nil && ctx.Err() == nil {
			logger.ErrorContext(ctx, "registry: gestionnaire de plateformes arrêté en erreur", "error", err)
		}
	}()

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
	// Le même dispatcher porte les tâches planifiées (schedule_task) : à
	// leur échéance, l'exécuteur fait tourner l'agent qui les a créées et
	// c'est sa réponse qui est délivrée.
	reminderDispatcher := reminder.NewDispatcher(db, platforms, logger, metrics).
		WithTaskRunner(agent.NewTaskRunner(cfg, taskAgents, logger))

	// Routeur des déclencheurs de plugins : un flux par plugin qui en
	// déclare, exécution du sous-agent avec plan confirmable, réponse sur
	// le canal privé du membre désigné. Le service hôte gagne du même
	// coup l'envoi de notifications applicatives (Notify).
	if pluginManager != nil {
		triggerRouter := plugin.NewTriggerRouter(pluginManager, db, &pluginTriggerRunner{
			provider: pluginProvider,
			actions:  actionEngine,
			logger:   logger,
		}, platforms, logger)
		pluginManager.HostService().WithNotifier(triggerRouter)

		wg.Add(1)
		go func() {
			defer wg.Done()
			triggerRouter.Run(ctx)
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()

		if err := reminderDispatcher.Run(ctx); err != nil && ctx.Err() == nil {
			logger.ErrorContext(ctx, "registry: dispatcher de rappels arrêté en erreur", "error", err)
		}
	}()

	// Réorganisation périodique de la mémoire (memory.consolidation) :
	// indépendante du scheduler et du dispatcher de rappels — une tâche de
	// maintenance interne, sans agent ni livraison sur un canal.
	if cfg.Memory.Consolidation.Enabled {
		consolidationCfg, clientName, roleOK := roleClientConfig(ctx, modelStore, llmclients.RoleConsolidation)
		switch {
		case memRes.store == nil:
			logger.Warn("registry: memory.consolidation activée sans système de mémoire configuré, consolidation désactivée")
		case !roleOK:
			// Le modèle se règle en ligne : la consolidation reprendra au
			// redémarrage suivant le réglage.
			logger.Warn("registry: consolidation désactivée, aucun modèle configuré",
				"remède", "réglez le rôle consolidation dans l'administration (Modèles)")
		default:
			consolidationClient, err := agent.BuildLLMClient(ctx, consolidationCfg)
			if err != nil {
				return fmt.Errorf("registry: construction du client de consolidation %q: %w", clientName, err)
			}

			consolidator, err := consolidation.New(db, memRes.store, consolidationClient, cfg.Memory.Consolidation, logger, metrics)
			if err != nil {
				return fmt.Errorf("registry: construction du consolidateur mémoire: %w", err)
			}

			if cfg.Memory.Consolidation.Reflection.Enabled {
				consolidator = consolidator.WithEpisodes(memRes.store, cfg.Memory.Consolidation.Reflection)
				if !cfg.Conversation.Compaction.RecordEpisodes {
					// Pas une erreur : des épisodes enregistrés par le
					// passé restent réfléchissables, mais sans
					// record_episodes aucun nouveau n'arrivera jamais.
					logger.Warn("registry: memory.consolidation.reflection activée sans conversation.compaction.record_episodes, aucun nouvel épisode ne sera enregistré")
				}
			}

			wg.Add(1)
			go func() {
				defer wg.Done()

				if err := consolidator.Run(ctx); err != nil && ctx.Err() == nil {
					logger.ErrorContext(ctx, "registry: consolidateur mémoire arrêté en erreur", "error", err)
				}
			}()
		}
	}

	// Facturation : conversion de la consommation en débits de crédits et
	// allocations mensuelles des organisations offertes (internal/billing).
	// Sans serveur web configuré, aucun portefeuille n'existe : inutile.
	if cfg.Web.Enabled {
		debiter := billing.New(db, cfg, logger, metrics).
			// L'organisation est prévenue dans sa conversation avant la
			// coupure : découvrir l'arrêt du service est bien pire que
			// recevoir un avertissement.
			WithNotifier(newOrgNotifier(db, cfg, platforms, tenants, logger))

		wg.Add(1)
		go func() {
			defer wg.Done()

			if err := debiter.Run(ctx); err != nil && ctx.Err() == nil {
				logger.ErrorContext(ctx, "registry: facturation arrêtée en erreur", "error", err)
			}
		}()
	}

	// Sauvegardes périodiques des bases SQLite : conversations,
	// portefeuilles, souvenirs et sessions de messagerie. Sans elles, une
	// panne de disque coûte tout l'historique des organisations servies.
	if cfg.Backup.Enabled {
		sources := []backup.Source{
			{Name: "app", Path: cfg.Storage.Application.Path, Driver: cfg.Storage.Application.Driver},
		}
		if cfg.Memory.Store.Path != "" {
			sources = append(sources, backup.Source{Name: "memory", Path: cfg.Memory.Store.Path})
		}
		for name, path := range cfg.Backup.ExtraPaths {
			sources = append(sources, backup.Source{Name: name, Path: path})
		}

		backupRunner := backup.New(backup.Options{
			Directory: cfg.Backup.Directory,
			Interval:  cfg.Backup.EffectiveInterval(),
			Keep:      cfg.Backup.EffectiveKeep(),
			Sources:   sources,
		}, logger)

		wg.Add(1)
		go func() {
			defer wg.Done()

			if err := backupRunner.Run(ctx); err != nil && ctx.Err() == nil {
				logger.ErrorContext(ctx, "registry: sauvegardes arrêtées en erreur", "error", err)
			}
		}()
	}

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

	// Serveur web d'administration et de profil (socle SaaS, maquettes P1).
	if cfg.Web.Enabled {
		mailSender, err := web.NewSMTPSender(cfg, db, secrets)
		if err != nil {
			return fmt.Errorf("registry: construction de l'expéditeur de courriels: %w", err)
		}

		privacyService := privacy.New(db, memRes.store, func(privacyCtx context.Context, q persistence.Querier) float64 {
			return cfg.Web.Credits.EffectiveUSDPerCredit()
		}).WithPersonalChannels(func(memberID string) []string {
			// Les canaux privés déclarés en configuration : sans eux,
			// l'export d'un membre issu du YAML serait vide et sa
			// suppression n'effacerait rien.
			var conversations []string
			for _, ch := range cfg.Channels {
				if ch.Kind == config.ChannelKindPrivate && ch.PrincipalID == memberID {
					conversations = append(conversations, ch.Provider+":"+ch.ChannelID)
				}
			}
			return conversations
		})

		webServer := web.NewServer(cfg, db, mailSender, logger).
			WithPlatformManager(platforms).
			// Un paiement se confirme là où il a été décidé : dans la
			// conversation, pas seulement sur l'écran de retour que le
			// client a peut-être déjà fermé.
			WithPurchaseNotifier(newOrgNotifier(db, cfg, platforms, tenants, logger)).
			WithPlatformValidator(func(providerType string, providerConfig map[string]any) error {
				_, err := buildManagedProvider("", providerType, providerConfig, nil)
				return err
			}).
			WithPrivacy(privacyService)
		if pluginManager != nil {
			webServer = webServer.WithPluginManager(pluginManager)
		}

		wg.Add(1)
		go func() {
			defer wg.Done()

			step("serveur web en écoute")

			if err := webServer.Run(ctx); err != nil {
				logger.ErrorContext(ctx, "registry: serveur web arrêté en erreur", "error", err)
			}
		}()
	}

	<-ctx.Done()

	logger.InfoContext(ctx, "automata stopping")

	wg.Wait()

	return nil
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
func buildConversationHandler(cfg *config.Config, db *persistence.DB, authorizer *authorization.Authorizer, memStore *memory.AmoxtliStore, mcpManager *mcp.Manager, actionEngine *action.Engine, tenants *tenantSource, pluginProvider agent.PluginSpecialistProvider, eventStores agent.EventStoreResolver, skillsProvider agent.SkillsProvider, clientResolver agent.ClientResolver, modelStore *llmclients.Store, metrics *observability.Metrics, logger *slog.Logger) (ingress.Handler, *agent.Registry, *agent.Registry, error) {
	memoryTools := buildMemoryTools(cfg, authorizer, memStore, metrics)

	// Outil open_profile_link : disponible dès que le serveur web est
	// configuré (sans base_url, aucun lien n'est composable).
	profileTools := agent.ProfileTools{Metrics: metrics}
	if cfg.Web.Enabled && cfg.Web.BaseURL != "" {
		profileTools.Generator = tenants
		profileTools.Enabled = true

		// L'outil reste recroisé avec agents.<nom>.profile_link. Si aucun
		// orchestrateur ne le déclare, personne ne peut obtenir son lien
		// depuis une conversation : à la question « donne-moi le lien vers
		// mon compte », l'assistant répondra qu'il n'a pas l'outil, sans
		// que rien n'ait l'air en panne. Le signaler au démarrage est la
		// seule occasion de rendre cet oubli visible.
		if !anyAgentExposesProfileLink(cfg) {
			logger.Warn("registry: aucun agent n'expose open_profile_link alors que le web est actif",
				"remède", "ajoutez profile_link: true à l'agent orchestrateur")
		}
	}

	// Les outils de rappels partagent la base applicative et l'Authorizer de
	// l'instance ; agent.NewRegistryWithMemory ne les attache qu'aux agents
	// déclarant reminders: true.
	reminderTools := agent.ReminderTools{
		DB:         db,
		Repo:       persistence.NewReminderRepository(db.Cipher()),
		Authorizer: authorizer,
		Metrics:    metrics,
		// Un exécuteur de tâches est câblé sur le dispatcher juste après
		// (voir Run) : les agents qui déclarent scheduled_tasks peuvent donc
		// réellement en programmer.
		Tasks: true,
		// Un plugin actif peut tenir le magasin des rappels d'un membre à
		// la place de la table — un agenda CalDAV, par exemple. Les tâches
		// planifiées n'y vont jamais : voir ReminderTools.Events.
		Events: eventStores,
	}

	agents, err := agent.NewRegistryWithMemory(cfg, memoryTools, reminderTools, profileTools, tenants, mcpManager, pluginProvider, skillsProvider, clientResolver, metrics, logger)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("construction du registre d'agents: %w", err)
	}

	// Registre distinct pour l'exécution des tâches planifiées : mêmes
	// agents, mêmes outils MCP, même mémoire, mais AUCUN outil de
	// programmation (ReminderTools nulle).
	//
	// Ce n'est pas une précaution théorique : à l'échéance, un agent qui voit
	// schedule_task devant une consigne rédigée comme une demande
	// (« Préparer un bulletin météo… ») la REPROGRAMME au lieu de
	// l'exécuter — observé en production. Retirer l'outil rend la confusion
	// impossible, là où une consigne de prompt ne ferait que la rendre moins
	// probable ; et cela ferme du même coup la boucle d'une tâche qui se
	// reprogrammerait indéfiniment.
	taskAgents, err := agent.NewRegistryWithMemory(cfg, memoryTools, agent.ReminderTools{}, profileTools, tenants, mcpManager, pluginProvider, skillsProvider, clientResolver, metrics, logger)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("construction du registre d'agents des tâches planifiées: %w", err)
	}

	mainAgent, err := agents.Get(mainAgentName)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("récupération de l'agent %q: %w", mainAgentName, err)
	}

	audioCfg := audio.Config{}
	var transcriber audio.Transcriber

	if cfg.Audio.Enabled {
		// Le modèle se règle en ligne (rôle « transcription »), construit
		// au démarrage : sans lui, les notes vocales sont refusées avec le
		// message habituel, jamais un refus de démarrer.
		transcriptionCfg, clientName, ok := roleClientConfig(context.Background(), modelStore, llmclients.RoleTranscription)
		if !ok {
			logger.Warn("registry: transcription désactivée, aucun modèle configuré",
				"remède", "réglez le rôle transcription dans l'administration (Modèles), puis redémarrez")
		} else {
			transcriptionClient, err := agent.BuildTranscriptionClient(context.Background(), transcriptionCfg)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("audio: construction du client de transcription %q: %w", clientName, err)
			}

			audioCfg = audio.Config{
				Enabled: true,
				MaxSize: int64(cfg.Audio.MaxSize.Bytes()),
				Timeout: cfg.Audio.Timeout.Duration(),
			}
			transcriber = audio.NewGenAITranscriber(transcriptionClient)
		}
	}

	handler := conversation.NewHandler(db, mainAgent, actionEngine, cfg.Conversation.HistoryLimit, audioCfg, transcriber, cfg.Audio.PersistTranscription, metrics).
		WithAttachments(media.Config{
			Enabled:       cfg.Attachments.Enabled,
			MaxSize:       int64(cfg.Attachments.MaxSize.Bytes()),
			MaxCount:      cfg.Attachments.MaxCount,
			AcceptedTypes: cfg.Attachments.AcceptedTypes,
			ToolTypes:     cfg.Attachments.ToolTypes,
			MaxToolSize:   int64(cfg.Attachments.MaxToolSize.Bytes()),
			MaxHistory:    cfg.Attachments.MaxHistory,
			MaxReply:      cfg.Attachments.MaxReply,
		})

	// Pause du service à solde épuisé (internal/conversation/pause.go) :
	// activée avec le serveur web, qui est ce qui donne un portefeuille aux
	// organisations et un moyen de le recharger.
	if cfg.Web.Enabled {
		handler = handler.WithBilling(tenants)
	}

	if cfg.Conversation.Compaction.Enabled {
		// Le modèle se règle en ligne (rôle « compaction »), résolu à
		// CHAQUE compaction par le catalogue — défaut d'instance, surchargé
		// par organisation. Sans modèle configuré, la compaction d'un tour
		// est sautée avec une trace : l'historique grossit, rien ne casse.
		compactor := conversation.NewCompactor(db, nil, cfg.Conversation.HistoryLimit, cfg.Conversation.Compaction.MaxSummaryChars, logger, metrics).
			WithClientResolver(clientResolver)

		if cfg.Conversation.Compaction.ExtractFacts {
			if memStore != nil {
				compactor = compactor.WithMemoryStore(memStore, cfg.Conversation.Compaction.MaxFacts)
			} else {
				// Pas une erreur fatale : la compaction garde toute sa valeur
				// sans extraction, mais l'écart avec la configuration doit se
				// voir dans les journaux.
				logger.Warn("conversation: extract_facts activé sans système de mémoire configuré, extraction désactivée")
			}
		}

		if cfg.Conversation.Compaction.RecordEpisodes {
			if memStore != nil {
				// Les épisodes sont restitués au modèle par la recherche
				// d'historique : les répliques y sont étiquetées par nom
				// affiché, jamais par identifiant interne.
				compactor = compactor.WithEpisodeStore(memStore, func(principalID model.PrincipalID) string {
					for _, p := range cfg.Identities.Principals {
						if p.ID == string(principalID) {
							return p.DisplayName
						}
					}
					return ""
				})
			} else {
				logger.Warn("conversation: record_episodes activé sans système de mémoire configuré, mémoire épisodique désactivée")
			}
		}

		handler = handler.WithCompactor(compactor)
	}

	return handler, agents, taskAgents, nil
}

// anyAgentExposesProfileLink indique si au moins un agent déclare
// profile_link.
func anyAgentExposesProfileLink(cfg *config.Config) bool {
	for _, agentCfg := range cfg.Agents {
		if agentCfg.ProfileLink {
			return true
		}
	}

	return false
}
