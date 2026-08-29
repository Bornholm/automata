// Package llmclients détient le catalogue des clients de modèles, dont la
// base de données est la source de vérité depuis la migration 0022.
//
// Trois responsabilités :
//   - le SEMIS initial depuis le YAML (Seed), une fois pour toutes ;
//   - la traduction entre la ligne de base et la config.LLMClient attendue
//     par les constructeurs de clients, en ouvrant la clé d'API scellée ;
//   - un POOL qui garde les clients construits en mémoire et les
//     reconstruit dès que leur définition change en base, pour qu'une
//     modification faite dans l'interface d'administration prenne effet
//     sans redémarrer le processus.
//
// Ce paquet ne connaît pas les agents : il ne sait pas quel client sert
// quel rôle. Cette question relève de la résolution par organisation
// (internal/agent.ClientResolver), qui s'appuie sur lui.
package llmclients

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/secretbox"
)

// Resolved est un client prêt à l'emploi, accompagné de ce que l'appelant
// doit savoir de lui : son nom au catalogue, son modèle, et s'il accepte
// les images en entrée — un agent n'envoie JAMAIS de pièce jointe à un
// modèle texte-seul, qui rejetterait la requête entière.
type Resolved struct {
	Client         llm.Client
	Name           string
	Model          string
	SupportsVision bool
}

// Builder construit un client à partir d'une définition applicative.
// Injecté plutôt qu'importé : agent.BuildLLMClient vit dans internal/agent,
// qui dépend de ce paquet — l'inverse formerait un cycle.
type Builder func(ctx context.Context, cfg config.LLMClient) (llm.Client, error)

// Store lit et écrit le catalogue, en scellant les clés d'API au passage.
// L'interface d'administration passe par lui ; elle ne relit jamais une
// clé en clair.
type Store struct {
	db      *persistence.DB
	box     *secretbox.Box
	clients *persistence.LLMClientRepository
	orgs    *persistence.OrgAgentClientRepository
}

// NewStore construit un Store. box scelle les clés d'API
// (secretbox.NewLLMClients).
func NewStore(db *persistence.DB, box *secretbox.Box) *Store {
	return &Store{
		db:      db,
		box:     box,
		clients: persistence.NewLLMClientRepository(),
		orgs:    persistence.NewOrgAgentClientRepository(),
	}
}

// Config traduit une ligne du catalogue en config.LLMClient, en ouvrant la
// clé d'API scellée. C'est le SEUL endroit où une clé revient en clair.
func (s *Store) Config(row persistence.LLMClient) (config.LLMClient, error) {
	apiKey, err := s.box.Open(row.APIKey)
	if err != nil {
		return config.LLMClient{}, fmt.Errorf("ouverture de la clé du client %q: %w", row.Name, err)
	}

	cfg := config.LLMClient{
		Provider: row.Provider,
		Model:    row.Model,
		APIKey:   apiKey,
		BaseURL:  row.BaseURL,
		Vision:   &row.Vision,
	}

	if row.ReasoningEffort != "" {
		cfg.Reasoning = &config.LLMReasoning{Effort: row.ReasoningEffort}
	}

	if extra := strings.TrimSpace(row.ExtraFields); extra != "" {
		if err := json.Unmarshal([]byte(extra), &cfg.ExtraFields); err != nil {
			return config.LLMClient{}, fmt.Errorf("champs supplémentaires du client %q illisibles: %w", row.Name, err)
		}
	}

	return cfg, nil
}

// ImageConfig traduit une ligne du catalogue en config.ImageClient.
func (s *Store) ImageConfig(row persistence.LLMClient) (config.ImageClient, error) {
	apiKey, err := s.box.Open(row.APIKey)
	if err != nil {
		return config.ImageClient{}, fmt.Errorf("ouverture de la clé du client %q: %w", row.Name, err)
	}

	return config.ImageClient{
		Provider: row.Provider,
		Model:    row.Model,
		APIKey:   apiKey,
		BaseURL:  row.BaseURL,
	}, nil
}

// Row assemble une ligne de catalogue à partir d'une définition
// applicative, en scellant la clé d'API.
func (s *Store) Row(name, kind string, cfg config.LLMClient, now time.Time) (persistence.LLMClient, error) {
	sealed, err := s.box.Seal(cfg.APIKey)
	if err != nil {
		return persistence.LLMClient{}, fmt.Errorf("scellement de la clé du client %q: %w", name, err)
	}

	row := persistence.LLMClient{
		Name:      name,
		Kind:      kind,
		Provider:  cfg.Provider,
		Model:     cfg.Model,
		BaseURL:   cfg.BaseURL,
		APIKey:    sealed,
		Vision:    cfg.SupportsVision(),
		CreatedAt: now,
		UpdatedAt: now,
	}

	if cfg.Reasoning != nil {
		row.ReasoningEffort = cfg.Reasoning.Effort
	}

	if len(cfg.ExtraFields) > 0 {
		encoded, err := json.Marshal(cfg.ExtraFields)
		if err != nil {
			return persistence.LLMClient{}, fmt.Errorf("champs supplémentaires du client %q: %w", name, err)
		}
		row.ExtraFields = string(encoded)
	}

	return row, nil
}

