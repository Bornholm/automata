package admin

import (
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/web/core"
	"github.com/bornholm/automata/internal/web/view"
	"github.com/bornholm/automata/internal/weblink"
)

// orgSubtitle décrit les canaux d'une organisation
// (« WhatsApp · 2 canaux »).
//
// Les deux sources comptent : les canaux déclarés en configuration et
// ceux rattachés en ligne par jeton de liaison. N'en compter qu'une
// affichait « Aucun canal lié » à des organisations dont toutes les
// conversations étaient rattachées — la liste contredisait alors l'écran
// des canaux.
func (h *Handlers) orgSubtitle(orgID string, bound []persistence.ChannelBinding) string {
	var count int
	firstType := ""
	for _, ch := range h.Cfg.Channels {
		if ch.OrgID != orgID {
			continue
		}
		count++
		if firstType == "" {
			firstType = h.ProviderTypeOf(ch.Provider)
		}
	}

	for _, binding := range bound {
		if binding.OrgID != orgID {
			continue
		}
		count++
		if firstType == "" {
			firstType = h.ProviderTypeOf(binding.Provider)
		}
	}

	if count == 0 {
		return "Aucun canal lié"
	}

	label := "canaux"
	if count == 1 {
		label = "canal"
	}
	return fmt.Sprintf("%s · %d %s", core.PlatformDisplayName(firstType, firstType), count, label)
}

// HandleOrgs — ADM-02.
func (h *Handlers) HandleOrgs(w http.ResponseWriter, r *http.Request) {
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	now := h.Now()
	monthFrom, monthTo := core.MonthBounds(now)

	page := view.OrgsPage{
		Search:    search,
		Platforms: h.SidebarPlatforms(),
		CSRFToken: h.CSRFToken(w, r),
	}

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		orgs, err := h.Orgs.List(r.Context(), tx, search)
		if err != nil {
			return err
		}
		balances, err := h.Wallet.Balances(r.Context(), tx)
		if err != nil {
			return err
		}
		memberCounts, err := h.Members.CountByOrg(r.Context(), tx)
		if err != nil {
			return err
		}
		bound, err := h.Bindings.ListAll(r.Context(), tx)
		if err != nil {
			return err
		}
		monthUsage, err := h.OrgUsageCredits(r.Context(), tx, monthFrom, monthTo)
		if err != nil {
			return err
		}

		for _, org := range orgs {
			lastCredit, err := h.Wallet.LastCredit(r.Context(), tx, org.ID)
			if err != nil {
				return err
			}

			state := core.ComputeWalletState(org, balances[org.ID], lastCredit, monthUsage[org.ID])

			usageLabel := "—"
			if monthUsage[org.ID] > 0 {
				usageLabel = view.FormatCredits(monthUsage[org.ID])
			}

			page.Rows = append(page.Rows, view.OrgRow{
				ID:           org.ID,
				Name:         org.DisplayName,
				Subtitle:     h.orgSubtitle(org.ID, bound),
				Chip:         state.Chip,
				BalanceLabel: state.BalanceLabel,
				GaugePct:     state.GaugePct,
				GaugeTone:    state.GaugeTone,
				Members:      memberCounts[org.ID],
				MonthUsage:   usageLabel,
				CreatedAt:    view.FormatShortDate(org.CreatedAt),
				RowTone:      state.RowTone,
			})
			if state.RowTone != "" {
				page.Flagged++
			}
		}
		page.Total = len(page.Rows)

		return nil
	})
	if !ok {
		return
	}

	h.Render(w, r, http.StatusOK, view.AdminOrgs(page))
}

func (h *Handlers) HandleOrgNewForm(w http.ResponseWriter, r *http.Request) {
	h.Render(w, r, http.StatusOK, view.AdminOrgNew(view.OrgNewPage{
		Platforms: h.SidebarPlatforms(),
		CSRFToken: h.CSRFToken(w, r),
	}))
}

// slugify dérive un identifiant stable d'un nom affiché.
func slugify(name string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case r == 'é' || r == 'è' || r == 'ê' || r == 'ë':
			b.WriteByte('e')
			lastDash = false
		case r == 'à' || r == 'â':
			b.WriteByte('a')
			lastDash = false
		case r == 'î' || r == 'ï':
			b.WriteByte('i')
			lastDash = false
		case r == 'ô' || r == 'ö':
			b.WriteByte('o')
			lastDash = false
		case r == 'ù' || r == 'û' || r == 'ü':
			b.WriteByte('u')
			lastDash = false
		case r == 'ç':
			b.WriteByte('c')
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}

	return strings.Trim(b.String(), "-")
}

