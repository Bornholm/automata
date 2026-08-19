package persistence

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Clés des tarifs de repli appliqués aux modèles absents de la grille.
const (
	SettingDefaultInputPrice  = "credits.default_input_usd_per_million"
	SettingDefaultOutputPrice = "credits.default_output_usd_per_million"
)

// Tarifs de repli par défaut, en dollars par million de tokens. Ils sont
// délibérément supérieurs aux modèles économiques courants : sous-estimer
// un coût le fait disparaître de la facturation, tandis qu'une
// surestimation reste visible et corrigeable depuis la grille.
const (
	FallbackInputPrice  = 1.0
	FallbackOutputPrice = 3.0
)

// ModelPrice est le tarif d'un modèle (table model_prices).
type ModelPrice struct {
	Model            string
	InputPerMillion  float64
	OutputPerMillion float64
	UpdatedAt        time.Time
}

// ModelPriceRepository donne accès à la grille tarifaire.
type ModelPriceRepository struct{}

// NewModelPriceRepository crée un ModelPriceRepository.
func NewModelPriceRepository() *ModelPriceRepository {
	return &ModelPriceRepository{}
}

// List retourne la grille, triée par modèle.
func (r *ModelPriceRepository) List(ctx context.Context, q Querier) ([]ModelPrice, error) {
	rows, err := q.QueryContext(ctx, `SELECT model, input_usd_per_million, output_usd_per_million, updated_at
		FROM model_prices ORDER BY model`)
	if err != nil {
		return nil, fmt.Errorf("liste des tarifs par modèle: %w", err)
	}
	defer rows.Close()

	var prices []ModelPrice
	for rows.Next() {
		var (
			price     ModelPrice
			updatedAt string
		)
		if err := rows.Scan(&price.Model, &price.InputPerMillion, &price.OutputPerMillion, &updatedAt); err != nil {
			return nil, fmt.Errorf("lecture d'un tarif: %w", err)
		}
		if price.UpdatedAt, err = parseTenantTime(updatedAt); err != nil {
			return nil, err
		}
		prices = append(prices, price)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des tarifs: %w", err)
	}

	return prices, nil
}

// Upsert enregistre ou remplace le tarif d'un modèle.
func (r *ModelPriceRepository) Upsert(ctx context.Context, q Querier, price ModelPrice) error {
	_, err := q.ExecContext(ctx, `INSERT INTO model_prices
		(model, input_usd_per_million, output_usd_per_million, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(model) DO UPDATE SET
			input_usd_per_million = excluded.input_usd_per_million,
			output_usd_per_million = excluded.output_usd_per_million,
			updated_at = excluded.updated_at`,
		price.Model, price.InputPerMillion, price.OutputPerMillion, formatTenantTime(price.UpdatedAt))
	if err != nil {
		return fmt.Errorf("enregistrement du tarif du modèle %q: %w", price.Model, err)
	}

	return nil
}

// Delete retire le tarif d'un modèle.
func (r *ModelPriceRepository) Delete(ctx context.Context, q Querier, model string) error {
	if _, err := q.ExecContext(ctx, `DELETE FROM model_prices WHERE model = ?`, model); err != nil {
		return fmt.Errorf("suppression du tarif du modèle %q: %w", model, err)
	}

	return nil
}

// PriceTable est la grille chargée en mémoire, avec ses tarifs de repli.
type PriceTable struct {
	prices        []ModelPrice
	defaultInput  float64
	defaultOutput float64
}

// NewPriceTable assemble une grille exploitable.
func NewPriceTable(prices []ModelPrice, defaultInput, defaultOutput float64) PriceTable {
	if defaultInput <= 0 {
		defaultInput = FallbackInputPrice
	}
	if defaultOutput <= 0 {
		defaultOutput = FallbackOutputPrice
	}

	return PriceTable{prices: prices, defaultInput: defaultInput, defaultOutput: defaultOutput}
}

// Lookup retourne les tarifs applicables à un modèle : correspondance
// exacte d'abord, puis la plus longue correspondance de préfixe (une
// entrée « deepseek/ » couvre toute la famille), puis les tarifs de repli.
func (t PriceTable) Lookup(model string) (input, output float64) {
	best := -1
	for i, price := range t.prices {
		switch {
		case price.Model == model:
			return price.InputPerMillion, price.OutputPerMillion
		case strings.HasPrefix(model, price.Model):
			if best < 0 || len(price.Model) > len(t.prices[best].Model) {
				best = i
			}
		}
	}

	if best >= 0 {
		return t.prices[best].InputPerMillion, t.prices[best].OutputPerMillion
	}

	return t.defaultInput, t.defaultOutput
}

// EstimateUSD estime le coût d'un appel à partir de ses volumes de tokens.
// C'est le filet qui empêche un appel non facturé par le fournisseur de
// passer gratuitement : mieux vaut une estimation approchée qu'un zéro.
func (t PriceTable) EstimateUSD(model string, promptTokens, completionTokens int64) float64 {
	input, output := t.Lookup(model)

	return (float64(promptTokens)*input + float64(completionTokens)*output) / 1_000_000
}
