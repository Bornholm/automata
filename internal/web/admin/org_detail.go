package admin

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/web/core"
	"github.com/bornholm/automata/internal/web/view"
)

// HandleOrg — ADM-03.
func (h *Handlers) HandleOrg(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("id")
	tab := r.URL.Query().Get("tab")
	if tab != "members" && tab != "channels" && tab != "customization" && tab != "models" && tab != "plugins" {
		tab = "credits"
	}
	now := h.Now()
	monthFrom, monthTo := core.MonthBounds(now)

	page := view.OrgPage{
		Platforms: h.SidebarPlatforms(),
		CSRFToken: h.CSRFToken(w, r),
		ID:        orgID,
		Tab:       tab,
	}

	found := false
	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		org, exists, err := h.Orgs.FindByID(r.Context(), tx, orgID)
		if err != nil || !exists {
			return err
		}
		found = true

		balance, err := h.Wallet.Balance(r.Context(), tx, orgID)
		if err != nil {
			return err
		}
		lastCredit, err := h.Wallet.LastCredit(r.Context(), tx, orgID)
		if err != nil {
			return err
		}
		monthUsage, err := h.SingleOrgUsageCredits(r.Context(), tx, orgID, monthFrom, monthTo)
		if err != nil {
			return err
		}
		members, err := h.Members.ListByOrg(r.Context(), tx, orgID)
		if err != nil {
			return err
		}
		rate, err := h.DailyRate(r.Context(), tx, orgID, now)
		if err != nil {
			return err
		}

		pluginRows, err := h.pluginActivationRows(r, tx, orgID)
		if err != nil {
			return err
		}
		page.PluginRows = pluginRows

		modelRoles, err := h.modelRoleRows(r.Context(), tx, orgID)
		if err != nil {
			return err
		}
		page.ModelRoles = modelRoles

		state := core.ComputeWalletState(org, balance, lastCredit, monthUsage)

		page.Name = org.DisplayName
		page.Chip = state.Chip
		page.Offered = org.Offered
		page.Unlimited = org.Unlimited
		page.Allowance = org.MonthlyAllowance
		page.Balance = balance
		page.GaugePct = state.GaugePct
		page.GaugeTone = state.GaugeTone
		page.GaugeRef = state.GaugeRef
		page.BalanceHint = view.HumanUsageDuration(r.Context(), balance, rate)
		page.AutoTopUp = "Inactive"
		page.DailyRate = "—"
		if rate > 0 {
			page.DailyRate = fmt.Sprintf("≈ %d cr./j", int(rate))
		}

		// Les canaux d'une organisation viennent de deux sources : la
		// configuration et les liaisons par jeton. Lues une seule fois,
		// elles servent au compteur d'en-tête comme à l'onglet Canaux.
		bound, err := h.Bindings.ListByOrg(r.Context(), tx, orgID)
		if err != nil {
			return err
		}

		channelCount := len(bound)
		for _, ch := range h.Cfg.Channels {
			if ch.OrgID == orgID {
				channelCount++
			}
		}
		page.Stats = []view.OrgStat{
			{Label: "Solde", Value: view.FormatCredits(balance)},
			{Label: "Conso. " + strings.ToLower(view.FormatMonth(r.Context(), now)), Value: view.FormatCredits(monthUsage)},
			{Label: "Membres", Value: view.FormatInt(int64(len(members)))},
			{Label: "Canaux", Value: view.FormatInt(int64(channelCount))},
		}

		// Mouvements avec solde après coup : la liste est antichronologique,
		// le solde « après » se déroule depuis le solde courant.
		entries, err := h.Wallet.List(r.Context(), tx, orgID, 50)
		if err != nil {
			return err
		}
		running := balance
		for _, entry := range entries {
			badge := ""
			if entry.Kind == persistence.WalletKindPurchase {
				badge = "STRIPE"
			}
			page.Ledger = append(page.Ledger, view.WalletRow{
				At:     view.FormatDayTime(entry.CreatedAt),
				Label:  entry.Label,
				Badge:  badge,
				Amount: entry.Amount,
				After:  running,
			})
			running -= entry.Amount
		}

		if page.Weeks, err = h.weeklyUsage(r.Context(), tx, orgID, now); err != nil {
			return err
		}

		for _, member := range members {
			chip := view.Chip{Label: "Pré-créé", Tone: "neutral", Dot: true}
			if member.Linked() {
				chip = view.Chip{Label: "Lié", Tone: "ok", Dot: true}
			}
			page.Members = append(page.Members, view.MemberRow{
				ID:    member.ID,
				Name:  member.DisplayName,
				Role:  memberRoleLabel(member.Role),
				Chip:  chip,
				Email: member.Email,
			})
		}

		// Personnalisation : les spécialistes déclarés dans l'instance,
		// cochés selon ce que l'organisation conserve.
		settings, _, err := h.OrgSettings.Get(r.Context(), tx, orgID)
		if err != nil {
			return err
		}
		page.PromptExtra = settings.PromptExtra
		page.MaxToolCalls = settings.MaxToolCalls

		for name, agentCfg := range h.Cfg.Agents {
			if agentCfg.Type == config.AgentTypeOrchestrator {
				continue
			}
			page.Specialists = append(page.Specialists, view.SpecialistToggle{
				Name:        name,
				Description: agentCfg.Description,
				Enabled:     !settings.AgentDisabled(name),
			})
		}
		sort.Slice(page.Specialists, func(i, j int) bool { return page.Specialists[i].Name < page.Specialists[j].Name })

		for _, binding := range bound {
			page.Channels = append(page.Channels, view.OrgChannelRow{
				PlatformType: h.ProviderTypeOf(binding.Provider),
				Name:         binding.DisplayName,
				Kind:         core.ChannelKindLabelFromScope(binding.Kind),
				Chip:         view.Chip{Label: "Actif", Tone: "ok"},
				Provider:     binding.Provider,
				ChannelID:    binding.ChannelID,
			})
		}

		for _, ch := range h.Cfg.Channels {
			if ch.OrgID != orgID {
				continue
			}
			page.Channels = append(page.Channels, view.OrgChannelRow{
				PlatformType: h.ProviderTypeOf(ch.Provider),
				Name:         h.ChannelDisplayName(ch),
				Kind:         core.ChannelKindLabel(ch.Kind),
				Chip:         view.Chip{Label: "Actif", Tone: "ok"},
			})
		}

		return nil
	})
	if !ok {
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}

	if key := r.URL.Query().Get("reveal"); key != "" {
		if value, ok := h.Reveals.Pop(key, now); ok {
			page.FlashToken = &view.TokenPanelData{
				Eyebrow:   "Jeton de groupe",
				Display:   value.Display,
				Clipboard: value.Clear,
				Help:      "Envoyez ce code dans la conversation de groupe à rattacher : Automata liera le canal à " + page.Name + ". Après fermeture de cette page, le code ne pourra plus être affiché.",
				Status:    "pending",
			}
		}
	}
	if r.URL.Query().Get("granted") == "1" {
		page.Flash = "Crédits ajoutés au portefeuille."
	}
	if r.URL.Query().Get("saved") == "1" && tab == "plugins" {
		page.Flash = "Activation des plugins enregistrée."
	}
	// Une suppression refusée revient sur la fiche avec son motif : le
	// dire est le seul moyen d'expliquer pourquoi l'organisation est
	// toujours là.
	page.Error = r.URL.Query().Get("error")

	h.Render(w, r, http.StatusOK, view.AdminOrg(page))
}

