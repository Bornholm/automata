package profile

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bornholm/automata/internal/i18n"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/web/core"
	"github.com/bornholm/automata/internal/web/view"
	"github.com/bornholm/automata/internal/weblink"
)

// resolveProfile valide l'accès à une page de profil : consomme le lien à
// la première ouverture (usage unique), pose la session courte, puis sert
// les visites suivantes sur cette session tant que le lien du chemin lui
// correspond. Retourne (membre, minutes restantes, true) ou rend l'état de
// lien approprié (PRO-90) et retourne false.
func (h *Handlers) resolveProfile(w http.ResponseWriter, r *http.Request) (persistence.Member, *http.Request, int, bool) {
	now := h.Now()
	segment := r.PathValue("link")

	linkID, secret, wellFormed := weblink.SplitProfileLink(segment)
	if !wellFormed {
		h.Render(w, r, http.StatusNotFound, view.ProfileLinkState(view.LinkStatePage{State: "expired"}))
		return persistence.Member{}, r, 0, false
	}

	// Session courte déjà ouverte pour ce lien ? (navigation entre pages,
	// soumissions de formulaires après consommation du lien.)
	if cookie, err := r.Cookie(core.ProfileCookieName); err == nil {
		if subject, expires, ok := h.Signer.ParseSession(cookie.Value, "profile", now); ok {
			memberID, sessionLink, found := strings.Cut(subject, "/")
			if found && sessionLink == linkID {
				member, exists, err := h.findMember(r, memberID)
				if err == nil && exists {
					return member, withMemberLocale(r, member), minutesLeft(now, expires), true
				}
			}
		}
	}

	var (
		link      persistence.ProfileLink
		linkFound bool
	)
	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		var err error
		link, linkFound, err = h.ProfileLinks.FindByID(r.Context(), tx, linkID)
		return err
	})
	if !ok {
		return persistence.Member{}, r, 0, false
	}

	switch {
	case !linkFound || weblink.HashToken(secret) != link.TokenHash:
		// Lien inconnu ou secret faux : même réponse qu'un lien périmé —
		// aucune information sur l'existence du lien.
		h.Render(w, r, http.StatusNotFound, view.ProfileLinkState(view.LinkStatePage{State: "expired"}))
		return persistence.Member{}, r, 0, false
	case link.Status == persistence.ProfileLinkStatusOpened:
		h.Render(w, r, http.StatusGone, view.ProfileLinkState(view.LinkStatePage{State: "used"}))
		return persistence.Member{}, r, 0, false
	case now.After(link.ExpiresAt):
		h.Render(w, r, http.StatusGone, view.ProfileLinkState(view.LinkStatePage{State: "expired"}))
		return persistence.Member{}, r, 0, false
	}

	// Le lien est intact. Le consommer ici le griller: les messageries
	// préchargent les adresses qu'on y colle pour en afficher un aperçu,
	// et ce robot arriverait avant la personne. L'ouverture demande donc
	// un geste — un POST, que nul aperçu n'émet.
	if r.Method != http.MethodPost || r.URL.Path != "/p/"+segment+"/open" {
		member, exists, err := h.findMember(r, link.MemberID)
		if err != nil || !exists {
			h.Logger.ErrorContext(r.Context(), "web: lien de profil vers un membre introuvable", "link_id", linkID)
			h.Render(w, r, http.StatusInternalServerError, view.ProfileLinkState(view.LinkStatePage{State: "error", Ref: linkID}))
			return persistence.Member{}, r, 0, false
		}

		r = withMemberLocale(r, member)
		h.Render(w, r, http.StatusOK, view.ProfileLinkOpen(view.LinkOpenPage{
			LinkID:    segment,
			CSRFToken: h.CSRFToken(w, r),
			Name:      firstName(member.DisplayName),
		}))
		return persistence.Member{}, r, 0, false
	}

	// Ouverture demandée : consommation atomique.
	consumed := false
	ok = h.WithTx(w, r, func(tx *sql.Tx) error {
		var err error
		consumed, err = h.ProfileLinks.MarkOpened(r.Context(), tx, linkID, now)
		return err
	})
	if !ok {
		return persistence.Member{}, r, 0, false
	}
	if !consumed {
		h.Render(w, r, http.StatusGone, view.ProfileLinkState(view.LinkStatePage{State: "used"}))
		return persistence.Member{}, r, 0, false
	}

	member, exists, err := h.findMember(r, link.MemberID)
	if err != nil || !exists {
		h.Logger.ErrorContext(r.Context(), "web: lien de profil vers un membre introuvable", "link_id", linkID)
		h.Render(w, r, http.StatusInternalServerError, view.ProfileLinkState(view.LinkStatePage{State: "error", Ref: linkID}))
		return persistence.Member{}, r, 0, false
	}

	expires := now.Add(core.ProfileSessionTTL)
	subject := member.ID + "/" + linkID
	core.SetSessionCookie(w, core.ProfileCookieName, h.Signer.Sign(core.SessionPayload("profile", subject, expires)), expires)

	h.Logger.InfoContext(r.Context(), "web: lien de profil ouvert", "link_id", linkID, "member_id", member.ID)

	return member, withMemberLocale(r, member), minutesLeft(now, expires), true
}

