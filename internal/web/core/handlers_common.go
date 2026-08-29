package core

import (
	"context"
	"strconv"
	"time"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/web/view"
)

// LowBalanceRatio définit le seuil « solde faible » : moins de 15 % du
// dernier apport (référence de jauge des maquettes).
const LowBalanceRatio = 0.15

// WalletState résume l'état d'un portefeuille pour les chips et jauges.
// State : ok | low | empty | offered.
type WalletState struct {
	State     string
	Balance   int64
	GaugeRef  int64
	GaugePct  int
	GaugeTone string
	Chip      view.Chip
	RowTone   string
	// BalanceLabel est le libellé court d'ADM-02.
	BalanceLabel string
}

// ComputeWalletState calcule l'état d'une organisation. Pour une
// organisation offerte, la jauge mesure l'usage du mois face à
// l'allocation ; pour une payante, le solde face au dernier apport.
func ComputeWalletState(org persistence.Organization, balance, lastCredit, monthUsage int64) WalletState {
	// Le mode sans limite passe avant tout le reste : cette organisation
	// n'est jamais débitée, son solde n'a donc rien à dire.
	if org.Unlimited {
		return WalletState{
			State:        "unlimited",
			Balance:      balance,
			GaugeTone:    "brand",
			Chip:         view.Chip{Label: "Offerte — sans limite", Tone: "brand", Dot: true},
			BalanceLabel: "Sans limite",
		}
	}

	if org.Offered {
		return WalletState{
			State:        "offered",
			Balance:      balance,
			GaugeRef:     org.MonthlyAllowance,
			GaugePct:     view.GaugePercent(monthUsage, org.MonthlyAllowance),
			GaugeTone:    "brand",
			Chip:         view.Chip{Label: "Offerte", Tone: "brand", Dot: true},
			BalanceLabel: view.FormatCredits(org.MonthlyAllowance) + "/mois",
		}
	}

	switch {
	case balance <= 0:
		return WalletState{
			State:     "empty",
			Balance:   balance,
			GaugeRef:  lastCredit,
			GaugePct:  0,
			GaugeTone: "crit",
			Chip:      view.Chip{Label: "En pause — solde épuisé", Tone: "crit", Dot: true},
			RowTone:   "crit",
		}
	case lastCredit > 0 && float64(balance) < float64(lastCredit)*LowBalanceRatio:
		return WalletState{
			State:        "low",
			Balance:      balance,
			GaugeRef:     lastCredit,
			GaugePct:     view.GaugePercent(balance, lastCredit),
			GaugeTone:    "warn",
			Chip:         view.Chip{Label: "Solde faible", Tone: "warn", Dot: true},
			RowTone:      "warn",
			BalanceLabel: view.FormatCredits(balance),
		}
	default:
		return WalletState{
			State:        "ok",
			Balance:      balance,
			GaugeRef:     lastCredit,
			GaugePct:     view.GaugePercent(balance, lastCredit),
			GaugeTone:    "brand",
			Chip:         view.Chip{Label: "Créditée", Tone: "ok", Dot: true},
			BalanceLabel: view.FormatCredits(balance),
		}
	}
}

// usageCredits convertit un coût mesuré (USD, usage_records) en crédits.
// rate vient des réglages de l'instance (écran de tarification) ; zéro
// retombe sur la configuration.
func (s *Deps) UsageCredits(cost float64, rate float64) int64 {
	if rate <= 0 {
		rate = s.Cfg.Web.Credits.EffectiveUSDPerCredit()
	}
	return int64(cost / rate)
}

// creditRate lit le taux de conversion effectif.
func (s *Deps) CreditRate(ctx context.Context, q persistence.Querier) float64 {
	value, found, err := s.PricingRepo.GetSetting(ctx, q, persistence.SettingUSDPerCredit)
	if err == nil && found {
		if parsed, parseErr := strconv.ParseFloat(value, 64); parseErr == nil && parsed > 0 {
			return parsed
		}
	}
	return s.Cfg.Web.Credits.EffectiveUSDPerCredit()
}

