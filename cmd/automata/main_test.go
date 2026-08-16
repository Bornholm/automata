package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// runHealthcheck exécute la commande "healthcheck" avec les arguments fournis
// et retourne l'erreur obtenue. errSilent traduit un code de sortie 1 (voir
// main).
func runHealthcheck(t *testing.T, args ...string) error {
	t.Helper()

	cmd := newHealthcheckCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)

	return cmd.ExecuteContext(context.Background())
}

// healthServer démarre un serveur exposant /healthz/ready avec le code
// fourni, et retourne son adresse "hôte:port".
func healthServer(t *testing.T, status int) string {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz/ready", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return strings.TrimPrefix(srv.URL, "http://")
}

func TestHealthcheck_ReadyServiceSucceeds(t *testing.T) {
	addr := healthServer(t, http.StatusOK)

	if err := runHealthcheck(t, "-addr", addr); err != nil {
		t.Fatalf("service prêt: aucune erreur attendue, obtenu %v", err)
	}
}

func TestHealthcheck_NotReadyServiceFails(t *testing.T) {
	addr := healthServer(t, http.StatusServiceUnavailable)

	err := runHealthcheck(t, "-addr", addr)
	if !errors.Is(err, errSilent) {
		t.Fatalf("service non prêt: errSilent attendu, obtenu %v", err)
	}
}

// TestHealthcheck_UnreachableServiceFails couvre le cas le plus fréquent en
// pratique : le processus n'écoute pas encore (ou plus).
func TestHealthcheck_UnreachableServiceFails(t *testing.T) {
	// Adresse fermée : on démarre puis on ferme immédiatement un serveur pour
	// obtenir un port dont on sait qu'il n'écoute plus.
	srv := httptest.NewServer(http.NewServeMux())
	addr := strings.TrimPrefix(srv.URL, "http://")
	srv.Close()

	err := runHealthcheck(t, "-addr", addr)
	if !errors.Is(err, errSilent) {
		t.Fatalf("service injoignable: errSilent attendu, obtenu %v", err)
	}
}