// Get retourne la ligne de catalogue du client nommé.
func (s *Store) Get(ctx context.Context, name string) (persistence.LLMClient, bool, error) {
	var (
		row   persistence.LLMClient
		found bool
	)
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		row, found, err = s.clients.Get(ctx, tx, name)
		return err
	})

	return row, found, err
}

// List retourne le catalogue, éventuellement restreint à une famille.
func (s *Store) List(ctx context.Context, kind string) ([]persistence.LLMClient, error) {
	var rows []persistence.LLMClient
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		rows, err = s.clients.List(ctx, tx, kind)
		return err
	})

	return rows, err
}

// OrgChoice retourne le client choisi par une organisation pour un rôle,
// ou ("", false, nil) si elle s'en remet au défaut de l'instance.
func (s *Store) OrgChoice(ctx context.Context, orgID, role string) (string, bool, error) {
	var (
		name  string
		found bool
	)
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		name, found, err = s.orgs.Get(ctx, tx, orgID, role)
		return err
	})

	return name, found, err
}

// seedTask est le jalon posé dans maintenance_runs après le semis. C'est
// lui — et non « la table est vide » — qui décide : sans cela, une instance
// où l'opérateur a volontairement supprimé tous les clients les verrait
// réapparaître au redémarrage suivant.
const seedTask = "llm-clients-seed"

// Seed copie en base, une seule fois dans la vie d'une instance, les
// clients déclarés dans le YAML. Après quoi les sections llm_clients et
// image_clients du fichier ne sont plus lues : la base fait autorité.
func Seed(ctx context.Context, db *persistence.DB, box *secretbox.Box, cfg *config.Config, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}

	store := NewStore(db, box)
	runs := persistence.NewMaintenanceRunRepository()
	repo := persistence.NewLLMClientRepository()
	now := time.Now().UTC()

	var seeded int

	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		if _, done, err := runs.GetLastRun(ctx, tx, seedTask); err != nil {
			return err
		} else if done {
			return nil
		}

		for name, clientCfg := range cfg.LLMClients {
			row, err := store.Row(name, persistence.LLMClientKindLLM, clientCfg, now)
			if err != nil {
				return err
			}
			if err := repo.Upsert(ctx, tx, row); err != nil {
				return err
			}
			seeded++
		}

		for name, imageCfg := range cfg.ImageClients {
			row, err := store.Row(name, persistence.LLMClientKindImage, config.LLMClient{
				Provider: imageCfg.Provider,
				Model:    imageCfg.Model,
				APIKey:   imageCfg.APIKey,
				BaseURL:  imageCfg.BaseURL,
			}, now)
			if err != nil {
				return err
			}
			if err := repo.Upsert(ctx, tx, row); err != nil {
				return err
			}
			seeded++
		}

		return runs.SetLastRun(ctx, tx, seedTask, now)
	})
	if err != nil {
		return fmt.Errorf("llmclients: semis du catalogue: %w", err)
	}

	if seeded > 0 {
		logger.InfoContext(ctx, "llmclients: catalogue de modèles initialisé depuis la configuration",
			"clients", seeded)
	}

	return nil
}

// Pool garde en mémoire les clients construits et les reconstruit dès que
// leur définition change en base.
//
// Il n'y a AUCUNE invalidation explicite à écrire : chaque Get relit la
// ligne par sa clé primaire (quelques microsecondes sur SQLite local) et
// compare une empreinte de la définition. Le client n'est reconstruit que
// si l'empreinte a changé. Le résultat reste donc juste même quand un autre
// processus écrit en base, ce qu'un cache invalidé à l'écriture ne
// garantirait pas.
type Pool struct {
	store   *Store
	build   Builder
	logger  *slog.Logger
	mu      sync.Mutex
	entries map[string]*poolEntry
}

// poolEntry est le client mémorisé d'un nom, avec l'empreinte de la
// définition qui l'a produit.
type poolEntry struct {
	fingerprint string
	resolved    Resolved
}

// NewPool construit un pool servant les clients de conversation. build est
// la fabrique à utiliser (agent.BuildLLMClient en production).
func NewPool(store *Store, build Builder, logger *slog.Logger) *Pool {
	if logger == nil {
		logger = slog.Default()
	}

	return &Pool{
		store:   store,
		build:   build,
		logger:  logger,
		entries: map[string]*poolEntry{},
	}
}

