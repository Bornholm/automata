package web

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/bornholm/automata/internal/persistence"
)

// pricing rassemble les réglages économiques effectifs de l'instance : la
// base d'abord (modifiable depuis ADM-08), la configuration YAML ensuite,
// et enfin des défauts raisonnables. Cette cascade permet d'activer
// l'écran de tarification sans réécrire la configuration existante.
type pricing struct {
	USDPerCredit     float64
	WelcomeCredits   int64
	DefaultAllowance int64
	EURPerUSD        float64
	Packs            []persistence.CreditPack
	// Tarifs de repli appliqués aux modèles absents de la grille.
	DefaultInput  float64
	DefaultOutput float64
	// TargetMargin est la marge visée sur la vente de crédits (%).
	TargetMargin float64
}

// CreditCostEUR est ce qu'un crédit doit couvrir de coût réel, converti en
// euros — le plancher sous lequel une offre est vendue à perte.
func (p pricing) CreditCostEUR() float64 {
	return p.USDPerCredit * p.EURPerUSD
}

// UnitMargin retourne la marge d'une offre, en pourcentage du prix payé.
// Une valeur négative signale une vente à perte.
func (p pricing) UnitMargin(credits int64, priceEUR float64) (float64, bool) {
	if credits <= 0 || priceEUR <= 0 {
		return 0, false
	}

	unitPrice := priceEUR / float64(credits)
	if unitPrice <= 0 {
		return 0, false
	}

	return (unitPrice - p.CreditCostEUR()) / unitPrice * 100, true
}

// RecommendedPrice retourne le prix à demander pour un pack afin
// d'atteindre la marge visée, arrondi à l'euro SUPÉRIEUR.
//
// L'arrondi n'est pas cosmétique : « 4 € » se lit et se compare, « 3,67 € »
// donne l'impression d'un tarif calculé à la virgule près par une machine.
// Arrondir vers le haut garantit en outre que la marge obtenue est toujours
// au moins celle visée — jamais en dessous.
//
// Un pack minuscule tombe donc à 1 € même si son coût est bien moindre :
// c'est le plus petit prix affichable, et la marge réelle est simplement
// meilleure qu'attendu.
func (p pricing) RecommendedPrice(credits int64) float64 {
	if credits <= 0 {
		return 0
	}

	margin := p.TargetMargin
	if margin < 0 || margin >= 100 {
		margin = persistence.DefaultTargetMargin
	}

	exact := float64(credits) * p.CreditCostEUR() / (1 - margin/100)

	rounded := math.Ceil(exact)
	if rounded < 1 {
		rounded = 1
	}

	return rounded
}

// defaultEURPerUSD sert d'ordre de grandeur pour comparer des recettes en
// euros à des coûts en dollars. C'est une estimation affichée comme telle,
// jamais une conversion comptable.
const defaultEURPerUSD = 0.92