func (h *Handlers) HandleOrgCreate(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("display_name"))
	if name == "" {
		h.Render(w, r, http.StatusBadRequest, view.AdminOrgNew(view.OrgNewPage{
			Platforms: h.SidebarPlatforms(),
			CSRFToken: h.CSRFToken(w, r),
			Error:     "Le nom de l'organisation est requis.",
		}))
		return
	}

	welcome, _ := strconv.ParseInt(r.PostFormValue("welcome_credits"), 10, 64)
	ownerName := strings.TrimSpace(r.PostFormValue("owner_name"))
	now := h.Now()

	orgID := slugify(name)
	if orgID == "" {
		orgID = "org"
	}

	// Deux organisations de même nom sont indiscernables partout où on
	// les liste : dans le sélecteur des jetons de liaison comme dans la
	// liste d'administration. Le doublon vient presque toujours d'une
	// création refaite, pas d'une intention — mieux vaut le dire que le
	// laisser passer et devoir démêler ensuite quel canal est allé où.
	var duplicate bool
	if !h.WithTx(w, r, func(tx *sql.Tx) error {
		existing, err := h.Orgs.List(r.Context(), tx, "")
		if err != nil {
			return err
		}
		for _, org := range existing {
			if strings.EqualFold(strings.TrimSpace(org.DisplayName), name) {
				duplicate = true
				break
			}
		}

		return nil
	}) {
		return
	}
	if duplicate {
		h.Render(w, r, http.StatusConflict, view.AdminOrgNew(view.OrgNewPage{
			Platforms: h.SidebarPlatforms(),
			CSRFToken: h.CSRFToken(w, r),
			Error:     "Une organisation nommée « " + name + " » existe déjà. Choisissez un autre nom, ou ouvrez l'existante.",
		}))
		return
	}

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		// Suffixe numérique en cas de collision d'identifiant.
		candidate := orgID
		for i := 2; ; i++ {
			_, exists, err := h.Orgs.FindByID(r.Context(), tx, candidate)
			if err != nil {
				return err
			}
			if !exists {
				break
			}
			candidate = fmt.Sprintf("%s-%d", orgID, i)
		}
		orgID = candidate

		org := persistence.Organization{
			ID:          orgID,
			DisplayName: name,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if err := h.Orgs.Insert(r.Context(), tx, org, false); err != nil {
			return err
		}

		if welcome > 0 {
			if err := h.Wallet.Insert(r.Context(), tx, persistence.WalletEntry{
				OrgID:     orgID,
				Kind:      persistence.WalletKindWelcome,
				Label:     "Crédits de bienvenue",
				Amount:    welcome,
				CreatedAt: now,
			}); err != nil {
				return err
			}
		}

		if ownerName != "" {
			memberID, err := weblink.RandomCrockford(10)
			if err != nil {
				return err
			}
			return h.Members.Insert(r.Context(), tx, persistence.Member{
				ID:          strings.ToLower(memberID),
				OrgID:       orgID,
				DisplayName: ownerName,
				Role:        persistence.MemberRoleOwner,
				CreatedAt:   now,
				UpdatedAt:   now,
			}, false)
		}

		return nil
	})
	if !ok {
		return
	}

	h.Logger.InfoContext(r.Context(), "web: organisation créée", "org_id", orgID)
	http.Redirect(w, r, "/admin/orgs/"+orgID, http.StatusFound)
}

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
		page.BalanceHint = view.HumanUsageDuration(balance, rate)
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
			{Label: "Conso. " + strings.ToLower(view.FormatMonth(now)), Value: view.FormatCredits(monthUsage)},
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

func (h *Handlers) HandleOrgGrant(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("id")
	amount, _ := strconv.ParseInt(r.PostFormValue("amount"), 10, 64)
	label := strings.TrimSpace(r.PostFormValue("label"))
	if amount <= 0 || label == "" {
		http.Redirect(w, r, "/admin/orgs/"+orgID, http.StatusFound)
		return
	}

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		return h.Wallet.Insert(r.Context(), tx, persistence.WalletEntry{
			OrgID:     orgID,
			Kind:      persistence.WalletKindGrant,
			Label:     label,
			Amount:    amount,
			CreatedAt: h.Now(),
		})
	})
	if !ok {
		return
	}

	h.Logger.InfoContext(r.Context(), "web: crédits offerts", "org_id", orgID, "amount", amount)
	http.Redirect(w, r, "/admin/orgs/"+orgID+"?granted=1", http.StatusFound)
}

