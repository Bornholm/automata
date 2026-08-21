package web

import (
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
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
func (s *Server) orgSubtitle(orgID string, bound []persistence.ChannelBinding) string {
	var count int
	firstType := ""
	for _, ch := range s.cfg.Channels {
		if ch.OrgID != orgID {
			continue
		}
		count++
		if firstType == "" {
			firstType = providerTypeOf(s.cfg, ch.Provider)
		}
	}

	for _, binding := range bound {
		if binding.OrgID != orgID {
			continue
		}
		count++
		if firstType == "" {
			firstType = providerTypeOf(s.cfg, binding.Provider)
		}
	}

	if count == 0 {
		return "Aucun canal lié"
	}

	label := "canaux"
	if count == 1 {
		label = "canal"
	}
	return fmt.Sprintf("%s · %d %s", platformDisplayName(firstType, firstType), count, label)
}

// handleOrgs — ADM-02.
func (s *Server) handleOrgs(w http.ResponseWriter, r *http.Request) {
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	now := s.now()
	monthFrom, monthTo := monthBounds(now)

	page := view.OrgsPage{
		Search:    search,
		Platforms: s.sidebarPlatforms(),
		CSRFToken: s.csrfToken(w, r),
	}

	ok := s.withTx(w, r, func(tx *sql.Tx) error {
		orgs, err := s.orgs.List(r.Context(), tx, search)
		if err != nil {
			return err
		}
		balances, err := s.wallet.Balances(r.Context(), tx)
		if err != nil {
			return err
		}
		memberCounts, err := s.members.CountByOrg(r.Context(), tx)
		if err != nil {
			return err
		}
		bound, err := s.bindings.ListAll(r.Context(), tx)
		if err != nil {
			return err
		}
		monthUsage, err := s.orgUsageCredits(r.Context(), tx, monthFrom, monthTo)
		if err != nil {
			return err
		}

		for _, org := range orgs {
			lastCredit, err := s.wallet.LastCredit(r.Context(), tx, org.ID)
			if err != nil {
				return err
			}

			state := computeWalletState(org, balances[org.ID], lastCredit, monthUsage[org.ID])

			usageLabel := "—"
			if monthUsage[org.ID] > 0 {
				usageLabel = view.FormatCredits(monthUsage[org.ID])
			}

			page.Rows = append(page.Rows, view.OrgRow{
				ID:           org.ID,
				Name:         org.DisplayName,
				Subtitle:     s.orgSubtitle(org.ID, bound),
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

	s.render(w, r, http.StatusOK, view.AdminOrgs(page))
}

func (s *Server) handleOrgNewForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, view.AdminOrgNew(view.OrgNewPage{
		Platforms: s.sidebarPlatforms(),
		CSRFToken: s.csrfToken(w, r),
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

func (s *Server) handleOrgCreate(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("display_name"))
	if name == "" {
		s.render(w, r, http.StatusBadRequest, view.AdminOrgNew(view.OrgNewPage{
			Platforms: s.sidebarPlatforms(),
			CSRFToken: s.csrfToken(w, r),
			Error:     "Le nom de l'organisation est requis.",
		}))
		return
	}

	welcome, _ := strconv.ParseInt(r.PostFormValue("welcome_credits"), 10, 64)
	ownerName := strings.TrimSpace(r.PostFormValue("owner_name"))
	now := s.now()

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
	if !s.withTx(w, r, func(tx *sql.Tx) error {
		existing, err := s.orgs.List(r.Context(), tx, "")
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
		s.render(w, r, http.StatusConflict, view.AdminOrgNew(view.OrgNewPage{
			Platforms: s.sidebarPlatforms(),
			CSRFToken: s.csrfToken(w, r),
			Error:     "Une organisation nommée « " + name + " » existe déjà. Choisissez un autre nom, ou ouvrez l'existante.",
		}))
		return
	}

	ok := s.withTx(w, r, func(tx *sql.Tx) error {
		// Suffixe numérique en cas de collision d'identifiant.
		candidate := orgID
		for i := 2; ; i++ {
			_, exists, err := s.orgs.FindByID(r.Context(), tx, candidate)
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
		if err := s.orgs.Insert(r.Context(), tx, org, false); err != nil {
			return err
		}

		if welcome > 0 {
			if err := s.wallet.Insert(r.Context(), tx, persistence.WalletEntry{
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
			return s.members.Insert(r.Context(), tx, persistence.Member{
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

	s.logger.InfoContext(r.Context(), "web: organisation créée", "org_id", orgID)
	http.Redirect(w, r, "/admin/orgs/"+orgID, http.StatusFound)
}

// handleOrg — ADM-03.
func (s *Server) handleOrg(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("id")
	tab := r.URL.Query().Get("tab")
	if tab != "members" && tab != "channels" && tab != "customization" && tab != "plugins" {
		tab = "credits"
	}
	now := s.now()
	monthFrom, monthTo := monthBounds(now)

	page := view.OrgPage{
		Platforms: s.sidebarPlatforms(),
		CSRFToken: s.csrfToken(w, r),
		ID:        orgID,
		Tab:       tab,
	}

	found := false
	ok := s.withTx(w, r, func(tx *sql.Tx) error {
		org, exists, err := s.orgs.FindByID(r.Context(), tx, orgID)
		if err != nil || !exists {
			return err
		}
		found = true

		balance, err := s.wallet.Balance(r.Context(), tx, orgID)
		if err != nil {
			return err
		}
		lastCredit, err := s.wallet.LastCredit(r.Context(), tx, orgID)
		if err != nil {
			return err
		}
		monthUsage, err := s.singleOrgUsageCredits(r.Context(), tx, orgID, monthFrom, monthTo)
		if err != nil {
			return err
		}
		members, err := s.members.ListByOrg(r.Context(), tx, orgID)
		if err != nil {
			return err
		}
		rate, err := s.dailyRate(r.Context(), tx, orgID, now)
		if err != nil {
			return err
		}

		pluginRows, err := s.pluginActivationRows(r, tx, orgID)
		if err != nil {
			return err
		}
		page.PluginRows = pluginRows

		state := computeWalletState(org, balance, lastCredit, monthUsage)

		page.Name = org.DisplayName
		page.Chip = state.Chip
		page.Offered = org.Offered
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

		channelCount := 0
		for _, ch := range s.cfg.Channels {
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
		entries, err := s.wallet.List(r.Context(), tx, orgID, 50)
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

		if page.Weeks, err = s.weeklyUsage(r.Context(), tx, orgID, now); err != nil {
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
		settings, _, err := s.orgSettings.Get(r.Context(), tx, orgID)
		if err != nil {
			return err
		}
		page.PromptExtra = settings.PromptExtra
		page.MaxToolCalls = settings.MaxToolCalls

		for name, agentCfg := range s.cfg.Agents {
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

		for _, ch := range s.cfg.Channels {
			if ch.OrgID != orgID {
				continue
			}
			page.Channels = append(page.Channels, view.OrgChannelRow{
				PlatformType: providerTypeOf(s.cfg, ch.Provider),
				Name:         s.channelDisplayName(ch),
				Kind:         channelKindLabel(ch.Kind),
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
		if value, ok := s.reveals.pop(key, now); ok {
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

	s.render(w, r, http.StatusOK, view.AdminOrg(page))
}

// weeklyUsage construit les 5 dernières semaines de consommation (barres
// d'ADM-03), normalisées sur 96px de haut.
func (s *Server) weeklyUsage(ctx context.Context, tx *sql.Tx, orgID string, now time.Time) ([]view.WeekBar, error) {
	const weeks = 5

	rate := s.creditRate(ctx, tx)
	start := now.AddDate(0, 0, -7*weeks)

	aggregates, err := s.usage.AggregateUsage(ctx, tx, start, now, []string{"day"}, persistence.UsageFilter{OrgID: orgID})
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
		totals[index] += s.usageCredits(agg.CostAmount, rate)
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

func (s *Server) handleOrgGrant(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("id")
	amount, _ := strconv.ParseInt(r.PostFormValue("amount"), 10, 64)
	label := strings.TrimSpace(r.PostFormValue("label"))
	if amount <= 0 || label == "" {
		http.Redirect(w, r, "/admin/orgs/"+orgID, http.StatusFound)
		return
	}

	ok := s.withTx(w, r, func(tx *sql.Tx) error {
		return s.wallet.Insert(r.Context(), tx, persistence.WalletEntry{
			OrgID:     orgID,
			Kind:      persistence.WalletKindGrant,
			Label:     label,
			Amount:    amount,
			CreatedAt: s.now(),
		})
	})
	if !ok {
		return
	}

	s.logger.InfoContext(r.Context(), "web: crédits offerts", "org_id", orgID, "amount", amount)
	http.Redirect(w, r, "/admin/orgs/"+orgID+"?granted=1", http.StatusFound)
}

func (s *Server) handleOrgOffered(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("id")
	offered := r.PostFormValue("offered") == "true"
	allowance, _ := strconv.ParseInt(r.PostFormValue("allowance"), 10, 64)

	ok := s.withTx(w, r, func(tx *sql.Tx) error {
		org, exists, err := s.orgs.FindByID(r.Context(), tx, orgID)
		if err != nil || !exists {
			return err
		}
		org.Offered = offered
		if allowance >= 0 && offered {
			org.MonthlyAllowance = allowance
		}
		org.UpdatedAt = s.now()
		if err := s.orgs.Update(r.Context(), tx, org); err != nil {
			return err
		}

		// Apport immédiat : sans lui, une organisation qu'on vient d'offrir
		// devrait attendre le 1er du mois suivant pour recevoir ses crédits
		// — et son service resterait en pause jusque-là.
		if !org.Offered || org.MonthlyAllowance <= 0 {
			return nil
		}

		balance, err := s.wallet.Balance(r.Context(), tx, orgID)
		if err != nil {
			return err
		}
		if balance >= org.MonthlyAllowance {
			return nil
		}

		return s.wallet.Insert(r.Context(), tx, persistence.WalletEntry{
			OrgID:     orgID,
			Kind:      persistence.WalletKindAllowance,
			Label:     "Allocation mensuelle offerte",
			Amount:    org.MonthlyAllowance - balance,
			CreatedAt: s.now(),
		})
	})
	if !ok {
		return
	}

	http.Redirect(w, r, "/admin/orgs/"+orgID, http.StatusFound)
}

func (s *Server) handleOrgGroupToken(w http.ResponseWriter, r *http.Request) {
	s.createGroupToken(w, r, r.PathValue("id"), "/admin/orgs/"+r.PathValue("id"))
}

// createGroupToken génère un jeton de groupe pour orgID et redirige vers
// redirectBase avec la clé de révélation.
func (s *Server) createGroupToken(w http.ResponseWriter, r *http.Request, orgID, redirectBase string) {
	now := s.now()

	clear, hash, display, err := weblink.NewLinkToken()
	if err != nil {
		s.logger.ErrorContext(r.Context(), "web: génération d'un jeton de groupe", "error", err)
		http.Error(w, "erreur interne", http.StatusInternalServerError)
		return
	}

	tokenID, err := weblink.RandomCrockford(10)
	if err != nil {
		http.Error(w, "erreur interne", http.StatusInternalServerError)
		return
	}

	ok := s.withTx(w, r, func(tx *sql.Tx) error {
		return s.linkTokens.Insert(r.Context(), tx, persistence.LinkToken{
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

	key, err := s.reveals.put(revealValue{Clear: clear, Display: display}, now)
	if err != nil {
		http.Error(w, "erreur interne", http.StatusInternalServerError)
		return
	}

	s.logger.InfoContext(r.Context(), "web: jeton de groupe généré", "org_id", orgID, "token_id", tokenID)

	separator := "?"
	if strings.Contains(redirectBase, "?") {
		separator = "&"
	}
	http.Redirect(w, r, redirectBase+separator+"reveal="+key, http.StatusFound)
}

// handleOrgWalletCSV exporte les mouvements du portefeuille.
func (s *Server) handleOrgWalletCSV(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("id")

	var entries []persistence.WalletEntry
	ok := s.withTx(w, r, func(tx *sql.Tx) error {
		var err error
		entries, err = s.wallet.List(r.Context(), tx, orgID, 0)
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

// handleOrgCustomization enregistre la personnalisation d'une
// organisation : consigne ajoutée, spécialistes conservés, plafond
// d'outils.
func (s *Server) handleOrgCustomization(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("id")

	// Les cases cochées disent ce qui reste : ce qui est retiré est le
	// complément, calculé sur les spécialistes réellement déclarés.
	kept := map[string]struct{}{}
	for _, name := range r.PostForm["agent"] {
		kept[name] = struct{}{}
	}

	var disabled []string
	for name, agentCfg := range s.cfg.Agents {
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

	ok := s.withTx(w, r, func(tx *sql.Tx) error {
		return s.orgSettings.Upsert(r.Context(), tx, persistence.OrgSettings{
			OrgID:          orgID,
			PromptExtra:    strings.TrimSpace(r.PostFormValue("prompt_extra")),
			DisabledAgents: disabled,
			MaxToolCalls:   maxToolCalls,
			UpdatedAt:      s.now(),
		})
	})
	if !ok {
		return
	}

	s.logger.InfoContext(r.Context(), "web: personnalisation d'organisation enregistrée",
		"org_id", orgID, "disabled_agents", len(disabled), "max_tool_calls", maxToolCalls)

	http.Redirect(w, r, "/admin/orgs/"+orgID+"?tab=customization&saved=1", http.StatusFound)
}