// pricing lit les réglages effectifs.
func (s *Server) pricing(ctx context.Context, q persistence.Querier) (pricing, error) {
	p := pricing{
		USDPerCredit:     s.cfg.Web.Credits.EffectiveUSDPerCredit(),
		WelcomeCredits:   500,
		DefaultAllowance: 0,
		EURPerUSD:        defaultEURPerUSD,
		DefaultInput:     persistence.FallbackInputPrice,
		DefaultOutput:    persistence.FallbackOutputPrice,
		TargetMargin:     persistence.DefaultTargetMargin,
	}

	for key, target := range map[string]*float64{
		persistence.SettingUSDPerCredit:       &p.USDPerCredit,
		persistence.SettingEURPerUSD:          &p.EURPerUSD,
		persistence.SettingDefaultInputPrice:  &p.DefaultInput,
		persistence.SettingDefaultOutputPrice: &p.DefaultOutput,
		persistence.SettingTargetMargin:       &p.TargetMargin,
	} {
		value, found, err := s.pricingRepo.GetSetting(ctx, q, key)
		if err != nil {
			return pricing{}, err
		}
		if found {
			if parsed, err := strconv.ParseFloat(value, 64); err == nil && parsed > 0 {
				*target = parsed
			}
		}
	}

	for key, target := range map[string]*int64{
		persistence.SettingWelcomeCredits:   &p.WelcomeCredits,
		persistence.SettingDefaultAllowance: &p.DefaultAllowance,
	} {
		value, found, err := s.pricingRepo.GetSetting(ctx, q, key)
		if err != nil {
			return pricing{}, err
		}
		if found {
			if parsed, err := strconv.ParseInt(value, 10, 64); err == nil && parsed >= 0 {
				*target = parsed
			}
		}
	}

	packs, err := s.pricingRepo.ListPacks(ctx, q)
	if err != nil {
		return pricing{}, err
	}

	// Aucun pack en base : ceux de la configuration font foi, le temps que
	// l'exploitant les reprenne en main depuis l'écran de tarification.
	if len(packs) == 0 {
		for i, pack := range s.cfg.Web.Credits.Packs {
			packs = append(packs, persistence.CreditPack{
				ID:       int64(-(i + 1)),
				Credits:  pack.Credits,
				PriceEUR: pack.PriceEUR,
				Featured: pack.Featured,
				Position: i,
			})
		}
	}
	p.Packs = packs

	return p, nil
}

// packByID retrouve un pack parmi ceux proposés.
func (p pricing) packByID(id int64) (persistence.CreditPack, bool) {
	for _, pack := range p.Packs {
		if pack.ID == id {
			return pack, true
		}
	}
	return persistence.CreditPack{}, false
}

// margin décrit l'économie de la période : ce que les crédits ont
// rapporté, ce que leur usage a réellement coûté, et l'écart.
type margin struct {
	SoldCredits  int64
	SoldEUR      float64
	GivenCredits int64
	UsedCredits  int64
	// CostUSD est le coût d'inférence réellement mesuré (usage_records).
	CostUSD float64
	// CostEUR est son estimation en euros, au taux configuré.
	CostEUR float64
	// MarginEUR est la différence entre recettes et coût estimé.
	MarginEUR float64
	// CoveredCalls / ReportedCalls disent quelle part du coût est
	// réellement rapportée par les fournisseurs : le reste n'est pas
	// mesuré, et la marge est donc optimiste d'autant.
	Calls         int64
	ReportedCalls int64
}

// computeMargin agrège recettes et coûts sur la période.
func (s *Server) computeMargin(ctx context.Context, q persistence.Querier, p pricing, from, to time.Time) (margin, error) {
	revenue, err := s.pricingRepo.AggregateRevenue(ctx, q, from, to)
	if err != nil {
		return margin{}, err
	}

	aggregates, err := s.usage.AggregateUsage(ctx, q, from, to, nil, persistence.UsageFilter{})
	if err != nil {
		return margin{}, err
	}

	m := margin{
		SoldCredits:  revenue.SoldCredits,
		SoldEUR:      revenue.SoldEUR,
		GivenCredits: revenue.GivenCredits,
		UsedCredits:  revenue.UsedCredits,
	}
	for _, agg := range aggregates {
		m.CostUSD += agg.CostAmount
		m.Calls += agg.Calls
		m.ReportedCalls += agg.ReportedCalls
	}

	m.CostEUR = m.CostUSD * p.EURPerUSD
	m.MarginEUR = m.SoldEUR - m.CostEUR

	return m, nil
}

// Ratio décrit la marge en proportion des recettes, ou dit qu'il n'y en a
// pas eu — un pourcentage calculé sur zéro recette n'a aucun sens.
func (m margin) Ratio() string {
	if m.SoldEUR <= 0 {
		return "aucune recette"
	}
	return fmt.Sprintf("%.0f %% des recettes", m.MarginEUR/m.SoldEUR*100)
}
