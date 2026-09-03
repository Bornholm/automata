package web

import (
	"net/http"
	"strings"
	"testing"
)

// La sonde répond sans authentification, sur le port applicatif : c'est
// l'orchestrateur qui l'appelle, et une sonde derrière une session serait
// un healthcheck qui échoue toujours.
func TestHealthz_OpenAndOK(t *testing.T) {
	_, ts, client := testServer(t)

	resp, err := client.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("code = %d, attendu 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q : une sonde ne doit pas être mise en cache", got)
	}
}

// Tant que le câblage interne n'est pas fini, la sonde répond 503 : un port
// ouvert n'est pas un service prêt, et c'est exactement ce qu'un
// healthcheck naïf laisse passer.
func TestHealthz_UnavailableWhileStarting(t *testing.T) {
	server, ts, client := testServer(t)
	server.WithReadiness(func() bool { return false })

	resp, err := client.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, attendu 503", resp.StatusCode)
	}
}

// La sonde interroge vraiment la base : un port ouvert devant une base
// fermée doit se voir.
func TestHealthz_FailsWhenDatabaseIsGone(t *testing.T) {
	server, ts, client := testServer(t)

	if err := server.DB.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	resp, err := client.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, attendu 503", resp.StatusCode)
	}

	// Le message reste un état : jamais le chemin de la base ni l'erreur du
	// pilote, qui partent au journal.
	if body := body(t, resp); strings.Contains(body, ".sqlite") || strings.Contains(body, "/") {
		t.Errorf("la réponse expose un détail interne : %q", body)
	}
}
