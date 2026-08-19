package web

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestFormToConfig_NestsDottedFields(t *testing.T) {
	r := httptest.NewRequest("POST", "/admin/platforms", strings.NewReader(url.Values{
		"imap.address":  {"imap.exemple.fr:993"},
		"imap.username": {"automata"},
		"smtp.address":  {"smtp.exemple.fr:587"},
		"smtp.issuer":   {"automata@exemple.fr"},
	}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	config := formToConfig(r, "mail")

	imap, ok := config["imap"].(map[string]any)
	if !ok {
		t.Fatalf("la section imap doit être imbriquée, obtenu %T", config["imap"])
	}
	if imap["address"] != "imap.exemple.fr:993" || imap["username"] != "automata" {
		t.Errorf("section imap inattendue: %+v", imap)
	}

	// Un champ non renseigné ne doit pas apparaître : le provider
	// distingue « absent » de « vide ».
	if _, present := imap["password"]; present {
		t.Error("un champ vide ne doit pas être écrit dans la configuration")
	}
}

func TestLookupConfig_FindsNestedAndFlatValues(t *testing.T) {
	config := map[string]any{
		"session_path": "data/session.db",
		"smtp":         map[string]any{"issuer": "automata@exemple.fr"},
	}

	if value, ok := lookupConfig(config, "session_path"); !ok || value != "data/session.db" {
		t.Errorf("champ simple non trouvé: %q, %v", value, ok)
	}
	if value, ok := lookupConfig(config, "smtp.issuer"); !ok || value != "automata@exemple.fr" {
		t.Errorf("champ imbriqué non trouvé: %q, %v", value, ok)
	}
	if _, ok := lookupConfig(config, "smtp.password"); ok {
		t.Error("un champ absent ne doit pas être signalé présent")
	}
	if _, ok := lookupConfig(config, "imap.address"); ok {
		t.Error("une section absente ne doit pas être signalée présente")
	}
}

// Le QR est rendu à la volée en image embarquée : rien n'est écrit sur
// disque, et le code lui-même ne transite jamais en clair vers la page.
func TestQRPNGDataURI_RendersImage(t *testing.T) {
	uri, err := qrPNGDataURI("2@code-de-test")
	if err != nil {
		t.Fatalf("qrPNGDataURI: %v", err)
	}

	if !strings.HasPrefix(uri, "data:image/png;base64,") {
		t.Fatalf("préfixe inattendu: %.40s", uri)
	}
	if strings.Contains(uri, "2@code-de-test") {
		t.Error("le code d'appairage ne doit pas apparaître en clair dans la page")
	}
}

func TestPlatformStatusChip_CoversEveryState(t *testing.T) {
	for state, expected := range map[string]string{
		"running": "ok", "pairing": "warn", "failed": "crit",
		"stopped": "neutral", "starting": "neutral", "": "neutral",
	} {
		if _, tone := platformStatusChip(state); tone != expected {
			t.Errorf("état %q: ton %q, attendu %q", state, tone, expected)
		}
	}
}