// withMemberLocale rend une requête dont le contexte porte la langue du
// membre. Le serveur y a déjà posé celle de l'instance : ici, on sait à qui
// l'on parle.
func withMemberLocale(r *http.Request, member persistence.Member) *http.Request {
	return r.WithContext(i18n.WithLocale(r.Context(), member.Locale))
}

func minutesLeft(now, expires time.Time) int {
	minutes := int(expires.Sub(now).Minutes())
	if minutes < 1 {
		minutes = 1
	}
	return minutes
}

// findMember lit un membre hors transaction longue.
func (h *Handlers) findMember(r *http.Request, memberID string) (persistence.Member, bool, error) {
	var (
		member persistence.Member
		found  bool
	)
	err := h.DB.WithTx(r.Context(), func(tx *sql.Tx) error {
		var err error
		member, found, err = h.Members.FindByID(r.Context(), tx, memberID)
		return err
	})
	return member, found, err
}

// profileHeader construit l'en-tête d'identification des pages de profil.
func (h *Handlers) profileHeader(r *http.Request, member persistence.Member, minutes int) view.ProfileHeader {
	orgName := member.OrgID
	_ = h.DB.WithTx(r.Context(), func(tx *sql.Tx) error {
		orgName = h.OrgDisplayName(r.Context(), tx, member.OrgID)
		return nil
	})

	return view.ProfileHeader{
		Name:         member.DisplayName,
		Organization: orgName,
		MinutesLeft:  minutes,
	}
}

// HandleProfileLanguage change la langue du membre.
//
// Le changement prend effet au rechargement qui suit, y compris pour les
// messages de la conversation : la langue est lue sur le membre à chaque
// tour, elle n'est mise en cache nulle part.
func (h *Handlers) HandleProfileLanguage(w http.ResponseWriter, r *http.Request) {
	member, r, _, ok := h.resolveProfile(w, r)
	if !ok {
		return
	}
	if !core.CheckCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}

	linkPath := "/p/" + r.PathValue("link")

	// Une langue que l'instance ne sert pas est ignorée : le formulaire n'en
	// propose que trois, et une valeur forgée ne vaut pas une page d'erreur.
	locale, valid := i18n.Parse(r.PostFormValue("locale"))
	if !valid || locale == member.Locale {
		http.Redirect(w, r, linkPath, http.StatusFound)
		return
	}

	now := h.Now()
	ok = h.WithTx(w, r, func(tx *sql.Tx) error {
		fresh, found, err := h.Members.FindByID(r.Context(), tx, member.ID)
		if err != nil || !found {
			return err
		}
		fresh.Locale = locale
		fresh.UpdatedAt = now
		return h.Members.Update(r.Context(), tx, fresh)
	})
	if !ok {
		return
	}

	h.Logger.InfoContext(r.Context(), "web: langue du membre changée", "member_id", member.ID, "locale", locale)

	http.Redirect(w, r, linkPath+"?langue=1", http.StatusFound)
}

