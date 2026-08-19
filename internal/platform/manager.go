package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bornholm/go-courier"

	"github.com/bornholm/automata/internal/persistence"
)

// syncInterval borne le délai entre un changement en base et sa prise en
// compte : l'interface d'administration demande une synchronisation
// immédiate après chaque modification, ce battement n'est qu'un filet.
const syncInterval = 30 * time.Second

// Builder construit un fournisseur Courier à partir de la configuration
// déchiffrée d'un compte. Implémenté par internal/registry, qui connaît les
// constructeurs de chaque type de fournisseur ; qrHandler, s'il n'est pas
// nil, reçoit les codes d'appairage (WhatsApp).
type Builder func(id, providerType string, config map[string]any, qrHandler func(code string, linked bool)) (courier.Provider, error)

// Runner exécute le pipeline d'ingestion d'un fournisseur jusqu'à
// l'annulation du contexte. Implémenté par internal/registry (ingress).
type Runner func(ctx context.Context, id string, provider courier.Provider) error

// Decryptor déchiffre la configuration stockée.
type Decryptor interface {
	Open(sealed string) (string, error)
}

// Manager démarre, arrête et surveille un pipeline par compte de
// messagerie enregistré. Il est la source de vérité de l'état affiché par
// l'administration.
type Manager struct {
	db      *persistence.DB
	repo    *persistence.PlatformRepository
	secrets Decryptor
	build   Builder
	run     Runner
	logger  *slog.Logger

	statuses *registry

	mu      sync.Mutex
	running map[string]*running
	// providers conserve le dernier fournisseur construit par compte, pour
	// que l'envoi de messages (scheduler, rappels) suive les changements.
	providers map[string]courier.Provider

	// generation retient la configuration appliquée, pour ne redémarrer un
	// pipeline que lorsqu'elle change réellement.
	generation map[string]string

	// wake réveille la boucle de synchronisation à la demande.
	wake chan struct{}
}

// NewManager construit le gestionnaire.
func NewManager(db *persistence.DB, secrets Decryptor, build Builder, run Runner, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}

	return &Manager{
		db:         db,
		repo:       persistence.NewPlatformRepository(),
		secrets:    secrets,
		build:      build,
		run:        run,
		logger:     logger,
		statuses:   newRegistry(),
		running:    map[string]*running{},
		providers:  map[string]courier.Provider{},
		generation: map[string]string{},
		wake:       make(chan struct{}, 1),
	}
}

// Run tient les pipelines alignés sur la base jusqu'à l'annulation de ctx,
// puis arrête proprement tout ce qui tourne.
func (m *Manager) Run(ctx context.Context) error {
	if err := m.sync(ctx); err != nil {
		m.logger.ErrorContext(ctx, "platform: synchronisation initiale échouée", "error", err)
	}

	ticker := time.NewTicker(syncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.stopAll()
			return ctx.Err()
		case <-ticker.C:
		case <-m.wake:
		}

		if err := m.sync(ctx); err != nil {
			m.logger.ErrorContext(ctx, "platform: synchronisation échouée", "error", err)
		}
	}
}

// Wake demande une synchronisation immédiate (après une modification dans
// l'administration). Ne bloque jamais : une demande déjà en attente suffit.
func (m *Manager) Wake() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Statuses retourne l'état de tous les comptes connus.
func (m *Manager) Statuses() map[string]Status { return m.statuses.all() }

// Status retourne l'état d'un compte.
func (m *Manager) Status(id string) (Status, bool) { return m.statuses.get(id) }

// Get retourne le fournisseur d'un compte, s'il est démarré. C'est le
// point d'accès des composants qui envoient des messages hors ingestion
// (scheduler, rappels) : ils voient ainsi les comptes ajoutés ou retirés
// en cours d'exécution, ce qu'une map figée au démarrage ne permettait pas.
func (m *Manager) Get(name string) (courier.Provider, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	provider, ok := m.providers[name]
	return provider, ok
}

// Providers retourne les fournisseurs actuellement construits, pour les
// composants qui envoient des messages hors ingestion (scheduler, rappels).
func (m *Manager) Providers() map[string]courier.Provider {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make(map[string]courier.Provider, len(m.providers))
	for id, provider := range m.providers {
		out[id] = provider
	}

	return out
}

// sync compare la base à ce qui tourne, et corrige l'écart.
func (m *Manager) sync(ctx context.Context) error {
	platforms, err := m.load(ctx)
	if err != nil {
		return err
	}

	wanted := map[string]struct{}{}

	for _, p := range platforms {
		if !p.Enabled {
			continue
		}
		wanted[p.ID] = struct{}{}

		fingerprint := p.Type + "\x00" + p.Config
		m.mu.Lock()
		_, isRunning := m.running[p.ID]
		unchanged := m.generation[p.ID] == fingerprint
		m.mu.Unlock()

		if isRunning && unchanged {
			continue
		}
		if isRunning {
			// Configuration modifiée : on redémarre pour l'appliquer.
			m.stop(p.ID)
		}

		if err := m.start(ctx, p, fingerprint); err != nil {
			m.logger.ErrorContext(ctx, "platform: démarrage impossible", "platform_id", p.ID, "error", err)
			m.statuses.set(p.ID, func(s *Status) {
				s.Type = p.Type
				s.State = StateFailed
				s.Err = err.Error()
			})
		}
	}

	// Comptes disparus ou désactivés : on arrête ce qui tourne encore.
	m.mu.Lock()
	var obsolete []string
	for id := range m.running {
		if _, ok := wanted[id]; !ok {
			obsolete = append(obsolete, id)
		}
	}
	m.mu.Unlock()

	for _, id := range obsolete {
		m.stop(id)
		m.statuses.set(id, func(s *Status) {
			s.State = StateStopped
			s.PairingCode = ""
		})
	}

	// Comptes supprimés de la base : on oublie aussi leur état.
	known := map[string]struct{}{}
	for _, p := range platforms {
		known[p.ID] = struct{}{}
	}
	for id := range m.statuses.all() {
		if _, ok := known[id]; !ok {
			m.statuses.remove(id)
		}
	}

	return nil
}

