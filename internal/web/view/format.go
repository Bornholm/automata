// Package view contient les vues templ du serveur web (admin + profil) et
// leurs modèles de présentation. Les handlers (internal/web) construisent
// des structs de page ; les vues ne contiennent aucune logique métier.
package view

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bornholm/automata/internal/i18n"
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

// FormatMonth rend le nom du mois, capitalisé : « Août ».
//
// La capitalisation est faite ici plutôt que dans les catalogues parce que
// les appelants en ont besoin dans les deux sens : « Août » en titre de
// colonne, « le 1ᵉʳ août » au fil d'une phrase. Les langues ne s'accordent
// d'ailleurs pas sur la casse d'un nom de mois — l'anglais le capitalise
// toujours, le français et l'espagnol jamais — et c'est le catalogue qui
// porte cette règle-là.
func FormatMonth(ctx context.Context, t time.Time) string {
	name := i18n.TC(ctx, "month."+strconv.Itoa(int(t.Month())))
	return strings.ToUpper(name[:1]) + name[1:]
}

// HumanUsageDuration traduit un solde et un rythme quotidien en durée
// parlée — l'annotation des maquettes impose d'énoncer le solde en durée
// avant le chiffre. rate <= 0 (aucune consommation mesurée) rend une
// formule neutre.
func HumanUsageDuration(ctx context.Context, balance int64, dailyRate float64) string {
	if balance <= 0 {
		return i18n.TC(ctx, "duration.balance.empty")
	}
	if dailyRate <= 0 {
		return i18n.TC(ctx, "duration.balance.unknown")
	}

	days := float64(balance) / dailyRate
	switch {
	case days < 1:
		return i18n.TC(ctx, "duration.usage.less_than_day")
	case days < 2:
		return i18n.TC(ctx, "duration.usage.one_day")
	case days < 7:
		return i18n.TC(ctx, "duration.usage.days", int(days))
	case days < 10:
		return i18n.TC(ctx, "duration.usage.one_week")
	case days < 18:
		return i18n.TC(ctx, "duration.usage.two_weeks")
	case days < 25:
		return i18n.TC(ctx, "duration.usage.three_weeks")
	case days < 45:
		return i18n.TC(ctx, "duration.usage.one_month")
	case days < 75:
		return i18n.TC(ctx, "duration.usage.two_months")
	default:
		return i18n.TC(ctx, "duration.usage.several_months")
	}
}

// HumanPackDuration décrit la durée approximative d'un pack au rythme
// donné (« Environ un mois », PRO-02).
func HumanPackDuration(ctx context.Context, credits int64, dailyRate float64) string {
	if dailyRate <= 0 {
		return ""
	}

	days := float64(credits) / dailyRate
	switch {
	case days < 5:
		return i18n.TC(ctx, "duration.pack.days", int(days))
	case days < 10:
		return i18n.TC(ctx, "duration.pack.one_week")
	case days < 20:
		return i18n.TC(ctx, "duration.pack.two_weeks")
	case days < 45:
		return i18n.TC(ctx, "duration.pack.one_month")
	case days < 100:
		return i18n.TC(ctx, "duration.pack.two_months")
	default:
		return i18n.TC(ctx, "duration.pack.three_months_plus")
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
