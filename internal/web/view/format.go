// Package view contient les vues templ du serveur web (admin + profil) et
// leurs modèles de présentation. Les handlers (internal/web) construisent
// des structs de page ; les vues ne contiennent aucune logique métier.
package view

import (
	"fmt"
	"strings"
	"time"
)

// nbsp est l'espace fine insécable utilisée comme séparateur de milliers
// (convention typographique française, reprise des maquettes : « 3 180 »).
const thinSpace = " "

// FormatInt rend n avec séparateur de milliers à la française.
func FormatInt(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}

	digits := fmt.Sprintf("%d", n)
	var b strings.Builder
	for i, d := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteString(thinSpace)
		}
		b.WriteRune(d)
	}

	if neg {
		return "−" + b.String()
	}
	return b.String()
}

// FormatCredits rend un montant en crédits : « 3 180 cr. ».
func FormatCredits(n int64) string {
	return FormatInt(n) + thinSpace + "cr."
}

// FormatSignedCredits rend un mouvement signé : « +2 500 » / « −128 »
// (colonne MOUVEMENT d'ADM-03).
func FormatSignedCredits(n int64) string {
	if n > 0 {
		return "+" + FormatInt(n)
	}
	return FormatInt(n)
}

// FormatShortDate rend une date courte à la française : « 12/03/26 ».
func FormatShortDate(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("02/01/06")
}

// FormatDayTime rend « 18/08 09:12 » (mouvements du portefeuille).
func FormatDayTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("02/01 15:04")
}

// frMonths donne les noms de mois français pour les historiques (PRO-02).
var frMonths = [...]string{"janvier", "février", "mars", "avril", "mai", "juin",
	"juillet", "août", "septembre", "octobre", "novembre", "décembre"}

// FormatMonth rend le nom du mois, capitalisé : « Août ».
func FormatMonth(t time.Time) string {
	name := frMonths[t.Month()-1]
	return strings.ToUpper(name[:1]) + name[1:]
}

// HumanUsageDuration traduit un solde et un rythme quotidien en durée
// parlée — l'annotation des maquettes impose d'énoncer le solde en durée
// avant le chiffre. rate <= 0 (aucune consommation mesurée) rend une
// formule neutre.
func HumanUsageDuration(balance int64, dailyRate float64) string {
	if balance <= 0 {
		return "Solde épuisé"
	}
	if dailyRate <= 0 {
		return "De quoi voir venir"
	}

	days := float64(balance) / dailyRate
	switch {
	case days < 1:
		return "Moins d'un jour d'usage courant"
	case days < 2:
		return "Environ un jour d'usage courant"
	case days < 7:
		return fmt.Sprintf("Environ %d jours d'usage courant", int(days))
	case days < 10:
		return "Environ une semaine d'usage courant"
	case days < 18:
		return "Environ deux semaines d'usage courant"
	case days < 25:
		return "Environ trois semaines d'usage courant"
	case days < 45:
		return "Environ un mois d'usage courant"
	case days < 75:
		return "Environ deux mois d'usage courant"
	default:
		return "Plusieurs mois d'usage courant"
	}
}

// HumanPackDuration décrit la durée approximative d'un pack au rythme
// donné (« Environ un mois », PRO-02).
func HumanPackDuration(credits int64, dailyRate float64) string {
	if dailyRate <= 0 {
		return ""
	}

	days := float64(credits) / dailyRate
	switch {
	case days < 5:
		return fmt.Sprintf("Environ %d jours", int(days))
	case days < 10:
		return "Environ une semaine"
	case days < 20:
		return "Environ deux semaines"
	case days < 45:
		return "Environ un mois"
	case days < 100:
		return "Environ deux mois"
	default:
		return "Environ trois mois ou plus"
	}
}

// GaugePercent borne une fraction en pourcentage d'affichage [0, 100].
func GaugePercent(value, reference int64) int {
	if reference <= 0 || value <= 0 {
		return 0
	}
	pct := int(value * 100 / reference)
	if pct > 100 {
		pct = 100
	}
	if pct == 0 {
		pct = 1
	}
	return pct
}

// itoa évite d'importer strconv dans les vues templ.
func itoa(n int) string { return fmt.Sprintf("%d", n) }

// FormatPlainInt rend n sans séparateur (valeur d'un champ de formulaire).
func FormatPlainInt(n int64) string { return fmt.Sprintf("%d", n) }

// boolFlip rend la valeur inverse d'un booléen pour un bouton de bascule.
func boolFlip(b bool) string {
	if b {
		return "false"
	}
	return "true"
}

// FirstName extrait le prénom d'un nom affiché, pour les tournures des
// emplacements en pointillés et les consignes de transmission d'ADM-04.
func FirstName(displayName string) string {
	if i := strings.IndexByte(displayName, ' '); i > 0 {
		return displayName[:i]
	}
	return displayName
}

// firstName est l'alias interne utilisé par les vues templ.
func firstName(displayName string) string { return FirstName(displayName) }
