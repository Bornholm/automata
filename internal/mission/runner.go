// Package mission réveille les dossiers au long cours (table missions) :
// à chaque échéance, l'agent créateur relit le journal de bord du dossier,
// avance ce qui peut l'être, note ce qu'il laisse et se rendort.
//
// Ce que le runner garantit, et que le modèle ne décide jamais :
//
//   - l'identité d'exécution est celle du CRÉATEUR de la mission, figée à
//     la création, la portée relue dans la configuration du canal (patron
//     agent.TaskRunner) ;
//   - la mission mise à jour est celle du réveil : l'outil update_mission
//     est lié par l'identité (model.ExecutionIdentity.MissionID), pas par
//     un paramètre du modèle ;
//   - les actions proposées pendant le réveil deviennent un plan
//     confirmable dans la conversation d'origine (patron
//     scheduler.proposeActionPlan) — jamais une exécution directe : une
//     mission propose, l'humain confirme.
package mission

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bornholm/go-courier"
	"github.com/google/uuid"

	"github.com/bornholm/automata/internal/action"
	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/i18n"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
)

// tickInterval est la période de la boucle. La même que le dispatcher de
// rappels : une mission ne se réveille jamais à la minute près, mais un
// tick lent retarderait aussi les tests de recette.
const tickInterval = 30 * time.Second

// maxDuePerTick borne le travail d'un tick : chaque réveil est un tour de
// modèle complet, dix suffisent largement à rattraper un arrêt.
const maxDuePerTick = 10

// runTimeout borne un réveil, comme un tour de tâche planifiée : personne
// n'attend devant son téléphone, mais un dossier ne doit pas monopoliser la
// boucle.
const runTimeout = 5 * time.Minute

// Backoff sur échec d'exécution : 5 min doublées, plafonnées à 1 h. Au
// failureAlertThreshold-ième échec consécutif, le membre est prévenu — une
// mission qui ne tourne plus sans que personne ne le sache est une promesse
// rompue en silence.
const (
	failureBackoffBase    = 5 * time.Minute
	failureBackoffMax     = 1 * time.Hour
	failureAlertThreshold = 8
)

// noNoteDelay est le garde-fou d'inaction : un réveil terminé sans appel à
// update_mission est replanifié d'office à cette distance. Une mission ne
// reste jamais sans échéance, et ne tourne jamais en boucle sur le tick.
const noNoteDelay = 24 * time.Hour

// silentMarker est le marqueur de silence, même convention que les
// déclencheurs de plugins (pluginsdk.TriggerSilent). Recopié ici :
// internal/mission ne dépend pas du SDK des plugins.
const silentMarker = "NOTHING_TO_REPORT"

// isSilent reconnaît le marqueur, seul dans la réponse, en tolérant la
// casse et la ponctuation dont un modèle habille un mot isolé — mais rien
// de plus, sans quoi une vraie réponse le mentionnant disparaîtrait.
func isSilent(reply string) bool {
	trimmed := strings.TrimSpace(reply)
	trimmed = strings.Trim(trimmed, ".!\"'`*_ \t\n")

	return strings.EqualFold(trimmed, silentMarker)
}

// SenderSet donne accès aux fournisseurs Courier par nom — la même
// interface que le dispatcher de rappels, redéclarée pour ne pas créer de
// dépendance entre les deux boucles.
type SenderSet interface {
	Get(name string) (courier.Provider, bool)
}

// SenderMap adapte un ensemble figé de fournisseurs à SenderSet — pour les
// tests et les appelants dont la liste ne change pas.
type SenderMap map[string]courier.Provider

// Get implémente SenderSet.
func (m SenderMap) Get(name string) (courier.Provider, bool) {
	provider, ok := m[name]
	return provider, ok
}

// AgentSource fournit l'agent d'un réveil par son nom. Satisfaite par
// *agent.Registry ; une interface pour permettre un agent scripté dans les
// tests, comme le TaskRunner du dispatcher.
type AgentSource interface {
	Get(name string) (agent.Agent, error)
}

