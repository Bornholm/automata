package plugin

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/bornholm/go-courier"

	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/pkg/pluginsdk"
	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// TriggerRunner exécute le sous-agent d'un plugin pour un événement.
// Implémenté dans internal/registry (il détient le client LLM et le moteur
// d'actions) : le routeur ne connaît que ce contrat.
type TriggerRunner interface {
	// RunTrigger exécute le sous-agent du plugin avec input pour
	// l'identité donnée, crée un plan pour ses actions proposées et
	// retourne le texte à envoyer (réponse + rapport de plan). Un texte
	// vide n'envoie rien.
	RunTrigger(ctx context.Context, pluginName string, identity model.ExecutionIdentity, conversation model.Conversation, input string) (string, error)
}

// SenderSet donne accès aux fournisseurs de messagerie par nom de compte.
// Même contrat que reminder.SenderSet — satisfait par platform.Manager.
type SenderSet interface {
	Get(name string) (courier.Provider, bool)
}

// triggerExecutionTimeout borne un tour déclenché : même budget que les
// tâches planifiées.
const triggerExecutionTimeout = 5 * time.Minute

// dedupeCacheSize borne la mémoire des identifiants déjà vus, par plugin.
const dedupeCacheSize = 1024

// TriggerRouter consomme les flux WatchTriggers des plugins qui en
// déclarent, applique les garde-fous, et fait exécuter le sous-agent.
// L'événement DÉSIGNE (une organisation, un membre) mais ne décide rien :
// l'activation et l'appartenance sont re-vérifiées en base à chaque
// événement.
type TriggerRouter struct {
	manager  *Manager
	db       *persistence.DB
	runner   TriggerRunner
	senders  SenderSet
	logger   *slog.Logger
	members  *persistence.MemberRepository
	bindings *persistence.ChannelBindingRepository

	maxPerMinute  int
	maxConcurrent chan struct{}

	mu    sync.Mutex
	seen  map[string]*dedupeRing
	rates map[string]*rateWindow
}

// NewTriggerRouter construit le routeur.
func NewTriggerRouter(manager *Manager, db *persistence.DB, runner TriggerRunner, senders SenderSet, logger *slog.Logger) *TriggerRouter {
	if logger == nil {
		logger = slog.Default()
	}
	return &TriggerRouter{
		manager:       manager,
		db:            db,
		runner:        runner,
		senders:       senders,
		logger:        logger,
		members:       persistence.NewMemberRepository(),
		bindings:      persistence.NewChannelBindingRepository(),
		maxPerMinute:  manager.cfg.Triggers.EffectiveMaxPerMinute(),
		maxConcurrent: make(chan struct{}, manager.cfg.Triggers.EffectiveMaxConcurrent()),
		seen:          map[string]*dedupeRing{},
		rates:         map[string]*rateWindow{},
	}
}

// Run ouvre un flux par plugin déclarant des déclencheurs et le maintient
// jusqu'à l'arrêt. Les flux vivent sur le contexte passé (celui de
// l'instance), reconnexion à délai croissant plafonné.
func (r *TriggerRouter) Run(ctx context.Context) {
	var wg sync.WaitGroup

	for _, st := range r.manager.Statuses() {
		if !st.HasTriggers {
			continue
		}
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			r.watchLoop(ctx, name)
		}(st.Name)
	}

	wg.Wait()
}

func (r *TriggerRouter) watchLoop(ctx context.Context, pluginName string) {
	backoff := time.Second

	for ctx.Err() == nil {
		client, _, ok := r.manager.GetOrRestart(ctx, pluginName)
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = minDuration(backoff*2, time.Minute)
			continue
		}

		stream, err := client.WatchTriggers(ctx, &proto.WatchTriggersRequest{})
		if err != nil {
			r.logger.WarnContext(ctx, "plugin: ouverture du flux de déclencheurs échouée",
				"plugin", pluginName, "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = minDuration(backoff*2, time.Minute)
			continue
		}

		r.logger.InfoContext(ctx, "plugin: flux de déclencheurs ouvert", "plugin", pluginName)
		backoff = time.Second

		for {
			event, err := stream.Recv()
			if err != nil {
				if ctx.Err() == nil && err != io.EOF {
					r.logger.WarnContext(ctx, "plugin: flux de déclencheurs interrompu",
						"plugin", pluginName, "error", err)
				}
				break
			}
			r.handle(ctx, pluginName, event)
		}
	}
}