// HandleProfile — PRO-01.
func (h *Handlers) HandleProfile(w http.ResponseWriter, r *http.Request) {
	member, r, minutes, ok := h.resolveProfile(w, r)
	if !ok {
		return
	}
	now := h.Now()

	page := view.ProfilePage{
		LinkID:         r.PathValue("link"),
		Header:         h.profileHeader(r, member, minutes),
		CSRFToken:      h.CSRFToken(w, r),
		Email:          member.Email,
		MailConfigured: h.Mail != nil,
		Locale:         string(member.Locale),
		LanguageSaved:  r.URL.Query().Get("langue") == "1",
	}

	switch {
	case !member.EmailVerifiedAt.IsZero():
		page.EmailState = "verified"
	default:
		if email, pending := h.Codes.Pending(member.ID, now); pending {
			page.EmailState = "pending"
			page.Email = email
		} else {
			page.EmailState = "absent"
		}
	}
	if r.URL.Query().Get("bad_code") == "1" && page.EmailState == "pending" {
		page.Error = i18n.TC(r.Context(), "profile.email.error_code")
	}

	plugins, ok := h.profilePluginUIs(w, r, member)
	if !ok {
		return
	}
	page.PluginUIs = plugins

	var channels []view.OrgChannelRow
	if ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		channels = h.MemberChannels(r.Context(), tx, member.ID, member.OrgID)
		return nil
	}); !ok {
		return
	}

	for _, ch := range channels {
		detail := i18n.TC(r.Context(), "profile.channel.private")
		if ch.Kind == "Groupe" {
			detail = i18n.TC(r.Context(), "profile.channel.group")
		}
		page.Channels = append(page.Channels, view.ProfileChannel{
			PlatformType: ch.PlatformType,
			Name:         ch.Name,
			Detail:       detail + " · " + core.PlatformDisplayName(ch.PlatformType, ch.PlatformType),
		})
	}

	h.Render(w, r, http.StatusOK, view.ProfileHome(page))
}

// profilePluginUIs liste les plugins actifs pour l'organisation du membre
// et dotés d'une interface. Chaque page du profil en a besoin : ils
// forment des onglets, donc ils doivent apparaître partout, pas seulement
// là où ils sont affichés.
func (h *Handlers) profilePluginUIs(w http.ResponseWriter, r *http.Request, member persistence.Member) ([]view.ProfilePluginUI, bool) {
	endpoint, ok := h.PluginMgr.(core.PluginUIEndpoint)
	if !ok || h.PluginMgr == nil {
		return nil, true
	}

	var enabled []string
	if ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		var err error
		enabled, err = h.PluginActivations.EnabledPlugins(r.Context(), tx, member.OrgID)
		return err
	}); !ok {
		return nil, false
	}

	now := h.Now()
	uis := make([]view.ProfilePluginUI, 0, len(enabled))
	for _, name := range enabled {
		if _, _, hasUI := endpoint.UIEndpoint(name); !hasUI {
			continue
		}
		uis = append(uis, view.ProfilePluginUI{
			Name:  name,
			Title: upperFirst(name),
			// L'iframe s'authentifie par ce jeton : sandbouclée, elle ne
			// transporte pas le cookie de profil.
			Src: core.PluginUIPrefix + h.PluginUIToken(core.PluginViewMember, member.OrgID, member.ID, name, now) + "/",
		})
	}
	return uis, true
}

// HandleProfilePluginPage rend l'onglet d'un plugin. Un plugin inactif ou
// sans interface n'a pas d'onglet : sa page n'existe pas non plus.
func (h *Handlers) HandleProfilePluginPage(w http.ResponseWriter, r *http.Request) {
	member, r, minutes, ok := h.resolveProfile(w, r)
	if !ok {
		return
	}

	plugins, ok := h.profilePluginUIs(w, r, member)
	if !ok {
		return
	}

	name := r.PathValue("name")
	current, found := view.ProfilePluginUI{}, false
	for _, plugin := range plugins {
		if plugin.Name == name {
			current, found = plugin, true
			break
		}
	}
	if !found {
		http.NotFound(w, r)
		return
	}

	h.Render(w, r, http.StatusOK, view.ProfilePlugin(view.ProfilePluginPage{
		LinkID:    r.PathValue("link"),
		Header:    h.profileHeader(r, member, minutes),
		Current:   current,
		PluginUIs: plugins,
	}))
}