// Runner est la boucle de réveil des missions.
type Runner struct {
	cfg          *config.Config
	db           *persistence.DB
	repo         *persistence.MissionRepository
	agents       AgentSource
	senders      SenderSet
	actionEngine *action.Engine

	conversations *persistence.ConversationRepository
	auditEvents   *persistence.AuditEventRepository

	logger *slog.Logger
	now    func() time.Time
}

// NewRunner construit un Runner. agents est le registre DÉDIÉ aux réveils
// de missions (outils mémoire + update_mission, rien d'autre — voir
// internal/registry) ; lui passer le registre conversationnel réveillerait
// le bug de re-planification documenté là-bas.
func NewRunner(cfg *config.Config, db *persistence.DB, agents AgentSource, senders SenderSet, actionEngine *action.Engine, logger *slog.Logger) *Runner {
	if logger == nil {
		logger = slog.Default()
	}

	return &Runner{
		cfg:           cfg,
		db:            db,
		repo:          persistence.NewMissionRepository(db.Cipher()),
		agents:        agents,
		senders:       senders,
		actionEngine:  actionEngine,
		conversations: persistence.NewConversationRepository(),
		auditEvents:   persistence.NewAuditEventRepository(),
		logger:        logger,
		now:           time.Now,
	}
}

// WithClock remplace l'horloge (tests).
func (r *Runner) WithClock(now func() time.Time) *Runner {
	r.now = now
	return r
}