// handle applique les garde-fous puis exécute, dans l'ordre : doublon,
// débit, activation, appartenance, identité, tour, envoi.
func (r *TriggerRouter) handle(ctx context.Context, pluginName string, event *proto.TriggerEvent) {
	logCtx := []any{"plugin", pluginName, "event_id", event.Id, "kind", event.Kind, "org_id", event.OrgId}

	if event.Id == "" || event.OrgId == "" || event.MemberId == "" {
		r.logger.WarnContext(ctx, "plugin: événement incomplet ignoré", logCtx...)
		return
	}

	if !r.firstSight(pluginName, event.Id) {
		return
	}

	if !r.allowRate(pluginName, event.OrgId) {
		r.logger.WarnContext(ctx, "plugin: événement abandonné (plafond de débit)", logCtx...)
		return
	}

	// Re-vérification côté hôte : l'événement désigne, il ne décide pas.
	var member persistence.Member
	err := r.db.WithTx(ctx, func(tx *sql.Tx) error {
		enabled, err := r.manager.hostService.activations.IsEnabled(ctx, tx, pluginName, event.OrgId)
		if err != nil {
			return err
		}
		if !enabled {
			return fmt.Errorf("plugin inactif pour l'organisation")
		}

		m, found, err := r.members.FindByID(ctx, tx, event.MemberId)
		if err != nil {
			return err
		}
		if !found || m.OrgID != event.OrgId {
			return fmt.Errorf("membre hors de l'organisation")
		}
		member = m
		return nil
	})
	if err != nil {
		r.logger.WarnContext(ctx, "plugin: événement refusé", append(logCtx, "error", err)...)
		return
	}

	// Le tour vise la conversation privée du membre : sans rattachement de
	// messagerie, il n'y a nulle part où répondre.
	if !member.Linked() {
		r.logger.WarnContext(ctx, "plugin: membre sans conversation privée, événement ignoré", logCtx...)
		return
	}

	select {
	case r.maxConcurrent <- struct{}{}:
	case <-ctx.Done():
		return
	}

	go func() {
		defer func() { <-r.maxConcurrent }()
		r.execute(ctx, pluginName, member, event, logCtx)
	}()
}

func (r *TriggerRouter) execute(ctx context.Context, pluginName string, member persistence.Member, event *proto.TriggerEvent, logCtx []any) {
	// Livraison verbatim : un texte que la personne a écrit elle-même —
	// un rappel échu, typiquement — part tel quel. Le faire passer par le
	// sous-agent coûterait un appel de modèle et le reformulerait, ce qui
	// est exactement ce qu'un pense-bête ne doit pas subir.
	//
	// DeliverText est du contenu privé : il n'apparaît jamais dans les
	// journaux, seulement le fait qu'il y en avait un.
	if event.DeliverText != "" {
		r.logger.InfoContext(ctx, "plugin: livraison verbatim d'un déclencheur", logCtx...)
		r.send(ctx, member, event.DeliverText, logCtx)
		return
	}

	identity, conversation := r.buildIdentity(member)

	execCtx, cancel := context.WithTimeout(ctx, triggerExecutionTimeout)
	defer cancel()

	started := time.Now()
	reply, err := r.runner.RunTrigger(execCtx, pluginName, identity, conversation, event.AgentInput)
	if err != nil {
		r.logger.ErrorContext(ctx, "plugin: exécution du déclencheur échouée",
			append(logCtx, "error", err, "duration", time.Since(started).String())...)
		return
	}

	r.logger.InfoContext(ctx, "plugin: déclencheur traité",
		append(logCtx, "duration", time.Since(started).String(), "reply", reply != "")...)

	if reply == "" {
		return
	}

	// Le sous-agent a jugé qu'il n'y avait rien à signaler. Tous les
	// événements d'un flux ne méritent pas d'interrompre quelqu'un — un
	// courriel publicitaire, un accusé de réception —, et une personne
	// dérangée pour rien finit par ignorer aussi ce qui comptait.
	if pluginsdk.IsTriggerSilent(reply) {
		r.logger.InfoContext(ctx, "plugin: déclencheur sans suite (rien à signaler)", logCtx...)
		return
	}

	r.send(ctx, member, reply, logCtx)
}

