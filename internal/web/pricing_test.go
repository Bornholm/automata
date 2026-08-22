package web

import (
	"context"
	"database/sql"
	"net/url"

	"github.com/bornholm/automata/internal/persistence"
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
	// Le prix conseillé est arrondi à l'euro supérieur : 10,12 € deviennent
	// 11 €. Un tarif entier se lit et se compare ; un prix au centime près
	// a l'air calculé par une machine.
	recommended := p.RecommendedPrice(4400)
	if recommended != 11 {
		t.Errorf("prix conseillé = %.2f €, attendu 11 € (10,12 € arrondis)", recommended)
	}

	// L'arrondi va toujours vers le haut : la marge obtenue est donc au
	// moins celle visée, jamais en dessous.
	check, _ := p.UnitMargin(4400, recommended)
	if check < 60 {
		t.Errorf("le prix conseillé (%.2f €) donne %.1f %% de marge, moins que les 60 %% visés", recommended, check)
	}
}

// Un pack minuscule coûte moins d'un euro à couvrir : il est vendu 1 €,
// le plus petit prix affichable. La marge est alors meilleure que visée,
// ce qui est le bon sens du compromis.
func TestPricing_RecommendedPriceNeverFallsBelowOneEuro(t *testing.T) {
	p := pricing{USDPerCredit: 0.001, EURPerUSD: 0.92, TargetMargin: 60}

	if got := p.RecommendedPrice(100); got != 1 {
		t.Errorf("prix conseillé pour 100 crédits = %.2f €, attendu 1 €", got)
	}

	// Un nombre de crédits absurde ne doit pas produire de prix.
	if got := p.RecommendedPrice(0); got != 0 {
		t.Errorf("prix conseillé pour 0 crédit = %.2f €, attendu 0", got)
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

// Ajouter une offre sans indiquer de prix est le geste courant : le tarif
// découle du coût réel et de la marge visée. Le calcul qui fait foi est
// celui du serveur, pas l'aperçu affiché pendant la saisie.
func TestPricingPackCreate_ComputesThePriceWhenLeftEmpty(t *testing.T) {
	srv, ts, client := testServer(t)
	login(t, ts, client)

	if _, err := client.Get(ts.URL + "/admin/pricing"); err != nil {
		t.Fatalf("GET tarification: %v", err)
	}

	resp, err := client.PostForm(ts.URL+"/admin/pricing/packs", url.Values{
		"csrf_token": {csrfFrom(t, client, ts.URL+"/admin/pricing")},
		"credits":    {"4400"},
		"price_eur":  {""},
	})
	if err != nil {
		t.Fatalf("POST offre: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var packs []persistence.CreditPack
	if err := srv.db.WithTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		packs, err = srv.pricingRepo.ListPacks(context.Background(), tx)
		return err
	}); err != nil {
		t.Fatalf("relecture des offres: %v", err)
	}

	if len(packs) != 1 {
		t.Fatalf("offres enregistrées: %d, attendu 1", len(packs))
	}
	if packs[0].PriceEUR <= 0 {
		t.Fatalf("le prix devait être calculé, obtenu %.2f €", packs[0].PriceEUR)
	}
	// Un prix entier : c'est tout l'objet de l'arrondi.
	if packs[0].PriceEUR != float64(int64(packs[0].PriceEUR)) {
		t.Errorf("le prix calculé (%.2f €) devrait être un nombre entier d'euros", packs[0].PriceEUR)
	}
}

// Un prix saisi à la main est respecté tel quel : l'automatisme ne doit pas
// écraser une décision commerciale (offre d'appel, tarif rond imposé).
func TestPricingPackCreate_KeepsAnExplicitPrice(t *testing.T) {
	srv, ts, client := testServer(t)
	login(t, ts, client)

	if _, err := client.Get(ts.URL + "/admin/pricing"); err != nil {
		t.Fatalf("GET tarification: %v", err)
	}

	resp, err := client.PostForm(ts.URL+"/admin/pricing/packs", url.Values{
		"csrf_token": {csrfFrom(t, client, ts.URL+"/admin/pricing")},
		"credits":    {"4400"},
		"price_eur":  {"9,50"},
	})
	if err != nil {
		t.Fatalf("POST offre: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var packs []persistence.CreditPack
	if err := srv.db.WithTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		packs, err = srv.pricingRepo.ListPacks(context.Background(), tx)
		return err
	}); err != nil {
		t.Fatalf("relecture des offres: %v", err)
	}

	if len(packs) != 1 || packs[0].PriceEUR != 9.5 {
		t.Fatalf("prix enregistré = %+v, attendu 9,50 €", packs)
	}
}