// Run exécute la boucle jusqu'à l'annulation de ctx. Un tick a lieu
// immédiatement : les missions devenues échues pendant un arrêt du
// processus se réveillent sans attendre la première période.
func (r *Runner) Run(ctx context.Context) error {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		if err := r.Tick(ctx); err != nil && ctx.Err() == nil {
			r.logger.ErrorContext(ctx, "mission: tick en erreur", "error", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Tick réveille les missions échues, la plus ancienne d'abord.
func (r *Runner) Tick(ctx context.Context) error {
	var due []persistence.Mission
	err := r.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		due, err = r.repo.ListDue(ctx, tx, r.now(), maxDuePerTick)
		return err
	})
	if err != nil {
		return err
	}

	for _, mission := range due {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.wake(ctx, mission)
	}

	return nil
}

// wake exécute un réveil complet. Jamais de contenu privé dans les
// journaux : ni l'objectif, ni le journal de bord, ni la réponse.
func (r *Runner) wake(ctx context.Context, mission persistence.Mission) {
	logCtx := []any{
		"mission_id", mission.ID,
		"org_id", mission.OrgID,
		"principal_id", mission.PrincipalID,
		"conversation_id", mission.ConversationID,
		"agent_id", mission.AgentID,
	}

	a, err := r.agents.Get(mission.AgentID)
	if err != nil {
		r.logger.ErrorContext(ctx, "mission: agent introuvable", append(logCtx, "error", err)...)
		r.recordFailure(ctx, mission, logCtx)
		return
	}

	identity, conversation := r.buildIdentity(mission)

	execCtx, cancel := context.WithTimeout(ctx, runTimeout)
	result, err := a.Execute(execCtx, agent.Request{
		Identity:     identity,
		Conversation: conversation,
		Input:        buildBriefing(mission, r.now()),
	})
	cancel()
	if err != nil {
		r.logger.ErrorContext(ctx, "mission: réveil en échec", append(logCtx, "error", err, "attempts", mission.Attempts+1)...)
		r.recordFailure(ctx, mission, logCtx)
		return
	}

	// Garde-fou d'inaction : si le tour n'a pas appelé update_mission, la
	// mission est toujours échue — sans replanification d'office, elle se
	// réveillerait à chaque tick. Détecté en relisant la ligne : l'outil
	// écrit updated_at à chaque appel.
	r.ensureRescheduled(ctx, mission, logCtx)

	reply := result.Reply

	// Les actions proposées deviennent un plan confirmable dans la
	// conversation d'origine — c'est ce qui distingue une mission d'une
	// tâche planifiée, où elles sont ignorées.
	if len(result.ProposedActions) > 0 {
		if planText, ok := r.proposePlan(ctx, identity, conversation, result, logCtx); ok {
			if isSilent(reply) || reply == "" {
				reply = planText
			} else {
				reply = reply + "\n\n" + planText
			}
		}
	}

	// Un réveil qui n'apprend rien met à jour son journal et se tait.
	if isSilent(reply) || strings.TrimSpace(reply) == "" {
		r.logger.InfoContext(ctx, "mission: réveil silencieux", logCtx...)
		return
	}

	r.deliver(ctx, mission, reply, logCtx)
}

// recordFailure incrémente le compteur d'échecs et recule l'échéance
// (backoff doublé plafonné). Le journal de bord n'est pas touché : un échec
// d'infrastructure n'est pas une note de dossier.
func (r *Runner) recordFailure(ctx context.Context, mission persistence.Mission, logCtx []any) {
	attempts := mission.Attempts + 1

	backoff := failureBackoffBase << (attempts - 1)
	if backoff > failureBackoffMax || backoff <= 0 {
		backoff = failureBackoffMax
	}

	now := r.now()
	err := r.db.WithTx(ctx, func(tx *sql.Tx) error {
		return r.repo.UpdateJournal(ctx, tx, mission.ID, mission.Journal,
			mission.Status, now.Add(backoff).UTC(), attempts, now)
	})
	if err != nil {
		r.logger.ErrorContext(ctx, "mission: échec de l'enregistrement du backoff", append(logCtx, "error", err)...)
		return
	}

	// Une seule alerte, au franchissement du seuil : le backoff plafonné
	// ferait sinon un message par heure.
	if attempts == failureAlertThreshold {
		r.deliver(ctx, mission,
			i18n.T(principalLocale(r.cfg, mission.PrincipalID), "mission.stalled", mission.Title, attempts),
			logCtx)
	}
}

// ensureRescheduled replanifie d'office une mission restée échue après son
// réveil, en le notant au journal.
func (r *Runner) ensureRescheduled(ctx context.Context, before persistence.Mission, logCtx []any) {
	now := r.now()
	err := r.db.WithTx(ctx, func(tx *sql.Tx) error {
		current, found, err := r.repo.FindByID(ctx, tx, before.ID)
		if err != nil || !found {
			return err
		}
		// updated_at a bougé : update_mission (ou un abandon humain) est
		// passé pendant le tour, rien à rattraper.
		if !current.UpdatedAt.Equal(before.UpdatedAt) || current.Status != persistence.MissionStatusActive {
			return nil
		}

		r.logger.WarnContext(ctx, "mission: réveil terminé sans note, replanifiée d'office", logCtx...)

		journal := current.Journal
		entry := now.UTC().Format("2006-01-02") + ": check-in ran but left no note."
		if journal != "" {
			journal += "\n" + entry
		} else {
			journal = entry
		}

		return r.repo.UpdateJournal(ctx, tx, current.ID, journal,
			persistence.MissionStatusActive, now.Add(noNoteDelay).UTC(), 0, now)
	})
	if err != nil {
		r.logger.ErrorContext(ctx, "mission: échec de la replanification d'office", append(logCtx, "error", err)...)
	}
}

// proposePlan transforme les actions proposées en plan d'actions en attente
// de confirmation dans la conversation de la mission. Patron du scheduler :
// la conversation est créée si besoin pour que « confirmer » la retrouve.
func (r *Runner) proposePlan(ctx context.Context, identity model.ExecutionIdentity, conversation model.Conversation, result agent.Result, logCtx []any) (string, bool) {
	if r.actionEngine == nil {
		r.logger.ErrorContext(ctx, "mission: actions proposées mais aucun moteur d'actions câblé", logCtx...)
		return "", false
	}

	if err := r.ensureConversation(ctx, conversation); err != nil {
		r.logger.ErrorContext(ctx, "mission: échec de l'enregistrement de la conversation de livraison", append(logCtx, "error", err)...)
		return "", false
	}

	plan, planText, err := r.actionEngine.CreatePlan(ctx, identity, result.ProposedActions)
	if err != nil {
		r.logger.ErrorContext(ctx, "mission: échec de la création du plan d'actions", append(logCtx, "error", err)...)
		return "", false
	}

	r.logger.InfoContext(ctx, "mission: plan d'actions proposé, en attente de confirmation humaine",
		append(logCtx, "action_plan_id", plan.ID, "action_count", len(result.ProposedActions))...)

	r.recordPlanProposedAudit(ctx, identity, plan, len(result.ProposedActions), logCtx)

	return planText, true
}

// ensureConversation insère la conversation si elle n'existe pas encore —
// même besoin que le scheduler : un plan sans conversation ne peut pas
// recevoir son « confirmer ».
func (r *Runner) ensureConversation(ctx context.Context, conv model.Conversation) error {
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, found, err := r.conversations.FindByProviderAndExternalChannelID(ctx, tx, conv.Provider, conv.ChannelID)
		if err != nil {
			return err
		}
		if found {
			return nil
		}

		now := r.now().UTC().Format(time.RFC3339)

		return r.conversations.Insert(ctx, tx, persistence.Conversation{
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
	})
}

// recordPlanProposedAudit journalise l'événement d'audit
// "action_plan.proposed" — identifiants et compteurs seulement.
func (r *Runner) recordPlanProposedAudit(ctx context.Context, identity model.ExecutionIdentity, plan persistence.ActionPlan, actionCount int, logCtx []any) {
	metadata, err := json.Marshal(map[string]any{
		"plan_id":       string(plan.ID),
		"actions_count": actionCount,
	})
	if err != nil {
		r.logger.ErrorContext(ctx, "mission: échec de la sérialisation des métadonnées d'audit", append(logCtx, "error", err)...)
		return
	}
	metadataJSON := string(metadata)
	convID := plan.ConversationID

	event := persistence.AuditEvent{
		ID:              persistence.AuditEventID(uuid.NewString()),
		OrgID:           plan.OrgID,
		PrincipalID:     identity.PrincipalID,
		Trigger:         model.TriggerMission,
		ConversationID:  &convID,
		EventType:       "action_plan.proposed",
		ResourceKind:    "action_plan",
		ResourceScope:   plan.Scope,
		ResourceScopeID: plan.ScopeID,
		Outcome:         "proposed",
		MetadataJSON:    &metadataJSON,
		CreatedAt:       r.now().UTC().Format(time.RFC3339),
	}

	err = r.db.WithTx(ctx, func(tx *sql.Tx) error {
		return r.auditEvents.Insert(ctx, tx, event)
	})
	if err != nil {
		r.logger.ErrorContext(ctx, "mission: échec de l'enregistrement de l'audit du plan", append(logCtx, "error", err)...)
	}
}

// deliver envoie text sur le canal de la mission.
func (r *Runner) deliver(ctx context.Context, mission persistence.Mission, text string, logCtx []any) {
	provider, ok := r.senders.Get(mission.Provider)
	if !ok {
		r.logger.ErrorContext(ctx, "mission: fournisseur courier introuvable, réveil non délivré", logCtx...)
		return
	}

	outgoing := courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannelRef(courier.ChannelID(mission.ChannelID)),
		courier.NewUser(courier.UserID(mission.PrincipalID), mission.PrincipalID),
		courier.WithMessageMainPart(text),
	)

	if err := provider.Send(ctx, outgoing); err != nil {
		// La note du réveil est déjà au journal et la prochaine échéance
		// posée : perdre ce message ne perd pas le dossier.
		r.logger.ErrorContext(ctx, "mission: échec de l'envoi du compte rendu", append(logCtx, "error", err)...)
		return
	}

	r.logger.InfoContext(ctx, "mission: compte rendu délivré", logCtx...)
}