// HandleProfileEmail enregistre l'adresse et envoie le code (PRO-01).
func (h *Handlers) HandleProfileEmail(w http.ResponseWriter, r *http.Request) {
	member, r, _, ok := h.resolveProfile(w, r)
	if !ok {
		return
	}
	if !core.CheckCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}

	email := strings.TrimSpace(r.PostFormValue("email"))
	linkPath := "/p/" + r.PathValue("link")
	if email == "" || !strings.Contains(email, "@") || h.Mail == nil {
		http.Redirect(w, r, linkPath, http.StatusFound)
		return
	}

	code, err := verificationCode()
	if err != nil {
		http.Error(w, "erreur interne", http.StatusInternalServerError)
		return
	}

	if err := h.Mail.SendVerificationCode(r.Context(), member.Locale, email, code); err != nil {
		// L'adresse n'est pas journalisée : identifiants seulement.
		h.Logger.ErrorContext(r.Context(), "web: échec d'envoi d'un code de vérification", "member_id", member.ID, "error", err)
		http.Redirect(w, r, linkPath, http.StatusFound)
		return
	}

	h.Codes.Put(member.ID, email, code, h.Now())
	h.Logger.InfoContext(r.Context(), "web: code de vérification envoyé", "member_id", member.ID)

	http.Redirect(w, r, linkPath, http.StatusFound)
}

// verificationCode produit six chiffres aléatoires.
func verificationCode() (string, error) {
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("web: lecture d'aléa: %w", err)
	}
	n := binary.BigEndian.Uint32(raw[:]) % 1_000_000
	return fmt.Sprintf("%06d", n), nil
}

// lowerFirst met la première lettre en minuscule (insertion d'une tournure
// dans une phrase).
func lowerFirst(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		return s
	}
	return string(unicode.ToLower(r)) + s[size:]
}

// upperFirst met la première lettre en majuscule. Le nom d'un plugin est
// un identifiant technique en minuscules ; en onglet, il voisine des
// libellés rédigés (« Crédits », « Confidentialité ») où il détonnerait.
func upperFirst(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

// HandleProfileEmailVerify vérifie le code à six chiffres (PRO-01b).
func (h *Handlers) HandleProfileEmailVerify(w http.ResponseWriter, r *http.Request) {
	member, r, _, ok := h.resolveProfile(w, r)
	if !ok {
		return
	}
	if !core.CheckCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}

	now := h.Now()
	code := strings.TrimSpace(r.PostFormValue("code"))
	linkPath := "/p/" + r.PathValue("link")

	email, valid := h.Codes.Verify(member.ID, code, now)
	if !valid {
		http.Redirect(w, r, linkPath+"?bad_code=1", http.StatusFound)
		return
	}

	txOK := h.WithTx(w, r, func(tx *sql.Tx) error {
		fresh, found, err := h.Members.FindByID(r.Context(), tx, member.ID)
		if err != nil || !found {
			return err
		}
		fresh.Email = email
		fresh.EmailVerifiedAt = now
		fresh.UpdatedAt = now
		return h.Members.Update(r.Context(), tx, fresh)
	})
	if !txOK {
		return
	}

	h.Logger.InfoContext(r.Context(), "web: courriel de récupération vérifié", "member_id", member.ID)
	http.Redirect(w, r, linkPath, http.StatusFound)
}

