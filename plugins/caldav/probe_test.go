package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// « Tester la connexion » doit CONSTATER, pas supposer. dial rend la main
// sans aucun aller-retour réseau dès qu'un agenda est choisi — c'est ce
// qu'il faut pour les opérations de tous les jours, et c'est ce qui faisait
// répondre « connexion réussie » à un serveur qui refusait tout. Le bouton
// paraissait alors sans effet, et le panneau d'exception de certificat, qui
// a besoin d'un échec pour paraître, ne s'affichait jamais.
func TestProbeServer_FailsWhereDialSucceeds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	cfg := memberConfig{
		ServerURL:    server.URL,
		Username:     "alice",
		CalendarPath: "/calendars/alice/perso/",
	}

	// dial ne parle à personne : il ne peut donc rien constater.
	if _, err := dial(context.Background(), cfg, "secret"); err != nil {
		t.Fatalf("dial a échoué alors qu'il ne fait aucun appel: %v", err)
	}

	if err := probeServer(context.Background(), cfg, "secret"); err == nil {
		t.Fatal("un serveur qui refuse l'authentification a été déclaré joignable")
	}
}

// Un serveur injoignable doit remonter une erreur, et pas un succès.
func TestProbeServer_ReportsAnUnreachableServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := server.URL
	server.Close()

	cfg := memberConfig{
		ServerURL:    url,
		Username:     "alice",
		CalendarPath: "/calendars/alice/perso/",
	}

	if err := probeServer(context.Background(), cfg, "secret"); err == nil {
		t.Fatal("un serveur éteint a été déclaré joignable")
	}
}
