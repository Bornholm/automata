package persistence

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bornholm/automata/internal/usage"
)

// UsageRecordRepository donne accès à la table usage_records : la trace
// comptable de chaque appel d'inférence (voir la migration 0009 et
// internal/usage).
type UsageRecordRepository struct{}

// NewUsageRecordRepository crée un UsageRecordRepository.
func NewUsageRecordRepository() *UsageRecordRepository {
	return &UsageRecordRepository{}
}

// Insert enregistre rec.
func (r *UsageRecordRepository) Insert(ctx context.Context, q Querier, rec usage.Record) error {
	costReported := 0
	if rec.CostReported {
		costReported = 1
	}

	_, err := q.ExecContext(ctx, `
		INSERT INTO usage_records (
			created_at, org_id, principal_id, conversation_id, component, agent,
			kind, provider, model,
			prompt_tokens, completion_tokens, total_tokens, cached_tokens,
			cost_amount, cost_currency, cost_reported
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		rec.CreatedAt.UTC().Format(time.RFC3339), rec.OrgID, rec.PrincipalID, rec.ConversationID, rec.Component, rec.Agent,
		rec.Kind, rec.Provider, rec.Model,
		rec.PromptTokens, rec.CompletionTokens, rec.TotalTokens, rec.CachedTokens,
		rec.CostAmount, rec.CostCurrency, costReported,
	)
	if err != nil {
		return fmt.Errorf("insertion d'une trace d'usage: %w", err)
	}

	return nil
}

// usageGroupColumns associe chaque dimension d'agrégation acceptée à son
// expression SQL. Liste blanche stricte : la dimension est interpolée dans
// la requête, jamais une valeur venue de l'utilisateur.
var usageGroupColumns = map[string]string{
	"org":          "org_id",
	"principal":    "principal_id",
	"conversation": "conversation_id",
	"component":    "component",
	"agent":        "agent",
	"kind":         "kind",
	"provider":     "provider",
	"model":        "model",
	"day":          "date(created_at)",
	"month":        "strftime('%Y-%m', created_at)",
}

// UsageGroupKeys retourne les dimensions d'agrégation acceptées, triées,
// pour les messages d'erreur.
func UsageGroupKeys() []string {
	keys := make([]string, 0, len(usageGroupColumns))
	for key := range usageGroupColumns {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// UsageAggregate est une ligne d'agrégation : les valeurs des dimensions
// demandées (dans l'ordre de groupBy), puis les totaux. Currency est
// toujours une dimension implicite : additionner des montants de devises
// différentes n'aurait pas de sens.
type UsageAggregate struct {
	Keys     []string
	Currency string

	Calls            int64
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	CachedTokens     int64
	CostAmount       float64
	// ReportedCalls compte les appels dont le coût a été rapporté par le
	// provider : s'il est inférieur à Calls, CostAmount sous-estime le coût
	// réel (des appels ne portent que des tokens).
	ReportedCalls int64
}

// AggregateUsage agrège les traces de la période [from, to) selon les
// dimensions groupBy (voir usageGroupColumns). Les lignes sont triées par
// coût décroissant, puis tokens décroissants.
func (r *UsageRecordRepository) AggregateUsage(ctx context.Context, q Querier, from, to time.Time, groupBy []string) ([]UsageAggregate, error) {
	exprs := make([]string, 0, len(groupBy))
	for _, key := range groupBy {
		expr, ok := usageGroupColumns[key]
		if !ok {
			return nil, fmt.Errorf("dimension d'agrégation %q inconnue (dimensions: %s)", key, strings.Join(UsageGroupKeys(), ", "))
		}
		exprs = append(exprs, expr)
	}

	selectCols := append(append([]string{}, exprs...), "cost_currency",
		"COUNT(*)", "SUM(prompt_tokens)", "SUM(completion_tokens)", "SUM(total_tokens)",
		"SUM(cached_tokens)", "SUM(cost_amount)", "SUM(cost_reported)")
	groupCols := append(append([]string{}, exprs...), "cost_currency")

	query := "SELECT " + strings.Join(selectCols, ", ") + `
		FROM usage_records
		WHERE created_at >= ? AND created_at < ?
		GROUP BY ` + strings.Join(groupCols, ", ") + `
		ORDER BY SUM(cost_amount) DESC, SUM(total_tokens) DESC`

	rows, err := q.QueryContext(ctx, query, from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("agrégation des traces d'usage: %w", err)
	}
	defer rows.Close()

	var aggregates []UsageAggregate
	for rows.Next() {
		agg := UsageAggregate{Keys: make([]string, len(exprs))}

		dest := make([]any, 0, len(exprs)+8)
		for i := range agg.Keys {
			dest = append(dest, &agg.Keys[i])
		}
		dest = append(dest, &agg.Currency,
			&agg.Calls, &agg.PromptTokens, &agg.CompletionTokens, &agg.TotalTokens,
			&agg.CachedTokens, &agg.CostAmount, &agg.ReportedCalls)

		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("lecture d'une ligne d'agrégation d'usage: %w", err)
		}

		aggregates = append(aggregates, agg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des lignes d'agrégation d'usage: %w", err)
	}

	return aggregates, nil
}
