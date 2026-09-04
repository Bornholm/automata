// Package i18n porte les textes qu'Automata adresse à une personne sans
// passer par un modèle : messages de repli, propositions d'actions,
// questions de la visite d'accueil, pages web.
//
// Il existe pour une raison qui n'est pas seulement la traduction. La règle
// du dépôt — anglais vers le modèle, français partout ailleurs — ne
// distingue pas un journal d'un message lu par un humain : les deux sont
// des chaînes françaises dans le même fichier. Un appel à T est ce
// marqueur manquant. Ce qui passe par ici s'adresse à quelqu'un ; le reste
// reste du français de service.
//
// Trois langues, pas de bibliothèque. `golang.org/x/text/message` impose sa
// génération de catalogues, `go-i18n` des fichiers que personne ne
// régénère. Ici : trois JSON embarqués, une fonction, et un test qui refuse
// une clé absente d'une des langues — c'est ce test qui empêche l'espagnol
// de retomber silencieusement en français au bout de six mois.
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"strings"
)

// Locale est une des langues servies par l'instance.
type Locale string

const (
	FR Locale = "fr"
	EN Locale = "en"
	ES Locale = "es"
)

// Default est la langue employée quand rien n'est su de l'interlocuteur.
const Default = FR

// Supported énumère les langues servies, dans l'ordre où on les propose.
var Supported = []Locale{FR, EN, ES}

//go:embed locales/*.json
var catalogFS embed.FS

// catalogs porte les messages par langue, chargés une fois au démarrage.
var catalogs = loadCatalogs()

func loadCatalogs() map[Locale]map[string]string {
	loaded := make(map[Locale]map[string]string, len(Supported))
	for _, locale := range Supported {
		raw, err := catalogFS.ReadFile("locales/" + string(locale) + ".json")
		if err != nil {
			// Le fichier est embarqué : son absence est une erreur de
			// construction, pas une condition d'exécution.
			panic(fmt.Sprintf("i18n: catalogue %s introuvable: %v", locale, err))
		}
		var messages map[string]string
		if err := json.Unmarshal(raw, &messages); err != nil {
			panic(fmt.Sprintf("i18n: catalogue %s illisible: %v", locale, err))
		}
		loaded[locale] = messages
	}
	return loaded
}

// Parse reconnaît une étiquette de langue, telle qu'elle vient d'une
// configuration, d'une colonne ou d'un en-tête Accept-Language : « es »,
// « es-ES », « ES_es » désignent la même chose. ok vaut false pour une
// langue que l'instance ne sert pas — l'appelant décide alors s'il refuse
// ou s'il retombe sur Default.
func Parse(raw string) (Locale, bool) {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	// Une étiquette BCP 47 porte parfois une région ou une variante : seule
	// la langue nous intéresse, nous n'avons pas de catalogue régional.
	if cut := strings.IndexAny(normalized, "-_."); cut > 0 {
		normalized = normalized[:cut]
	}

	for _, locale := range Supported {
		if normalized == string(locale) {
			return locale, true
		}
	}
	return "", false
}

// Resolve rend une langue servie, quoi qu'on lui passe. C'est la forme à
// employer sur un chemin d'exécution : une locale inconnue en base ou en
// configuration ne doit jamais faire échouer un message.
func Resolve(raw string) Locale {
	if locale, ok := Parse(raw); ok {
		return locale
	}
	return Default
}

// T rend le message key dans la langue locale, formaté avec args selon les
// verbes de fmt.
//
// Une clé absente retombe sur Default, puis sur la clé elle-même : un
// message manquant doit dégrader l'affichage, jamais interrompre une
// conversation. Le test de complétude du paquet garantit que ce repli ne
// sert pas.
func T(locale Locale, key string, args ...any) string {
	template, ok := catalogs[locale][key]
	if !ok {
		if template, ok = catalogs[Default][key]; !ok {
			return key
		}
	}
	if len(args) == 0 {
		return template
	}
	return fmt.Sprintf(template, args...)
}

// Has indique si une clé existe dans le catalogue de référence. Réservé aux
// tests et aux vérifications de démarrage.
func Has(key string) bool {
	_, ok := catalogs[Default][key]
	return ok
}