// NotifyMember implémente Notifier (service hôte) : message applicatif,
// sans tour de modèle.
func (r *TriggerRouter) NotifyMember(ctx context.Context, orgID, memberID, text string) error {
	var member persistence.Member
	err := r.db.WithTx(ctx, func(tx *sql.Tx) error {
		m, found, err := r.members.FindByID(ctx, tx, memberID)
		if err != nil {
			return err
		}
		if !found || m.OrgID != orgID {
			return fmt.Errorf("membre hors de l'organisation")
		}
		member = m
		return nil
	})
	if err != nil {
		return err
	}
	if !member.Linked() {
		return fmt.Errorf("membre sans conversation privée")
	}

	r.send(ctx, member, text, []any{"org_id", orgID})
	return nil
}

// buildIdentity construit l'identité du tour déclenché : le membre, sa
// conversation privée, portée personnelle — motif TaskRunner.
func (r *TriggerRouter) buildIdentity(member persistence.Member) (model.ExecutionIdentity, model.Conversation) {
	conversationID := model.ConversationID(member.Provider + ":" + member.ExternalUserID)

	identity := model.ExecutionIdentity{
		Trigger:              model.TriggerPlugin,
		PrincipalID:          model.PrincipalID(member.ID),
		PrincipalDisplayName: member.DisplayName,
		OrgID:                model.OrgID(member.OrgID),
		ConversationID:       conversationID,
		Provider:             member.Provider,
		ChannelID:            member.ExternalUserID,
		ChannelKind:          model.ChannelPrivate,
		Scope:                model.ScopePersonal,
		ScopeID:              model.ScopeID(member.ID),
	}

	conversation := model.Conversation{
		ID:      conversationID,
		OrgID:   model.OrgID(member.OrgID),
		Scope:   model.ScopePersonal,
		ScopeID: model.ScopeID(member.ID),
	}

	return identity, conversation
}

// send livre le message sur le canal privé du membre, trois tentatives.
func (r *TriggerRouter) send(ctx context.Context, member persistence.Member, text string, logCtx []any) {
	providerName := member.Provider
	provider, ok := r.senders.Get(providerName)
	if !ok {
		r.logger.ErrorContext(ctx, "plugin: compte de messagerie indisponible",
			append(logCtx, "provider", providerName)...)
		return
	}

	outgoing := courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannelRef(courier.ChannelID(member.ExternalUserID)),
		courier.NewUser("automata", "Automata"),
		courier.WithMessageMainPart(text),
	)

	backoff := time.Second
	for attempt := 1; attempt <= 3; attempt++ {
		sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := provider.Send(sendCtx, outgoing)
		cancel()
		if err == nil {
			return
		}
		r.logger.WarnContext(ctx, "plugin: envoi échoué",
			append(logCtx, "attempt", attempt, "error", err)...)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
	}
}

// firstSight retourne vrai si l'identifiant n'a jamais été vu ; anneau
// borné par plugin, un plugin bavard n'affame pas la mémoire.
func (r *TriggerRouter) firstSight(pluginName, id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	ring, ok := r.seen[pluginName]
	if !ok {
		ring = newDedupeRing(dedupeCacheSize)
		r.seen[pluginName] = ring
	}
	return ring.add(id)
}

// allowRate applique le plafond par minute et par (plugin, organisation).
func (r *TriggerRouter) allowRate(pluginName, orgID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := pluginName + "\x00" + orgID
	window, ok := r.rates[key]
	if !ok {
		window = &rateWindow{}
		r.rates[key] = window
	}
	return window.allow(time.Now(), r.maxPerMinute)
}

// dedupeRing est un ensemble borné FIFO.
type dedupeRing struct {
	set   map[string]struct{}
	order []string
	size  int
}

func newDedupeRing(size int) *dedupeRing {
	return &dedupeRing{set: make(map[string]struct{}, size), size: size}
}

func (d *dedupeRing) add(id string) bool {
	if _, seen := d.set[id]; seen {
		return false
	}
	if len(d.order) >= d.size {
		oldest := d.order[0]
		d.order = d.order[1:]
		delete(d.set, oldest)
	}
	d.set[id] = struct{}{}
	d.order = append(d.order, id)
	return true
}

// rateWindow est une fenêtre glissante d'une minute.
type rateWindow struct {
	times []time.Time
}

func (w *rateWindow) allow(now time.Time, max int) bool {
	cutoff := now.Add(-time.Minute)
	kept := w.times[:0]
	for _, t := range w.times {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	w.times = kept

	if len(w.times) >= max {
		return false
	}
	w.times = append(w.times, now)
	return true
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