// buildIdentity reconstruit l'identité d'exécution et la conversation d'un
// réveil depuis ce qui a été figé à la création de la mission, complété par
// la portée du canal telle que déclarée en configuration — patron
// agent.TaskRunner.buildIdentity : une mission ancienne suit toujours la
// politique courante de son canal.
func (r *Runner) buildIdentity(mission persistence.Mission) (model.ExecutionIdentity, model.Conversation) {
	scope := model.ScopePersonal
	scopeID := model.ScopeID(mission.PrincipalID)
	channelKind := model.ChannelPrivate

	for _, ch := range r.cfg.Channels {
		if ch.Provider != mission.Provider || ch.ChannelID != mission.ChannelID {
			continue
		}

		if ch.Kind == config.ChannelKindGroup {
			channelKind = model.ChannelGroup
		}
		if ch.Scope != "" {
			scope = model.Scope(ch.Scope)
		}
		if ch.ScopeID != "" {
			scopeID = model.ScopeID(ch.ScopeID)
		}
		break
	}

	identity := model.ExecutionIdentity{
		Trigger:              model.TriggerMission,
		PrincipalID:          model.PrincipalID(mission.PrincipalID),
		PrincipalDisplayName: principalDisplayName(r.cfg, mission.PrincipalID),
		Locale:               principalLocale(r.cfg, mission.PrincipalID),
		OrgID:                model.OrgID(mission.OrgID),
		OrgDisplayName:       r.cfg.OrganizationDisplayName(mission.OrgID),
		ConversationID:       model.ConversationID(mission.ConversationID),
		Provider:             mission.Provider,
		ChannelID:            mission.ChannelID,
		ChannelKind:          channelKind,
		Scope:                scope,
		ScopeID:              scopeID,
		MissionID:            mission.ID,
	}

	conversation := model.Conversation{
		ID:        identity.ConversationID,
		OrgID:     identity.OrgID,
		Provider:  identity.Provider,
		ChannelID: identity.ChannelID,
		Kind:      channelKind,
		Scope:     scope,
		ScopeID:   scopeID,
	}

	return identity, conversation
}