func (h *Handlers) HandleOrgOffered(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("id")
	offered := r.PostFormValue("offered") == "true"
	allowance, _ := strconv.ParseInt(r.PostFormValue("allowance"), 10, 64)

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		org, exists, err := h.Orgs.FindByID(r.Context(), tx, orgID)
		if err != nil || !exists {
			return err
		}
		org.Offered = offered
		if allowance >= 0 && offered {
			org.MonthlyAllowance = allowance
		}
		org.UpdatedAt = h.Now()
		if err := h.Orgs.Update(r.Context(), tx, org); err != nil {
			return err
		}

		// Apport immédiat : sans lui, une organisation qu'on vient d'offrir
		// devrait attendre le 1er du mois suivant pour recevoir ses crédits
		// — et son service resterait en pause jusque-là.
		if !org.Offered || org.MonthlyAllowance <= 0 {
			return nil
		}

		balance, err := h.Wallet.Balance(r.Context(), tx, orgID)
		if err != nil {
			return err
		}
		if balance >= org.MonthlyAllowance {
			return nil
		}

		return h.Wallet.Insert(r.Context(), tx, persistence.WalletEntry{
			OrgID:     orgID,
			Kind:      persistence.WalletKindAllowance,
			Label:     "Allocation mensuelle offerte",
			Amount:    org.MonthlyAllowance - balance,
			CreatedAt: h.Now(),
		})
	})
	if !ok {
		return
	}

	http.Redirect(w, r, "/admin/orgs/"+orgID, http.StatusFound)
}

// HandleOrgUnlimited bascule le mode gratuit sans limite : l'organisation
// n'est plus jamais débitée ni mise en pause, et son allocation mensuelle
// devient sans objet. Sa consommation reste mesurée : le coût réel demeure
// visible dans les écrans d'usage et de marge.
func (h *Handlers) HandleOrgUnlimited(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("id")
	unlimited := r.PostFormValue("unlimited") == "true"

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		org, exists, err := h.Orgs.FindByID(r.Context(), tx, orgID)
		if err != nil || !exists {
			return err
		}
		org.Unlimited = unlimited
		org.UpdatedAt = h.Now()

		return h.Orgs.Update(r.Context(), tx, org)
	})
	if !ok {
		return
	}

	h.Logger.InfoContext(r.Context(), "web: mode gratuit sans limite modifié",
		"org", orgID, "unlimited", unlimited)

	http.Redirect(w, r, "/admin/orgs/"+orgID, http.StatusFound)
}

func (h *Handlers) HandleOrgGroupToken(w http.ResponseWriter, r *http.Request) {
	h.createGroupToken(w, r, r.PathValue("id"), "/admin/orgs/"+r.PathValue("id"))
}

// createGroupToken génère un jeton de groupe pour orgID et redirige vers
// redirectBase avec la clé de révélation.
func (h *Handlers) createGroupToken(w http.ResponseWriter, r *http.Request, orgID, redirectBase string) {
	now := h.Now()

	clear, hash, display, err := weblink.NewLinkToken()
	if err != nil {
		h.Logger.ErrorContext(r.Context(), "web: génération d'un jeton de groupe", "error", err)
		http.Error(w, "erreur interne", http.StatusInternalServerError)
		return
	}

	tokenID, err := weblink.RandomCrockford(10)
	if err != nil {
		http.Error(w, "erreur interne", http.StatusInternalServerError)
		return
	}

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		return h.LinkTokens.Insert(r.Context(), tx, persistence.LinkToken{
			ID:        strings.ToLower(tokenID),
			Kind:      persistence.LinkTokenKindGroup,
			OrgID:     orgID,
			TokenHash: hash,
			Status:    persistence.LinkTokenStatusPending,
			ExpiresAt: now.AddDate(0, 0, 7),
			CreatedAt: now,
		})
	})
	if !ok {
		return
	}

	key, err := h.Reveals.Put(core.RevealValue{Clear: clear, Display: display}, now)
	if err != nil {
		http.Error(w, "erreur interne", http.StatusInternalServerError)
		return
	}

	h.Logger.InfoContext(r.Context(), "web: jeton de groupe généré", "org_id", orgID, "token_id", tokenID)

	separator := "?"
	if strings.Contains(redirectBase, "?") {
		separator = "&"
	}
	http.Redirect(w, r, redirectBase+separator+"reveal="+key, http.StatusFound)
}

