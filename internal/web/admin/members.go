package admin

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"

	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/web/core"
	"github.com/bornholm/automata/internal/web/view"
	"github.com/bornholm/automata/internal/weblink"
)

func (h *Handlers) HandleMemberNewForm(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("id")

	var orgName string
	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		orgName = h.OrgDisplayName(r.Context(), tx, orgID)
		return nil
	})
	if !ok {
		return
	}

	h.Render(w, r, http.StatusOK, view.AdminMemberNew(view.MemberNewPage{
		Platforms: h.SidebarPlatforms(),
		CSRFToken: h.CSRFToken(w, r),
		OrgID:     orgID,
		OrgName:   orgName,
	}))
}

// HandleMemberCreate pré-crée le membre puis génère immédiatement son
// jeton (« Créer et générer le jeton », parcours A).
func (h *Handlers) HandleMemberCreate(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("id")
	name := strings.TrimSpace(r.PostFormValue("display_name"))
	role := r.PostFormValue("role")
	email := strings.TrimSpace(r.PostFormValue("email"))

	if name == "" {
		http.Redirect(w, r, "/admin/orgs/"+orgID+"/members/new", http.StatusFound)
		return
	}
	if role != persistence.MemberRoleOwner && role != persistence.MemberRoleReadOnly {
		role = persistence.MemberRoleMember
	}

	now := h.Now()
	rawID, err := weblink.RandomCrockford(10)
	if err != nil {
		http.Error(w, "erreur interne", http.StatusInternalServerError)
		return
	}
	memberID := strings.ToLower(rawID)

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		return h.Members.Insert(r.Context(), tx, persistence.Member{
			ID:          memberID,
			OrgID:       orgID,
			DisplayName: name,
			Role:        role,
			Email:       email,
			CreatedAt:   now,
			UpdatedAt:   now,
		}, false)
	})
	if !ok {
		return
	}

	h.Logger.InfoContext(r.Context(), "web: membre pré-créé", "org_id", orgID, "member_id", memberID)
	h.generateMemberToken(w, r, memberID)
}

// generateMemberToken révoque les jetons en attente du membre, en crée un
// nouveau et redirige vers sa fiche avec la clé de révélation.
func (h *Handlers) generateMemberToken(w http.ResponseWriter, r *http.Request, memberID string) {
	now := h.Now()

	clear, hash, display, err := weblink.NewLinkToken()
	if err != nil {
		http.Error(w, "erreur interne", http.StatusInternalServerError)
		return
	}
	rawID, err := weblink.RandomCrockford(10)
	if err != nil {
		http.Error(w, "erreur interne", http.StatusInternalServerError)
		return
	}

	var orgID string
	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		member, found, err := h.Members.FindByID(r.Context(), tx, memberID)
		if err != nil || !found {
			return err
		}
		orgID = member.OrgID

		if err := h.LinkTokens.RevokePendingByMember(r.Context(), tx, memberID); err != nil {
			return err
		}
		return h.LinkTokens.Insert(r.Context(), tx, persistence.LinkToken{
			ID:        strings.ToLower(rawID),
			Kind:      persistence.LinkTokenKindPersonal,
			MemberID:  memberID,
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

	h.Logger.InfoContext(r.Context(), "web: jeton personnel généré", "member_id", memberID, "org_id", orgID)
	http.Redirect(w, r, "/admin/members/"+memberID+"?reveal="+key, http.StatusFound)
}

func (h *Handlers) HandleMemberToken(w http.ResponseWriter, r *http.Request) {
	h.generateMemberToken(w, r, r.PathValue("id"))
}

func (h *Handlers) HandleMemberTokenRevoke(w http.ResponseWriter, r *http.Request) {
	memberID := r.PathValue("id")

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		return h.LinkTokens.RevokePendingByMember(r.Context(), tx, memberID)
	})
	if !ok {
		return
	}

	h.Logger.InfoContext(r.Context(), "web: jeton personnel révoqué", "member_id", memberID)
	http.Redirect(w, r, "/admin/members/"+memberID, http.StatusFound)
}

// maskedIdentity décrit l'identité de messagerie sans exposer
// l'identifiant brut complet : seuls les derniers caractères de la partie
// locale sont montrés — le domaine technique (« @lid », « @g.us ») ne dit
// rien à personne et ne distingue rien.
func maskedIdentity(providerType, externalUserID string) string {
	local, _, found := strings.Cut(externalUserID, "@")
	if !found {
		local = externalUserID
	}
	if len(local) > 4 {
		local = local[len(local)-4:]
	}

	label := core.PlatformDisplayName(providerType, providerType)
	if local == "" {
		return label
	}

	return label + " · …" + local
}

