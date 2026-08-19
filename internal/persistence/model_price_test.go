package persistence_test

import (
	"math"
	"testing"

	"github.com/bornholm/automata/internal/persistence"
)

func table() persistence.PriceTable {
	return persistence.NewPriceTable([]persistence.ModelPrice{
		{Model: "deepseek/", InputPerMillion: 0.14, OutputPerMillion: 0.28},
		{Model: "deepseek/deepseek-v4-flash-0731", InputPerMillion: 0.05, OutputPerMillion: 0.10},
		{Model: "openai/gpt-5", InputPerMillion: 2, OutputPerMillion: 8},
	}, 0, 0)
}

// La correspondance exacte prime sur la famille : un modèle précisément
// tarifé ne doit pas être facturé au tarif de sa famille.
func TestPriceTable_PrefersExactModel(t *testing.T) {
	input, output := table().Lookup("deepseek/deepseek-v4-flash-0731")
	if input != 0.05 || output != 0.10 {
		t.Errorf("tarifs %v/%v, attendus 0.05/0.10", input, output)
	}
}

// Une entrée de famille couvre les modèles qu'on n'a pas listés un par un.
func TestPriceTable_FallsBackToLongestPrefix(t *testing.T) {
	input, output := table().Lookup("deepseek/deepseek-v5-pro")
	if input != 0.14 || output != 0.28 {
		t.Errorf("tarifs %v/%v, attendus 0.14/0.28 (famille)", input, output)
	}
}

// Un modèle inconnu retombe sur les tarifs de repli — délibérément
// généreux : un coût sous-estimé disparaît de la facturation.
func TestPriceTable_UnknownModelUsesFallback(t *testing.T) {
	input, output := table().Lookup("un/modele-inconnu")
	if input != persistence.FallbackInputPrice || output != persistence.FallbackOutputPrice {
		t.Errorf("tarifs %v/%v, attendus les valeurs de repli", input, output)
	}
}

func TestPriceTable_EstimateUSD(t *testing.T) {
	// 1 000 000 tokens d'entrée à 0,05 $ + 500 000 de sortie à 0,10 $.
	got := table().EstimateUSD("deepseek/deepseek-v4-flash-0731", 1_000_000, 500_000)

	if math.Abs(got-0.10) > 1e-9 {
		t.Errorf("estimation %v, attendue 0.10", got)
	}
}

// Le cas qui motive tout ce mécanisme : un appel réel sans coût rapporté
// ne doit jamais valoir zéro.
func TestPriceTable_EstimateIsNeverZeroForRealUsage(t *testing.T) {
	if got := table().EstimateUSD("un/modele-inconnu", 6000, 66); got <= 0 {
		t.Fatalf("estimation %v, attendue strictement positive", got)
	}
}