// weeklyUsage construit les 5 dernières semaines de consommation (barres
// d'ADM-03), normalisées sur 96px de haut.

// weeklyUsage construit les 5 dernières semaines de consommation (barres
// d'ADM-03), normalisées sur 96px de haut.
func (h *Handlers) weeklyUsage(ctx context.Context, tx *sql.Tx, orgID string, now time.Time) ([]view.WeekBar, error) {
	const weeks = 5

	rate := h.CreditRate(ctx, tx)
	start := now.AddDate(0, 0, -7*weeks)

	aggregates, err := h.Usage.AggregateUsage(ctx, tx, start, now, []string{"day"}, persistence.UsageFilter{OrgID: orgID})
	if err != nil {
		return nil, err
	}

	totals := make([]int64, weeks)
	for _, agg := range aggregates {
		day, err := time.Parse("2006-01-02", agg.Keys[0])
		if err != nil {
			continue
		}
		index := weeks - 1 - int(now.Sub(day).Hours()/(24*7))
		if index < 0 || index >= weeks {
			continue
		}
		totals[index] += h.UsageCredits(agg.CostAmount, rate)
	}

	var max int64 = 1
	for _, total := range totals {
		if total > max {
			max = total
		}
	}

	bars := make([]view.WeekBar, weeks)
	for i, total := range totals {
		_, week := now.AddDate(0, 0, -7*(weeks-1-i)).ISOWeek()
		tone := ""
		if total == 0 {
			tone = "empty"
		}
		if i == weeks-1 {
			tone = "current"
		}
		pct := int(total * 90 / max)
		if pct < 4 {
			pct = 4
		}
		bars[i] = view.WeekBar{Label: fmt.Sprintf("S%d", week), Pct: pct, Tone: tone}
	}

	return bars, nil
}

func memberRoleLabel(role string) string {
	switch role {
	case persistence.MemberRoleOwner:
		return "Responsable"
	case persistence.MemberRoleReadOnly:
		return "Lecture seule"
	default:
		return "Membre"
	}
}

// redirectOrgError renvoie sur la fiche avec un message d'échec.
func (h *Handlers) redirectOrgError(w http.ResponseWriter, r *http.Request, orgID, message string) {
	http.Redirect(w, r, "/admin/orgs/"+orgID+"?tab=customization&error="+url.QueryEscape(message), http.StatusFound)
}
