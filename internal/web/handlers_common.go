package web

import (
	"context"
	"strconv"
	"time"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/web/view"
)

// lowBalanceRatio définit le seuil « solde faible » : moins de 15 % du
// dernier apport (référence de jauge des maquettes).
const lowBalanceRatio = 0.15

// walletState résume l'état d'un portefeuille pour les chips et jauges.
// State : ok | low | empty | offered.
type walletState struct {
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

// computeWalletState calcule l'état d'une organisation. Pour une
// organisation offerte, la jauge mesure l'usage du mois face à
// l'allocation ; pour une payante, le solde face au dernier apport.
func computeWalletState(org persistence.Organization, balance, lastCredit, monthUsage int64) walletState {
	// Le mode sans limite passe avant tout le reste : cette organisation
	// n'est jamais débitée, son solde n'a donc rien à dire.
	if org.Unlimited {
		return walletState{
			State:        "unlimited",
			Balance:      balance,
			GaugeTone:    "brand",
			Chip:         view.Chip{Label: "Offerte — sans limite", Tone: "brand", Dot: true},
			BalanceLabel: "Sans limite",
		}
	}

	if org.Offered {
		return walletState{
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
		return walletState{
			State:     "empty",
			Balance:   balance,
			GaugeRef:  lastCredit,
			GaugePct:  0,
			GaugeTone: "crit",
			Chip:      view.Chip{Label: "En pause — solde épuisé", Tone: "crit", Dot: true},
			RowTone:   "crit",
		}
	case lastCredit > 0 && float64(balance) < float64(lastCredit)*lowBalanceRatio:
		return walletState{
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
		return walletState{
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
func (s *Server) usageCredits(cost float64, rate float64) int64 {
	if rate <= 0 {
		rate = s.cfg.Web.Credits.EffectiveUSDPerCredit()
	}
	return int64(cost / rate)
}

// creditRate lit le taux de conversion effectif.
func (s *Server) creditRate(ctx context.Context, q persistence.Querier) float64 {
	value, found, err := s.pricingRepo.GetSetting(ctx, q, persistence.SettingUSDPerCredit)
	if err == nil && found {
		if parsed, parseErr := strconv.ParseFloat(value, 64); parseErr == nil && parsed > 0 {
			return parsed
		}
	}
	return s.cfg.Web.Credits.EffectiveUSDPerCredit()
}

// orgUsageCredits retourne la consommation d'une organisation sur la
// période, en crédits. orgID vide = toutes organisations (par org).
func (s *Server) orgUsageCredits(ctx context.Context, q persistence.Querier, from, to time.Time) (map[string]int64, error) {
	aggregates, err := s.usage.AggregateUsage(ctx, q, from, to, []string{"org"}, persistence.UsageFilter{})
	if err != nil {
		return nil, err
	}

	rate := s.creditRate(ctx, q)
	usageByOrg := map[string]int64{}
	for _, agg := range aggregates {
		usageByOrg[agg.Keys[0]] += s.usageCredits(agg.CostAmount, rate)
	}

	return usageByOrg, nil
}

// singleOrgUsageCredits retourne la consommation d'une seule organisation
// sur la période, en crédits.
func (s *Server) singleOrgUsageCredits(ctx context.Context, q persistence.Querier, orgID string, from, to time.Time) (int64, error) {
	aggregates, err := s.usage.AggregateUsage(ctx, q, from, to, nil, persistence.UsageFilter{OrgID: orgID})
	if err != nil {
		return 0, err
	}

	rate := s.creditRate(ctx, q)

	var total int64
	for _, agg := range aggregates {
		total += s.usageCredits(agg.CostAmount, rate)
	}

	return total, nil
}

// monthBounds retourne le mois civil courant [début, fin).
func monthBounds(now time.Time) (time.Time, time.Time) {
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return from, from.AddDate(0, 1, 0)
}

// dailyRate estime le rythme de consommation (crédits/jour) sur les 7
// derniers jours.
func (s *Server) dailyRate(ctx context.Context, q persistence.Querier, orgID string, now time.Time) (float64, error) {
	total, err := s.singleOrgUsageCredits(ctx, q, orgID, now.AddDate(0, 0, -7), now)
	if err != nil {
		return 0, err
	}
	return float64(total) / 7, nil
}

// platformDisplayName traduit un type de provider en nom de plateforme
// affiché ; retombe sur le nom configuré pour un type inconnu.
func platformDisplayName(providerType, fallback string) string {
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
func (s *Server) providerTypeOf(providerName string) string {
	if s.platformManager == nil {
		return ""
	}
	if status, ok := s.platformManager.Statuses()[providerName]; ok {
		return status.Type
	}
	return ""
}

// channelKindLabel traduit un genre de canal de la config.
func channelKindLabel(kind config.ChannelKind) string {
	if kind == config.ChannelKindGroup {
		return "Groupe"
	}
	return "Privé"
}

// orgDisplayName résout le nom affiché d'une organisation : la base
// d'abord (source SaaS), la configuration sinon (organisations pas encore
// bootstrapées).
func (s *Server) orgDisplayName(ctx context.Context, q persistence.Querier, orgID string) string {
	if org, found, err := s.orgs.FindByID(ctx, q, orgID); err == nil && found {
		return org.DisplayName
	}
	if name := s.cfg.OrganizationDisplayName(orgID); name != "" {
		return name
	}
	return orgID
}

// channelDisplayName retourne un libellé lisible pour un canal de la
// configuration : son nom affiché s'il en a un, sinon le nom de la
// personne dont c'est la conversation privée. Un identifiant de messagerie
// brut n'apprend rien à l'écran et n'a pas sa place dans une interface.
func (s *Server) channelDisplayName(ch config.Channel) string {
	if ch.DisplayName != "" {
		return ch.DisplayName
	}

	if ch.Kind == config.ChannelKindGroup {
		return "Groupe sans nom"
	}

	if ch.PrincipalID != "" {
		for _, principal := range s.cfg.Identities.Principals {
			if principal.ID == ch.PrincipalID && principal.DisplayName != "" {
				return principal.DisplayName
			}
		}
	}

	return "Conversation privée"
}