// principalDisplayName retourne le nom affiché configuré du principal, ou
// une chaîne vide — jamais l'identifiant interne, qui ne doit pas atteindre
// le modèle.
func principalDisplayName(cfg *config.Config, principalID string) string {
	for _, p := range cfg.Identities.Principals {
		if p.ID == principalID {
			return p.DisplayName
		}
	}

	return ""
}

// principalLocale rend la langue du principal, symétrique de
// principalDisplayName. Elle a la même limite : un membre rattaché en ligne
// n'est pas dans la configuration, et reçoit alors la langue par défaut de
// l'instance. Le réveil d'une mission ne dispose pas de la transaction qui
// permettrait de lire members.locale, et le seul texte concerné est
// l'avertissement d'enlisement.
func principalLocale(cfg *config.Config, principalID string) i18n.Locale {
	for _, p := range cfg.Identities.Principals {
		if p.ID == principalID {
			if locale, ok := i18n.Parse(p.Locale); ok {
				return locale
			}
			break
		}
	}

	return i18n.Resolve(cfg.DefaultLocale)
}

// buildBriefing compose l'entrée du réveil : le dossier complet. C'est ce
// qui remplace l'amnésie des tâches planifiées — History et Summary restent
// vides, le journal EST la mémoire du dossier. En anglais, comme tout texte
// destiné au modèle.
func buildBriefing(mission persistence.Mission, now time.Time) string {
	var b strings.Builder

	b.WriteString("Scheduled check-in on a mission you follow for the person named in the execution context.\n\n")
	fmt.Fprintf(&b, "## Mission: %s\n\n", mission.Title)
	fmt.Fprintf(&b, "Objective (fixed at creation):\n%s\n\n", mission.Objective)

	if strings.TrimSpace(mission.Journal) != "" {
		fmt.Fprintf(&b, "## Logbook (your own notes from previous check-ins, oldest first)\n\n%s\n\n", mission.Journal)
	} else {
		b.WriteString("## Logbook\n\nEmpty: this is the first check-in.\n\n")
	}

	if !mission.LastRunAt.IsZero() {
		fmt.Fprintf(&b, "Previous check-in: %s.\n", mission.LastRunAt.UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(&b, "Current time: %s.\n\n", now.UTC().Format(time.RFC3339))

	b.WriteString(`## What to do now

1. Act first: advance the mission with the tools you have. Anything that
   writes outside (an email, an event) becomes a proposed action awaiting
   the person's confirmation in their conversation — propose it when the
   logbook says it is due.
2. Then ALWAYS call update_mission exactly once: a dense dated note (what
   moved, what you decided, what to look at next time) and the delay until
   the next check-in, chosen from the matter's real pace. Close with
   status 'done' only when the objective is reached. The logbook is capped:
   keep notes short, they replace your memory of this matter.
3. Your reply is sent to the person's conversation. If nothing is worth
   telling them right now (routine check, nothing new), reply exactly
   NOTHING_TO_REPORT and nothing else — the logbook keeps the trace.
`)

	return strings.TrimSpace(b.String())
}
