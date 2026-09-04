package i18n

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// C'est LE test du paquet. Sans lui, une clé ajoutée en français et oubliée
// ailleurs ne se voit pas : T retombe silencieusement sur le catalogue par
// défaut, et l'espagnol se remet à parler français au bout de quelques
// mois, un message à la fois.
func TestCatalogs_CoverEveryKey(t *testing.T) {
	reference := readCatalog(t, Default)

	for _, locale := range Supported {
		if locale == Default {
			continue
		}
		translated := readCatalog(t, locale)

		for key := range reference {
			if _, ok := translated[key]; !ok {
				t.Errorf("%s: clé %q absente", locale, key)
			}
		}
		for key := range translated {
			if _, ok := reference[key]; !ok {
				t.Errorf("%s: clé %q inconnue du catalogue de référence", locale, key)
			}
		}
	}
}

// Un message traduit sans ses verbes de format n'affiche pas le nom, le
// délai ou le numéro qu'il annonce — et fmt ajoute un %!(EXTRA…) bien
// visible au milieu d'une phrase envoyée à quelqu'un.
func TestCatalogs_KeepTheSameFormatVerbs(t *testing.T) {
	reference := readCatalog(t, Default)

	for _, locale := range Supported {
		if locale == Default {
			continue
		}
		translated := readCatalog(t, locale)

		for key, template := range reference {
			want, got := formatVerbs(template), formatVerbs(translated[key])
			if strings.Join(want, ",") != strings.Join(got, ",") {
				t.Errorf("%s: %q attend les verbes %v, le catalogue en porte %v", locale, key, want, got)
			}
		}
	}
}

// Une valeur vide passe les deux tests précédents et rend un message
// silencieusement muet.
func TestCatalogs_HaveNoEmptyMessage(t *testing.T) {
	for _, locale := range Supported {
		for key, value := range readCatalog(t, locale) {
			if strings.TrimSpace(value) == "" {
				t.Errorf("%s: message %q vide", locale, key)
			}
		}
	}
}

// keyPattern relève les clés littérales passées à i18n.T dans le dépôt.
// Une clé mal orthographiée compile parfaitement et ne se voit qu'en
// production, où T rend la clé elle-même au lieu du message.
//
// Le littéral doit fermer l'argument (`,` ou `)`) : une clé composée —
// `"discover."+name+".title"` — n'est pas vérifiable ici, et la relever à
// moitié produirait un faux positif à chaque exécution. Ces cas-là ont leur
// propre test, au plus près du code qui compose la clé.
var keyPattern = regexp.MustCompile(`i18n\.TC?\([^,)]+,\s*"([^"]+)"\s*[,)]`)

// pluralKeyPattern relève les clés de base passées à i18n.TN.
var pluralKeyPattern = regexp.MustCompile(`i18n\.TN\([^,)]+,\s*"([^"]+)"\s*,`)

func TestEveryKeyUsedInTheRepositoryExists(t *testing.T) {
	root := filepath.Join("..", "..")
	reference := readCatalog(t, Default)

	var checked int
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Ni les dépendances ni les artefacts de construction.
			if name := info.Name(); name == ".git" || name == "node_modules" || name == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, ".templ") {
			return nil
		}

		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, match := range keyPattern.FindAllStringSubmatch(string(source), -1) {
			checked++
			if _, ok := reference[match[1]]; !ok {
				t.Errorf("%s: clé %q absente du catalogue", path, match[1])
			}
		}
		// TN compose sa clé : les deux formes doivent exister, sinon le
		// décompte de zéro — rare en développement, banal chez qui vient
		// d'arriver — afficherait la clé brute.
		for _, match := range pluralKeyPattern.FindAllStringSubmatch(string(source), -1) {
			for _, form := range []string{".one", ".other"} {
				checked++
				if _, ok := reference[match[1]+form]; !ok {
					t.Errorf("%s: clé %q absente du catalogue", path, match[1]+form)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("parcours du dépôt: %v", err)
	}
	if checked == 0 {
		t.Error("aucun appel à i18n.T trouvé : le relevé de clés ne protège plus rien")
	}
}

// Une étiquette de langue arrive d'une colonne, d'une configuration ou d'un
// en-tête HTTP : les trois écrivent la même langue différemment.
func TestParseAndResolve(t *testing.T) {
	cases := map[string]struct {
		want Locale
		ok   bool
	}{
		"fr": {FR, true}, "FR": {FR, true}, "fr-FR": {FR, true}, "fr_BE": {FR, true},
		"en": {EN, true}, "en-GB": {EN, true}, " es ": {ES, true}, "es-419": {ES, true},
		"de": {"", false}, "": {"", false}, "francais": {"", false},
	}

	for raw, tc := range cases {
		got, ok := Parse(raw)
		if ok != tc.ok || got != tc.want {
			t.Errorf("Parse(%q) = %q, %v — attendu %q, %v", raw, got, ok, tc.want, tc.ok)
		}
		// Resolve ne doit jamais échouer : un message qui manque vaut mieux
		// qu'une conversation interrompue.
		if resolved := Resolve(raw); !tc.ok && resolved != Default {
			t.Errorf("Resolve(%q) = %q, défaut attendu", raw, resolved)
		}
	}
}

// Une clé inconnue ne doit ni paniquer ni rendre du vide.
func TestT_FallsBackWithoutFailing(t *testing.T) {
	if got := T(ES, "clé.inexistante"); got != "clé.inexistante" {
		t.Errorf("clé inconnue = %q", got)
	}
	if got := T("de", "action.cancelled"); got != T(Default, "action.cancelled") {
		t.Errorf("langue inconnue = %q, repli sur le défaut attendu", got)
	}
	if !Has("ingress.fallback_reply") || Has("rien.du.tout") {
		t.Error("Has ne reconnaît pas le catalogue de référence")
	}
}

func readCatalog(t *testing.T, locale Locale) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("locales", string(locale)+".json"))
	if err != nil {
		t.Fatalf("lecture du catalogue %s: %v", locale, err)
	}
	var messages map[string]string
	if err := json.Unmarshal(raw, &messages); err != nil {
		t.Fatalf("catalogue %s illisible: %v", locale, err)
	}
	return messages
}

// formatVerbs relève les verbes de fmt d'un gabarit, dans l'ordre. « %% »
// n'en est pas un.
func formatVerbs(template string) []string {
	var verbs []string
	for i := 0; i < len(template); i++ {
		if template[i] != '%' {
			continue
		}
		i++
		if i >= len(template) || template[i] == '%' {
			continue
		}
		// Drapeaux, largeur, précision et index explicite précèdent le verbe.
		for i < len(template) && strings.ContainsRune("+-# 0123456789.[]", rune(template[i])) {
			i++
		}
		if i < len(template) {
			verbs = append(verbs, string(template[i]))
		}
	}
	return verbs
}