// load lit les comptes enregistrés et déchiffre leur configuration.
func (m *Manager) load(ctx context.Context) ([]persistence.Platform, error) {
	var platforms []persistence.Platform
	err := m.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		platforms, err = m.repo.List(ctx, tx)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("platform: lecture des comptes: %w", err)
	}

	return platforms, nil
}

// start construit le fournisseur et lance son pipeline.
func (m *Manager) start(ctx context.Context, p persistence.Platform, fingerprint string) error {
	config, err := m.decodeConfig(p)
	if err != nil {
		return err
	}

	m.statuses.set(p.ID, func(s *Status) {
		s.Type = p.Type
		s.State = StateStarting
		s.Err = ""
	})

	provider, err := m.build(p.ID, p.Type, config, func(code string, linked bool) {
		switch {
		case code != "":
			m.statuses.set(p.ID, func(s *Status) {
				s.State = StatePairing
				s.PairingCode = code
			})
			// Le code d'appairage n'est jamais journalisé : il vaut accès au
			// compte pendant sa durée de vie.
			m.logger.InfoContext(ctx, "platform: code d'appairage disponible", "platform_id", p.ID)
		case linked:
			m.statuses.set(p.ID, func(s *Status) {
				s.State = StateRunning
				s.PairingCode = ""
			})
			m.logger.InfoContext(ctx, "platform: compte appairé", "platform_id", p.ID)
		default:
			m.statuses.set(p.ID, func(s *Status) {
				s.State = StateFailed
				s.PairingCode = ""
				s.Err = "appairage abandonné"
			})
		}
	})
	if err != nil {
		return err
	}

	pipelineCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	m.mu.Lock()
	m.running[p.ID] = &running{cancel: cancel, done: done}
	m.providers[p.ID] = provider
	m.generation[p.ID] = fingerprint
	m.mu.Unlock()

	go func() {
		defer close(done)

		m.statuses.set(p.ID, func(s *Status) {
			if s.State != StatePairing {
				s.State = StateRunning
			}
		})

		err := m.run(pipelineCtx, p.ID, provider)

		switch {
		case pipelineCtx.Err() != nil:
			m.statuses.set(p.ID, func(s *Status) {
				s.State = StateStopped
				s.PairingCode = ""
			})
		case err != nil:
			m.logger.ErrorContext(ctx, "platform: pipeline arrêté en erreur", "platform_id", p.ID, "error", err)
			m.statuses.set(p.ID, func(s *Status) {
				s.State = StateFailed
				s.Err = err.Error()
				s.PairingCode = ""
			})
		default:
			m.statuses.set(p.ID, func(s *Status) { s.State = StateStopped })
		}

		// Le pipeline n'est plus : la prochaine synchronisation le
		// relancera si le compte est toujours actif.
		m.mu.Lock()
		if current, ok := m.running[p.ID]; ok && current.done == done {
			delete(m.running, p.ID)
			delete(m.generation, p.ID)
			delete(m.providers, p.ID)
		}
		m.mu.Unlock()
	}()

	return nil
}

// decodeConfig déchiffre et décode la configuration d'un compte.
func (m *Manager) decodeConfig(p persistence.Platform) (map[string]any, error) {
	raw := p.Config
	if m.secrets != nil {
		opened, err := m.secrets.Open(p.Config)
		if err != nil {
			return nil, fmt.Errorf("platform: configuration du compte %q: %w", p.ID, err)
		}
		raw = opened
	}

	config := map[string]any{}
	if raw == "" {
		return config, nil
	}
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		return nil, fmt.Errorf("platform: configuration du compte %q illisible: %w", p.ID, err)
	}

	return config, nil
}

// stop arrête le pipeline d'un compte et attend sa sortie.
func (m *Manager) stop(id string) {
	m.mu.Lock()
	current, ok := m.running[id]
	if ok {
		delete(m.running, id)
		delete(m.generation, id)
		// Le fournisseur part avec le pipeline : laisser le scheduler ou
		// les rappels émettre par un compte arrêté produirait des envois
		// silencieusement perdus.
		delete(m.providers, id)
	}
	m.mu.Unlock()

	if !ok {
		return
	}

	current.cancel()
	select {
	case <-current.done:
	case <-time.After(10 * time.Second):
		// Un fournisseur qui ne rend pas la main ne doit pas bloquer
		// l'administration : on le laisse derrière nous, en le signalant.
		m.logger.Warn("platform: arrêt du pipeline trop long, abandonné", "platform_id", id)
	}
}

// stopAll arrête tous les pipelines (arrêt du processus).
func (m *Manager) stopAll() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.running))
	for id := range m.running {
		ids = append(ids, id)
	}
	m.mu.Unlock()

	for _, id := range ids {
		m.stop(id)
	}
}
