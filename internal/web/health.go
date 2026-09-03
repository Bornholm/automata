package web

import (
	"context"
	"net/http"
	"time"
)

// GET /healthz : la sonde de santé du service web, sur le port applicatif.
//
// Elle existe parce que le serveur d'observabilité (/healthz/live,
// /healthz/ready) écoute sur une adresse séparée, souvent locale et
// désactivable : un orchestrateur qui ne publie que le port web — Dokku,
// entre autres — ne peut pas l'atteindre. Celle-ci est toujours là dès que
// le web est actif.
//
// Elle est plus qu'un « le processus tourne » : elle interroge la base.
// C'est la panne qui compte, parce que tout en dépend, et un port ouvert
// devant une base bloquée est précisément ce qu'un healthcheck naïf laisse
// passer. Elle n'expose ni version, ni chemin, ni détail interne : un
// message d'état, rien d'autre. Ce qu'elle ne couvre pas non plus : la
// joignabilité des fournisseurs de messagerie ou des modèles, surveillée
// par les sondes de l'exploitant (internal/alerting).

// healthCheckTimeout borne la sonde. Court à dessein : un healthcheck qui
// attend est un healthcheck qui ment, le temps qu'il attend.
const healthCheckTimeout = 3 * time.Second

// WithReadiness câble l'indicateur de disponibilité de l'instance
// (internal/observability.Ready.Load). Sans lui, /healthz ne rend compte
// que de la base — ce qui reste vrai et utile, mais ignore le câblage des
// pipelines.
func (s *Server) WithReadiness(ready func() bool) *Server {
	s.ready = ready
	return s
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// Une sonde ne doit jamais être servie depuis un cache intermédiaire.
	w.Header().Set("Cache-Control", "no-store")

	if s.ready != nil && !s.ready() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("starting\n"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), healthCheckTimeout)
	defer cancel()

	if err := s.DB.Ping(ctx); err != nil {
		// L'erreur va au journal, pas à la réponse : elle porte le chemin
		// de la base.
		s.Logger.ErrorContext(ctx, "web: sonde de santé en échec, base injoignable", "error", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("database unavailable\n"))
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}
