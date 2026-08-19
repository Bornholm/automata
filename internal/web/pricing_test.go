package web

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestUsageQuery_DefaultsToCurrentMonthByOrg(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	r := httptest.NewRequest("GET", "/admin/usage", nil)

	from, to, dimensions := usageQuery(r, now)

	if from.Format("2006-01-02") != "2026-08-01" || to.Format("2006-01-02") != "2026-09-01" {
		t.Errorf("période par défaut inattendue: %s → %s", from.Format("2006-01-02"), to.Format("2006-01-02"))
	}
	if len(dimensions) != 1 || dimensions[0] != "org" {
		t.Errorf("dimensions par défaut inattendues: %v", dimensions)
	}
}

func TestUsageQuery_ReadsFiltersAndRejectsUnknownDimensions(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	r := httptest.NewRequest("GET", "/admin/usage?from=2026-07-01&to=2026-08-01&by=agent&by=model&by=DROP", nil)

	from, to, dimensions := usageQuery(r, now)

	if from.Format("2006-01-02") != "2026-07-01" || to.Format("2006-01-02") != "2026-08-01" {
		t.Errorf("période inattendue: %s → %s", from.Format("2006-01-02"), to.Format("2006-01-02"))
	}
	// Une dimension hors liste blanche est ignorée : elle serait sinon
	// interpolée dans la requête SQL par le repository.
	if len(dimensions) != 2 || dimensions[0] != "agent" || dimensions[1] != "model" {
		t.Errorf("dimensions inattendues: %v", dimensions)
	}
}

// Une date illisible ne doit pas vider l'écran : on retombe sur le mois
// courant plutôt que d'afficher une période absurde.
func TestUsageQuery_IgnoresUnparsableDates(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	r := httptest.NewRequest("GET", "/admin/usage?from=hier&to=demain", nil)

	from, to, _ := usageQuery(r, now)

	if from.Format("2006-01-02") != "2026-08-01" || to.Format("2006-01-02") != "2026-09-01" {
		t.Errorf("période inattendue: %s → %s", from.Format("2006-01-02"), to.Format("2006-01-02"))
	}
}

func TestTrimFloat_KeepsInputReadable(t *testing.T) {
	for value, expected := range map[float64]string{
		0.001: "0.001",
		0.92:  "0.92",
		1:     "1",
	} {
		if got := trimFloat(value); got != expected {
			t.Errorf("trimFloat(%v) = %q, attendu %q", value, got, expected)
		}
	}
}

// La marge d'une offre doit être calculable avant publication : c'est ce
// qui distingue un tarif viable d'un tarif qui creuse le résultat.
func TestPricing_UnitMarginAndRecommendedPrice(t *testing.T) {
	p := pricing{USDPerCredit: 0.001, EURPerUSD: 0.92, TargetMargin: 60}

	// Un crédit coûte 0,00092 € ; vendu 0,00795 € (4 400 crédits à 35 €),
	// la marge est d'environ 88 %.
	margin, ok := p.UnitMargin(4400, 35)
	if !ok {
		t.Fatal("la marge d'une offre valide doit être calculable")
	}
	if margin < 87 || margin > 89 {
		t.Errorf("marge %.1f %%, attendue autour de 88 %%", margin)
	}

	// Une offre vendue sous son coût doit ressortir négative, pas nulle.
	loss, ok := p.UnitMargin(10000, 5)
	if !ok || loss >= 0 {
		t.Errorf("marge %.1f %%, attendue négative pour une vente à perte", loss)
	}

	// Le prix conseillé atteint exactement la marge visée.
	recommended := p.RecommendedPrice(4400)
	check, _ := p.UnitMargin(4400, recommended)
	if check < 59.9 || check > 60.1 {
		t.Errorf("le prix conseillé (%.2f €) donne %.1f %% de marge, attendu 60 %%", recommended, check)
	}
}

func TestPricing_UnitMarginRejectsMeaninglessOffers(t *testing.T) {
	p := pricing{USDPerCredit: 0.001, EURPerUSD: 0.92, TargetMargin: 60}

	if _, ok := p.UnitMargin(0, 35); ok {
		t.Error("une offre sans crédit n'a pas de marge calculable")
	}
	if _, ok := p.UnitMargin(1000, 0); ok {
		t.Error("une offre gratuite n'a pas de marge calculable")
	}
}