// orgUsageCredits retourne la consommation d'une organisation sur la
// période, en crédits. orgID vide = toutes organisations (par org).
func (s *Deps) OrgUsageCredits(ctx context.Context, q persistence.Querier, from, to time.Time) (map[string]int64, error) {
	aggregates, err := s.Usage.AggregateUsage(ctx, q, from, to, []string{"org"}, persistence.UsageFilter{})
	if err != nil {
		return nil, err
	}

	rate := s.CreditRate(ctx, q)
	usageByOrg := map[string]int64{}
	for _, agg := range aggregates {
		usageByOrg[agg.Keys[0]] += s.UsageCredits(agg.CostAmount, rate)
	}

	return usageByOrg, nil
}

// singleOrgUsageCredits retourne la consommation d'une seule organisation
// sur la période, en crédits.
func (s *Deps) SingleOrgUsageCredits(ctx context.Context, q persistence.Querier, orgID string, from, to time.Time) (int64, error) {
	aggregates, err := s.Usage.AggregateUsage(ctx, q, from, to, nil, persistence.UsageFilter{OrgID: orgID})
	if err != nil {
		return 0, err
	}

	rate := s.CreditRate(ctx, q)

	var total int64
	for _, agg := range aggregates {
		total += s.UsageCredits(agg.CostAmount, rate)
	}

	return total, nil
}

// MonthBounds retourne le mois civil courant [début, fin).
func MonthBounds(now time.Time) (time.Time, time.Time) {
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return from, from.AddDate(0, 1, 0)
}

// dailyRate estime le rythme de consommation (crédits/jour) sur les 7
// derniers jours.
func (s *Deps) DailyRate(ctx context.Context, q persistence.Querier, orgID string, now time.Time) (float64, error) {
	total, err := s.SingleOrgUsageCredits(ctx, q, orgID, now.AddDate(0, 0, -7), now)
	if err != nil {
		return 0, err
	}
	return float64(total) / 7, nil
}

// PlatformDisplayName traduit un type de provider en nom de plateforme
// affiché ; retombe sur le nom configuré pour un type inconnu.
func PlatformDisplayName(providerType, fallback string) string {
	switch providerType {
	case "whatsapp":
		return "WhatsApp"
	case "signal":
		return "Signal"
	case "rocket":
		return "Rocket.Chat"
	case "discord":
		return "Discord"
	case "mail":
		return "Courriel"
	default:
		return fallback
	}
}

// providerTypeOf retourne le type du compte de messagerie nommé, d'après
// l'état vivant du gestionnaire de plateformes — les comptes ne se
// déclarent plus dans le fichier de configuration.
func (s *Deps) ProviderTypeOf(providerName string) string {
	if s.PlatformMgr == nil {
		return ""
	}
	if status, ok := s.PlatformMgr.Statuses()[providerName]; ok {
		return status.Type
	}
	return ""
}

// ChannelKindLabel traduit un genre de canal de la config.
func ChannelKindLabel(kind config.ChannelKind) string {
	if kind == config.ChannelKindGroup {
		return "Groupe"
	}
	return "Privé"
}

// orgDisplayName résout le nom affiché d'une organisation : la base
// d'abord (source SaaS), la configuration sinon (organisations pas encore
// bootstrapées).
func (s *Deps) OrgDisplayName(ctx context.Context, q persistence.Querier, orgID string) string {
	if org, found, err := s.Orgs.FindByID(ctx, q, orgID); err == nil && found {
		return org.DisplayName
	}
	if name := s.Cfg.OrganizationDisplayName(orgID); name != "" {
		return name
	}
	return orgID
}

// channelDisplayName retourne un libellé lisible pour un canal de la
// configuration : son nom affiché s'il en a un, sinon le nom de la
// personne dont c'est la conversation privée. Un identifiant de messagerie
// brut n'apprend rien à l'écran et n'a pas sa place dans une interface.
func (s *Deps) ChannelDisplayName(ch config.Channel) string {
	if ch.DisplayName != "" {
		return ch.DisplayName
	}

	if ch.Kind == config.ChannelKindGroup {
		return "Groupe sans nom"
	}

	if ch.PrincipalID != "" {
		for _, principal := range s.Cfg.Identities.Principals {
			if principal.ID == ch.PrincipalID && principal.DisplayName != "" {
				return principal.DisplayName
			}
		}
	}

	return "Conversation privée"
}