// HandleProfileCredits — PRO-02.
func (h *Handlers) HandleProfileCredits(w http.ResponseWriter, r *http.Request) {
	member, r, minutes, ok := h.resolveProfile(w, r)
	if !ok {
		return
	}
	now := h.Now()
	monthFrom, monthTo := core.MonthBounds(now)

	page := view.CreditsPage{
		LinkID:        r.PathValue("link"),
		Header:        h.profileHeader(r, member, minutes),
		CSRFToken:     h.CSRFToken(w, r),
		StripeEnabled: h.Stripe != nil,
	}

	plugins, ok := h.profilePluginUIs(w, r, member)
	if !ok {
		return
	}
	page.PluginUIs = plugins

	switch {
	case r.URL.Query().Get("paid") == "1":
		page.Notice = i18n.TC(r.Context(), "credits.notice.paid")
		page.NoticeTone = "ok"
	case r.URL.Query().Get("canceled") == "1":
		page.Notice = i18n.TC(r.Context(), "credits.notice.cancelled")
	case r.URL.Query().Get("payment_error") == "1":
		page.Notice = i18n.TC(r.Context(), "credits.notice.error")
	}

	txOK := h.WithTx(w, r, func(tx *sql.Tx) error {
		org, found, err := h.Orgs.FindByID(r.Context(), tx, member.OrgID)
		if err != nil || !found {
			return err
		}
		balance, err := h.Wallet.Balance(r.Context(), tx, org.ID)
		if err != nil {
			return err
		}
		lastCredit, err := h.Wallet.LastCredit(r.Context(), tx, org.ID)
		if err != nil {
			return err
		}
		monthUsage, err := h.SingleOrgUsageCredits(r.Context(), tx, org.ID, monthFrom, monthTo)
		if err != nil {
			return err
		}
		rate, err := h.DailyRate(r.Context(), tx, org.ID, now)
		if err != nil {
			return err
		}

		state := core.ComputeWalletState(org, balance, lastCredit, monthUsage)
		page.State = state.State
		page.Balance = balance
		page.GaugeRef = state.GaugeRef
		page.GaugePct = state.GaugePct

		switch state.State {
		case "unlimited":
			// Rien à acheter, rien à surveiller : on montre l'usage du mois
			// et on s'arrête là — pas de solde, pas d'offres.
			page.UsedCredits = monthUsage
			page.BalanceHint = i18n.TC(r.Context(), "credits.balance.unlimited")
			return h.fillUsageSplit(r, tx, &page, org.ID, monthFrom, monthTo)
		case "offered":
			page.Allowance = org.MonthlyAllowance
			page.UsedCredits = monthUsage
			page.NextReset = i18n.TC(r.Context(), "credits.next_reset", strings.ToLower(view.FormatMonth(r.Context(), monthTo)))
			page.BalanceHint = offeredHint(r.Context(), monthUsage, org.MonthlyAllowance)
			return h.fillUsageSplit(r, tx, &page, org.ID, monthFrom, monthTo)
		case "empty":
			page.BalanceHint = i18n.TC(r.Context(), "duration.balance.empty")
		case "low":
			page.BalanceHint = i18n.TC(r.Context(), "credits.balance.remaining", lowerFirst(view.HumanUsageDuration(r.Context(), balance, rate)))
		default:
			page.BalanceHint = view.HumanUsageDuration(r.Context(), balance, rate)
		}

		// Offres effectives : celles de l'écran de tarification, sinon
		// celles de la configuration (voir Pricing.go).
		pricingSettings, err := h.Pricing(r.Context(), tx)
		if err != nil {
			return err
		}
		for i, pack := range pricingSettings.Packs {
			row := view.CreditPack{
				Index:    i,
				Credits:  pack.Credits,
				Duration: view.HumanPackDuration(r.Context(), pack.Credits, rate),
				// Les prix des offres sont hors taxes : Stripe ajoute la
				// TVA applicable au moment du paiement, et un prix
				// annoncé plus bas que le prix débité se découvrirait à
				// la première facture.
				PriceEUR: i18n.TC(r.Context(), "credits.price_excl_tax", core.FormatEuros(pack.PriceEUR)),
				Featured: pack.Featured,
			}
			if pack.Featured {
				row.FeaturedLabel = i18n.TC(r.Context(), "credits.featured_label")
				page.FeaturedPrice = row.PriceEUR
				page.FeaturedIndex = i
			}
			page.Packs = append(page.Packs, row)
		}
		if page.FeaturedPrice == "" && len(page.Packs) > 0 {
			page.FeaturedPrice = page.Packs[0].PriceEUR
			page.FeaturedIndex = page.Packs[0].Index
		}

		// Historique des trois derniers mois.
		for i := 0; i < 3; i++ {
			from := monthFrom.AddDate(0, -i, 0)
			to := from.AddDate(0, 1, 0)
			credits, err := h.SingleOrgUsageCredits(r.Context(), tx, org.ID, from, to)
			if err != nil {
				return err
			}
			label := view.FormatMonth(r.Context(), from)
			if i == 0 {
				label = i18n.TC(r.Context(), "credits.month_current", label)
			}
			page.Months = append(page.Months, view.MonthUsage{Label: label, Credits: credits})
		}

		return nil
	})
	if !txOK {
		return
	}

	h.Render(w, r, http.StatusOK, view.ProfileCredits(page))
}

