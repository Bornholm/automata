package observability

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// shutdownTimeout borne la durée de l'arrêt propre du serveur HTTP
// d'observabilité à l'annulation du contexte (même principe que les autres
// composants arrêtés par internal/registry.Run).
const shutdownTimeout = 5 * time.Second

// Ready est un indicateur de disponibilité partagé (readiness), mis à true
// une fois que la persistance est ouverte et que les pipelines
// ingress/scheduler sont démarrés (internal/registry.Run). Sa valeur zéro
// (non prête) est le comportement par défaut et sûr : un serveur qui
// n'expose pas encore GET /healthz/ready avec 200 ne doit jamais recevoir
// de trafic applicatif d'un orchestrateur externe.
type Ready struct {
	ready atomic.Bool
}

// Set marque le service comme prêt (ou non, si ready vaut false).
func (r *Ready) Set(ready bool) {
	if r == nil {
		return
	}
	r.ready.Store(ready)
}

// Load indique si le service est actuellement prêt.
func (r *Ready) Load() bool {
	if r == nil {
		return false
	}
	return r.ready.Load()
}

// Server expose localement, sur un unique http.Server, la santé du
// processus (liveness/readiness) et un export JSON des métriques (le plan de conception
// Phase 20) :
//
//   - GET /healthz/live  : 200 dès que le processus tourne, ne dépend
//     d'aucun état interne (liveness).
//   - GET /healthz/ready : 200 si ready.Load() est vrai, 503 sinon
//     (readiness).
//   - GET /metrics       : export JSON de metrics.Snapshot().
//
// Aucun endpoint n'expose de contenu de message, de transcription ni
// d'argument d'outil : uniquement des compteurs agrégés (AGENTS.md, "ne pas
// journaliser les contenus privés" — le même principe s'applique à
// l'exposition, pas seulement à la journalisation).
type Server struct {
	httpServer *http.Server
}

// NewServer construit un Server écoutant sur addr (ex : "127.0.0.1:9090").
// metrics et ready ne doivent pas être nil.
func NewServer(addr string, metrics *Metrics, ready *Ready, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz/live", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("GET /healthz/ready", func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := metrics.EncodeJSON(w); err != nil {
			logger.ErrorContext(r.Context(), "observability: échec de l'écriture de l'export des métriques", "error", err)
		}
	})

	return &Server{
		httpServer: &http.Server{
			Addr:    addr,
			Handler: mux,
		},
	}
}

// Run démarre l'écoute et bloque jusqu'à l'annulation de ctx, moment auquel
// le serveur est arrêté proprement (context.WithTimeout borné par
// shutdownTimeout). Retourne nil sur un arrêt normal déclenché par ctx ;
// toute autre erreur de démarrage ou d'arrêt est retournée telle quelle.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)

	go func() {
		if err := s.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("observability: écoute du serveur http: %w", err)
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("observability: arrêt du serveur http: %w", err)
		}

		<-errCh

		return nil
	case err := <-errCh:
		return err
	}
}