// HandleOrgWalletCSV exporte les mouvements du portefeuille.
func (h *Handlers) HandleOrgWalletCSV(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("id")

	var entries []persistence.WalletEntry
	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		var err error
		entries, err = h.Wallet.List(r.Context(), tx, orgID, 0)
		return err
	})
	if !ok {
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="portefeuille-`+orgID+`.csv"`)

	writer := csv.NewWriter(w)
	_ = writer.Write([]string{"date", "nature", "libelle", "montant"})
	for _, entry := range entries {
		_ = writer.Write([]string{
			entry.CreatedAt.UTC().Format(time.RFC3339),
			entry.Kind,
			entry.Label,
			strconv.FormatInt(entry.Amount, 10),
		})
	}
	writer.Flush()
}

// HandleOrgCustomization enregistre la personnalisation d'une
// organisation : consigne ajoutée, spécialistes conservés, plafond
// d'outils.
func (h *Handlers) HandleOrgCustomization(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("id")

	// Les cases cochées disent ce qui reste : ce qui est retiré est le
	// complément, calculé sur les spécialistes réellement déclarés.
	kept := map[string]struct{}{}
	for _, name := range r.PostForm["agent"] {
		kept[name] = struct{}{}
	}

	var disabled []string
	for name, agentCfg := range h.Cfg.Agents {
		if agentCfg.Type == config.AgentTypeOrchestrator {
			continue
		}
		if _, ok := kept[name]; !ok {
			disabled = append(disabled, name)
		}
	}
	sort.Strings(disabled)

	maxToolCalls, _ := strconv.Atoi(r.PostFormValue("max_tool_calls"))
	if maxToolCalls < 0 {
		maxToolCalls = 0
	}

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		return h.OrgSettings.Upsert(r.Context(), tx, persistence.OrgSettings{
			OrgID:          orgID,
			PromptExtra:    strings.TrimSpace(r.PostFormValue("prompt_extra")),
			DisabledAgents: disabled,
			MaxToolCalls:   maxToolCalls,
			UpdatedAt:      h.Now(),
		})
	})
	if !ok {
		return
	}

	h.Logger.InfoContext(r.Context(), "web: personnalisation d'organisation enregistrée",
		"org_id", orgID, "disabled_agents", len(disabled), "max_tool_calls", maxToolCalls)

	http.Redirect(w, r, "/admin/orgs/"+orgID+"?tab=customization&saved=1", http.StatusFound)
}

// HandleOrgDelete supprime une organisation et tout ce qui n'existe que
// par elle.
//
// La confirmation demande de retaper le nom : la liste d'administration
// peut présenter deux organisations homonymes, et se tromper de ligne
// n'aurait aucun recours — l'effacement est complet et définitif.
func (h *Handlers) HandleOrgDelete(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("id")
	typed := strings.TrimSpace(r.PostFormValue("confirm_name"))

	if h.Privacy == nil {
		h.redirectOrgError(w, r, orgID, "La suppression n'est pas disponible sur cette instance.")
		return
	}

	var (
		mismatch bool
		missing  bool
	)

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		org, found, err := h.Orgs.FindByID(r.Context(), tx, orgID)
		if err != nil {
			return err
		}
		if !found {
			missing = true
			return nil
		}
		if !strings.EqualFold(typed, strings.TrimSpace(org.DisplayName)) {
			mismatch = true
		}

		return nil
	})
	if !ok {
		return
	}

	if missing {
		http.NotFound(w, r)
		return
	}
	if mismatch {
		h.redirectOrgError(w, r, orgID, "Le nom saisi ne correspond pas : rien n'a été supprimé.")
		return
	}

	// La suppression touche la base mémoire autant que la base
	// applicative : elle passe par le service de confidentialité, qui
	// possède les deux.
	report, err := h.Privacy.DeleteOrganization(r.Context(), orgID)
	if err != nil {
		h.Logger.ErrorContext(r.Context(), "web: échec de la suppression d'une organisation", "org_id", orgID, "error", err)
		h.redirectOrgError(w, r, orgID, "La suppression a échoué. Rien n'a été supprimé, ou seulement une partie : consultez les journaux.")
		return
	}

	// Compteurs seulement : ce qui a été effacé ne se journalise pas.
	h.Logger.InfoContext(r.Context(), "web: organisation supprimée",
		"org_id", orgID,
		"members", report.Members,
		"orphan_members", report.OrphanMembers,
		"channels", report.Channels,
		"conversations", report.Conversations,
		"messages", report.Messages,
		"reminders", report.Reminders,
		"memories", report.Memories,
	)

	http.Redirect(w, r, "/admin/orgs?deleted="+url.QueryEscape(orgID), http.StatusFound)
}

// redirectOrgError renvoie sur la fiche avec un message d'échec.
func (h *Handlers) redirectOrgError(w http.ResponseWriter, r *http.Request, orgID, message string) {
	http.Redirect(w, r, "/admin/orgs/"+orgID+"?tab=customization&error="+url.QueryEscape(message), http.StatusFound)
}
