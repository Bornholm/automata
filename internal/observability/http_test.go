package observability

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newTestServer construit un Server sur un listener local éphémère et
// retourne son URL de base ainsi qu'une fonction d'arrêt.
func newTestServer(t *testing.T, metrics *Metrics, ready *Ready) (baseURL string, stop func()) {
	t.Helper()

	srv := NewServer("127.0.0.1:0", metrics, ready, nil)

	// L'adresse "127.0.0.1:0" ne permet pas de connaître le port avant
	// l'écoute effective : on utilise httptest.NewServer avec le même mux
	// pour connaître immédiatement l'URL, tout en gardant Server.Run testé
	// séparément pour le cycle de vie (démarrage/arrêt propre).
	_ = srv

	ts := httptest.NewServer(srv.httpServer.Handler)

	return ts.URL, ts.Close
}

func TestServer_HealthzLive(t *testing.T) {
	baseURL, stop := newTestServer(t, NewMetrics(), &Ready{})
	defer stop()

	resp, err := http.Get(baseURL + "/healthz/live")
	if err != nil {
		t.Fatalf("GET /healthz/live: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("statut = %d, attendu %d", resp.StatusCode, http.StatusOK)
	}
}

func TestServer_HealthzReady(t *testing.T) {
	ready := &Ready{}
	baseURL, stop := newTestServer(t, NewMetrics(), ready)
	defer stop()

	resp, err := http.Get(baseURL + "/healthz/ready")
	if err != nil {
		t.Fatalf("GET /healthz/ready: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("avant disponibilité: statut = %d, attendu %d", resp.StatusCode, http.StatusServiceUnavailable)
	}

	ready.Set(true)

	resp, err = http.Get(baseURL + "/healthz/ready")
	if err != nil {
		t.Fatalf("GET /healthz/ready: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("après disponibilité: statut = %d, attendu %d", resp.StatusCode, http.StatusOK)
	}
}

func TestServer_Metrics(t *testing.T) {
	metrics := NewMetrics()
	metrics.IncMessagesReceived()
	metrics.IncMessagesReceived()
	metrics.IncActionProposed()

	baseURL, stop := newTestServer(t, metrics, &Ready{})
	defer stop()

	resp, err := http.Get(baseURL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("statut = %d, attendu %d", resp.StatusCode, http.StatusOK)
	}

	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, attendu application/json", ct)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("décodage JSON: %v", err)
	}

	if got, want := body["messages_received"].(float64), float64(2); got != want {
		t.Errorf("messages_received = %v, attendu %v", got, want)
	}
	if got, want := body["actions_proposed"].(float64), float64(1); got != want {
		t.Errorf("actions_proposed = %v, attendu %v", got, want)
	}
}

func TestServer_RunStopsOnContextCancel(t *testing.T) {
	srv := NewServer("127.0.0.1:0", NewMetrics(), &Ready{}, nil)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- srv.Run(ctx)
	}()

	// Laisse le temps au serveur de démarrer réellement l'écoute avant de
	// l'annuler.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() après annulation = %v, attendu nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() ne s'est pas arrêté dans le délai imparti après annulation du contexte")
	}
}