// offeredHint décrit l'usage du mois d'une organisation offerte.
func offeredHint(ctx context.Context, used, allowance int64) string {
	if allowance <= 0 {
		return i18n.TC(ctx, "credits.hint.month_usage")
	}
	switch ratio := float64(used) / float64(allowance); {
	case ratio < 0.25:
		return i18n.TC(ctx, "credits.hint.barely_started")
	case ratio < 0.55:
		return i18n.TC(ctx, "credits.hint.under_half")
	case ratio < 0.85:
		return i18n.TC(ctx, "credits.hint.good_part")
	case ratio < 1:
		return i18n.TC(ctx, "credits.hint.approaching")
	default:
		return i18n.TC(ctx, "credits.hint.exhausted")
	}
}

// fillUsageSplit remplit la répartition « Ce mois-ci, en gros » (PRO-02
// offerte) : Conversations / Recherches / Images, sans jargon.
func (h *Handlers) fillUsageSplit(r *http.Request, tx *sql.Tx, page *view.CreditsPage, orgID string, from, to time.Time) error {
	aggregates, err := h.Usage.AggregateUsage(r.Context(), tx, from, to, []string{"agent", "kind"}, persistence.UsageFilter{OrgID: orgID})
	if err != nil {
		return err
	}

	rate := h.CreditRate(r.Context(), tx)
	buckets := map[string]int64{}
	var total int64
	for _, agg := range aggregates {
		credits := h.UsageCredits(agg.CostAmount, rate)
		key := "usage.split.conversations"
		switch {
		case agg.Keys[1] == "image":
			key = "usage.split.images"
		case agg.Keys[0] == "research":
			key = "usage.split.search"
		}
		buckets[key] += credits
		total += credits
	}
	if total == 0 {
		return nil
	}

	shades := []string{"", "soft", "faint"}
	// Les seaux sont indexés par CLÉ, pas par libellé traduit : deux
	// langues peuvent donner le même mot — « Images » en français comme en
	// anglais — et deux catégories se retrouveraient fusionnées.
	for i, key := range []string{"usage.split.conversations", "usage.split.search", "usage.split.images"} {
		credits := buckets[key]
		if credits == 0 {
			continue
		}
		page.Split = append(page.Split, view.UsageSplit{
			Label:   i18n.TC(r.Context(), key),
			Credits: credits,
			Pct:     int(credits * 100 / total),
			Shade:   shades[i],
		})
	}

	return nil
}

// firstName ne garde que le premier mot d'un nom affiché : la page
// d'ouverture salue, elle n'établit pas une identité.
func firstName(displayName string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(displayName), " ")

	return first
}

// HandleProfileOpen consomme le lien et ouvre la session, puis renvoie
// sur le profil. Séparé du GET pour que le préchargement d'un aperçu de
// messagerie ne grille pas le lien.
func (h *Handlers) HandleProfileOpen(w http.ResponseWriter, r *http.Request) {
	if _, _, _, ok := h.resolveProfile(w, r); !ok {
		return
	}

	http.Redirect(w, r, "/p/"+r.PathValue("link"), http.StatusFound)
}