// buildMemberPage assemble la fiche ADM-04.
func (h *Handlers) buildMemberPage(ctx context.Context, tx *sql.Tx, w http.ResponseWriter, r *http.Request, member persistence.Member) (view.MemberPage, error) {
	page := view.MemberPage{
		Platforms: h.SidebarPlatforms(),
		CSRFToken: h.CSRFToken(w, r),
		ID:        member.ID,
		OrgID:     member.OrgID,
		OrgName:   h.OrgDisplayName(ctx, tx, member.OrgID),
		Name:      member.DisplayName,
		Role:      member.Role,
		Email:     member.Email,
		Linked:    member.Linked(),
	}

	if member.Linked() {
		page.Chips = append(page.Chips, view.Chip{Label: "Lié", Tone: "ok", Dot: true})
		providerType := h.ProviderTypeOf(member.Provider)
		if providerType == "" {
			providerType = member.Provider
		}
		page.LinkedIdentity = maskedIdentity(providerType, member.ExternalUserID)
		page.LinkedChannels = h.MemberChannels(ctx, tx, member.ID, member.OrgID)

		monthFrom, monthTo := core.MonthBounds(h.Now())
		aggregates, err := h.Usage.AggregateUsage(ctx, tx, monthFrom, monthTo, nil, persistence.UsageFilter{PrincipalID: member.ID})
		if err != nil {
			return page, err
		}
		rate := h.CreditRate(ctx, tx)
		var total int64
		for _, agg := range aggregates {
			total += h.UsageCredits(agg.CostAmount, rate)
		}
		page.MonthUsage = view.FormatCredits(total)
	} else {
		page.Chips = append(page.Chips, view.Chip{Label: "Pré-créé", Tone: "neutral", Dot: true})
	}

	switch {
	case member.Email == "":
		page.Chips = append(page.Chips, view.Chip{Label: "Courriel non renseigné", Tone: "warn"})
	case member.EmailVerifiedAt.IsZero():
		page.Chips = append(page.Chips, view.Chip{Label: "Courriel non vérifié", Tone: "warn"})
	default:
		page.Chips = append(page.Chips, view.Chip{Label: "Courriel vérifié", Tone: "ok"})
	}

	token, found, err := h.LinkTokens.LatestByMember(ctx, tx, member.ID)
	if err != nil {
		return page, err
	}
	if found {
		status := token.Status
		if token.Expired(h.Now()) {
			status = "expired"
		}
		page.Token = &view.TokenPanelData{
			Eyebrow:          "Jeton personnel de liaison",
			Help:             "Transmettez ce code à " + view.FirstName(member.DisplayName) + " par le moyen de votre choix. Une fois envoyé à Automata dans sa messagerie, la liaison se fait automatiquement. Après fermeture de cette page, le code ne pourra plus être affiché — seulement régénéré.",
			Expires:          "exp. " + view.FormatShortDate(token.ExpiresAt),
			RegenerateAction: "/admin/members/" + member.ID + "/token",
			RevokeAction:     "/admin/members/" + member.ID + "/token/revoke",
			CSRFToken:        page.CSRFToken,
			Status:           status,
		}
	}

	return page, nil
}

// HandleMember — ADM-04.
func (h *Handlers) HandleMember(w http.ResponseWriter, r *http.Request) {
	memberID := r.PathValue("id")
	now := h.Now()

	var (
		page  view.MemberPage
		found bool
	)
	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		member, exists, err := h.Members.FindByID(r.Context(), tx, memberID)
		if err != nil || !exists {
			return err
		}
		found = true

		page, err = h.buildMemberPage(r.Context(), tx, w, r, member)
		return err
	})
	if !ok {
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}

	if key := r.URL.Query().Get("reveal"); key != "" {
		if value, ok := h.Reveals.Pop(key, now); ok && page.Token != nil {
			page.Token.Display = value.Display
			page.Token.Clipboard = value.Clear
		}
	}
	if key := r.URL.Query().Get("link"); key != "" {
		if value, ok := h.Reveals.Pop(key, now); ok {
			page.ProfileLink = value.Clear
		}
	}
	if r.URL.Query().Get("saved") == "1" {
		page.Flash = "Fiche enregistrée."
	}

	h.Render(w, r, http.StatusOK, view.AdminMember(page))
}

func (h *Handlers) HandleMemberUpdate(w http.ResponseWriter, r *http.Request) {
	memberID := r.PathValue("id")
	now := h.Now()

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		member, found, err := h.Members.FindByID(r.Context(), tx, memberID)
		if err != nil || !found {
			return err
		}

		if name := strings.TrimSpace(r.PostFormValue("display_name")); name != "" {
			member.DisplayName = name
		}
		if role := r.PostFormValue("role"); role == persistence.MemberRoleMember ||
			role == persistence.MemberRoleOwner || role == persistence.MemberRoleReadOnly {
			member.Role = role
		}
		if email := strings.TrimSpace(r.PostFormValue("email")); email != member.Email {
			// Une adresse changée par l'admin repart non vérifiée.
			member.Email = email
			member.EmailVerifiedAt = time.Time{}
		}
		member.UpdatedAt = now

		return h.Members.Update(r.Context(), tx, member)
	})
	if !ok {
		return
	}

	http.Redirect(w, r, "/admin/members/"+memberID+"?saved=1", http.StatusFound)
}

// HandleMemberProfileLink génère un lien de profil de test (lot A : la
// génération conversationnelle par l'agent arrive au lot B).
func (h *Handlers) HandleMemberProfileLink(w http.ResponseWriter, r *http.Request) {
	memberID := r.PathValue("id")
	now := h.Now()

	id, secretHash, urlPath, err := weblink.NewProfileLink()
	if err != nil {
		http.Error(w, "erreur interne", http.StatusInternalServerError)
		return
	}

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		return h.ProfileLinks.Insert(r.Context(), tx, persistence.ProfileLink{
			ID:        id,
			MemberID:  memberID,
			TokenHash: secretHash,
			Status:    persistence.ProfileLinkStatusPending,
			ExpiresAt: now.Add(core.ProfileSessionTTL),
			CreatedAt: now,
		})
	})
	if !ok {
		return
	}

	fullURL := strings.TrimSuffix(h.Cfg.Web.BaseURL, "/") + "/p/" + urlPath
	key, err := h.Reveals.Put(core.RevealValue{Clear: fullURL}, now)
	if err != nil {
		http.Error(w, "erreur interne", http.StatusInternalServerError)
		return
	}

	h.Logger.InfoContext(r.Context(), "web: lien de profil généré", "member_id", memberID, "link_id", id)
	http.Redirect(w, r, "/admin/members/"+memberID+"?link="+key, http.StatusFound)
}
