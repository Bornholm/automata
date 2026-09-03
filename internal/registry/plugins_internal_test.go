package registry

import (
	"reflect"
	"testing"

	"github.com/bornholm/automata/internal/agent"
)

// L'identité d'appel traverse deux couches. Un champ oublié dans cette
// conversion ne casse aucune compilation et ne produit aucune erreur : il
// arrive vide au plugin, qui refuse alors ce qu'il ne reconnaît plus.
//
// C'est arrivé à SubAgent, et le sous-agent netprobe en est devenu
// entièrement inutilisable — connexion MCP établie, sept outils annoncés,
// et « unknown sub-agent » à chaque appel (production, 2026-09-03).
//
// Le test compare les champs un à un plutôt que d'en vérifier quelques-uns :
// c'est l'oubli qu'il faut attraper, y compris celui d'un champ ajouté
// demain.
func TestToPluginCallContext_CarriesEveryField(t *testing.T) {
	in := agent.PluginCallContext{
		OrgID:          "org-1",
		MemberID:       "alice",
		Scope:          "private",
		ScopeID:        "conv-1",
		IdempotencyKey: "act-7",
		SubAgent:       "netprobe",
	}

	got := toPluginCallContext(in)

	source := reflect.ValueOf(in)
	target := reflect.ValueOf(got)
	for i := 0; i < source.NumField(); i++ {
		name := source.Type().Field(i).Name

		field := target.FieldByName(name)
		if !field.IsValid() {
			t.Errorf("le champ %s n'existe pas dans plugin.CallContext", name)
			continue
		}
		if field.Interface() != source.Field(i).Interface() {
			t.Errorf("champ %s perdu dans la conversion: %q attendu, %q obtenu",
				name, source.Field(i).Interface(), field.Interface())
		}
	}
}