// ErrUnknownClient signale un nom absent du catalogue : l'appelant doit
// alors se rabattre sur son client de démarrage plutôt que d'échouer.
var ErrUnknownClient = fmt.Errorf("llmclients: client de modèle inconnu")

// Get retourne le client nommé, en le reconstruisant si sa définition a
// changé depuis le dernier appel.
func (p *Pool) Get(ctx context.Context, name string) (Resolved, error) {
	if name == "" {
		return Resolved{}, ErrUnknownClient
	}

	row, found, err := p.store.Get(ctx, name)
	if err != nil {
		return Resolved{}, err
	}
	if !found {
		return Resolved{}, fmt.Errorf("%w: %q", ErrUnknownClient, name)
	}

	fingerprint := fingerprintOf(row)

	p.mu.Lock()
	entry, ok := p.entries[name]
	p.mu.Unlock()

	if ok && entry.fingerprint == fingerprint {
		return entry.resolved, nil
	}

	clientCfg, err := p.store.Config(row)
	if err != nil {
		return Resolved{}, err
	}

	client, err := p.build(ctx, clientCfg)
	if err != nil {
		return Resolved{}, fmt.Errorf("llmclients: construction du client %q: %w", name, err)
	}

	resolved := Resolved{
		Client:         client,
		Name:           name,
		Model:          row.Model,
		SupportsVision: row.Vision,
	}

	p.mu.Lock()
	p.entries[name] = &poolEntry{fingerprint: fingerprint, resolved: resolved}
	p.mu.Unlock()

	if ok {
		// Reconstruire réinitialise le disjoncteur du client : c'est voulu,
		// une configuration corrigée doit pouvoir repartir tout de suite.
		p.logger.InfoContext(ctx, "llmclients: client de modèle reconstruit après modification",
			"client", name, "provider", row.Provider, "model", row.Model)
	}

	return resolved, nil
}

// ImageBuilder construit un générateur d'images à partir d'une définition
// applicative. Injecté comme Builder, pour la même raison.
type ImageBuilder func(ctx context.Context, cfg config.ImageClient) (llm.ImageGenerationClient, error)

// ImagePool est au générateur d'images ce que Pool est au client de
// conversation : même mécanique d'empreinte, même reconstruction à la
// volée quand la définition change en base.
type ImagePool struct {
	store   *Store
	build   ImageBuilder
	logger  *slog.Logger
	mu      sync.Mutex
	entries map[string]*imagePoolEntry
}

type imagePoolEntry struct {
	fingerprint string
	generator   llm.ImageGenerationClient
}

// NewImagePool construit le pool des générateurs d'images.
func NewImagePool(store *Store, build ImageBuilder, logger *slog.Logger) *ImagePool {
	if logger == nil {
		logger = slog.Default()
	}

	return &ImagePool{
		store:   store,
		build:   build,
		logger:  logger,
		entries: map[string]*imagePoolEntry{},
	}
}

// Get retourne le générateur nommé, reconstruit si sa définition a changé.
func (p *ImagePool) Get(ctx context.Context, name string) (llm.ImageGenerationClient, error) {
	if name == "" {
		return nil, ErrUnknownClient
	}

	row, found, err := p.store.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%w: %q", ErrUnknownClient, name)
	}

	fingerprint := fingerprintOf(row)

	p.mu.Lock()
	entry, ok := p.entries[name]
	p.mu.Unlock()

	if ok && entry.fingerprint == fingerprint {
		return entry.generator, nil
	}

	clientCfg, err := p.store.ImageConfig(row)
	if err != nil {
		return nil, err
	}

	generator, err := p.build(ctx, clientCfg)
	if err != nil {
		return nil, fmt.Errorf("llmclients: construction du générateur d'images %q: %w", name, err)
	}

	p.mu.Lock()
	p.entries[name] = &imagePoolEntry{fingerprint: fingerprint, generator: generator}
	p.mu.Unlock()

	if ok {
		p.logger.InfoContext(ctx, "llmclients: générateur d'images reconstruit après modification",
			"client", name, "provider", row.Provider, "model", row.Model)
	}

	return generator, nil
}

// fingerprintOf résume tout ce qui change le comportement d'un client. La
// clé d'API y entre sous sa forme SCELLÉE, jamais en clair : le scellement
// étant aléatoire, réenregistrer la même clé produit une empreinte
// différente et donc une reconstruction inutile mais inoffensive.
func fingerprintOf(row persistence.LLMClient) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		row.Kind, row.Provider, row.Model, row.BaseURL, row.APIKey,
		row.ReasoningEffort, fmt.Sprintf("%t", row.Vision), row.ExtraFields,
	}, "\x00")))

	return hex.EncodeToString(sum[:])
}
