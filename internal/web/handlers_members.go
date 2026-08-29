package web

import (
	"context"
	"database/sql"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/web/core"
	"github.com/bornholm/automata/internal/web/view"
	"github.com/bornholm/automata/internal/weblink"
)

func (s *Server) handleMemberNewForm(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("id")

	var orgName string
	ok := s.WithTx(w, r, func(tx *sql.Tx) error {
		orgName = s.OrgDisplayName(r.Context(), tx, orgID)
		return nil
	})
	if !ok {
		return
	}

	s.Render(w, r, http.StatusOK, view.AdminMemberNew(view.MemberNewPage{
		Platforms: s.SidebarPlatforms(),
		CSRFToken: s.CSRFToken(w, r),
		OrgID:     orgID,
		OrgName:   orgName,
	}))
}

// handleMemberCreate pré-crée le membre puis génère immédiatement son
// jeton (« Créer et générer le jeton », parcours A).
func (s *Server) handleMemberCreate(w http.ResponseWriter, r *http.Request) {
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

	now := s.Now()
	rawID, err := weblink.RandomCrockford(10)
	if err != nil {
		http.Error(w, "erreur interne", http.StatusInternalServerError)
		return
	}
	memberID := strings.ToLower(rawID)

	ok := s.WithTx(w, r, func(tx *sql.Tx) error {
		return s.Members.Insert(r.Context(), tx, persistence.Member{
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

	s.Logger.InfoContext(r.Context(), "web: membre pré-créé", "org_id", orgID, "member_id", memberID)
	s.generateMemberToken(w, r, memberID)
}

// generateMemberToken révoque les jetons en attente du membre, en crée un
// nouveau et redirige vers sa fiche avec la clé de révélation.
func (s *Server) generateMemberToken(w http.ResponseWriter, r *http.Request, memberID string) {
	now := s.Now()

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
	ok := s.WithTx(w, r, func(tx *sql.Tx) error {
		member, found, err := s.Members.FindByID(r.Context(), tx, memberID)
		if err != nil || !found {
			return err
		}
		orgID = member.OrgID

		if err := s.LinkTokens.RevokePendingByMember(r.Context(), tx, memberID); err != nil {
			return err
		}
		return s.LinkTokens.Insert(r.Context(), tx, persistence.LinkToken{
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

	key, err := s.Reveals.Put(core.RevealValue{Clear: clear, Display: display}, now)
	if err != nil {
		http.Error(w, "erreur interne", http.StatusInternalServerError)
		return
	}

	s.Logger.InfoContext(r.Context(), "web: jeton personnel généré", "member_id", memberID, "org_id", orgID)
	http.Redirect(w, r, "/admin/members/"+memberID+"?reveal="+key, http.StatusFound)
}

func (s *Server) handleMemberToken(w http.ResponseWriter, r *http.Request) {
	s.generateMemberToken(w, r, r.PathValue("id"))
}

func (s *Server) handleMemberTokenRevoke(w http.ResponseWriter, r *http.Request) {
	memberID := r.PathValue("id")

	ok := s.WithTx(w, r, func(tx *sql.Tx) error {
		return s.LinkTokens.RevokePendingByMember(r.Context(), tx, memberID)
	})
	if !ok {
		return
	}

	s.Logger.InfoContext(r.Context(), "web: jeton personnel révoqué", "member_id", memberID)
	http.Redirect(w, r, "/admin/members/"+memberID, http.StatusFound)
}

// memberChannels retourne les canaux où le membre est présent : ceux de
// la configuration (après bootstrap, les identifiants coïncident avec les
// principals) et ceux rattachés en ligne — sa conversation privée, et
// les groupes de son organisation. Sans les seconds, la fiche d'un membre
// rattaché par jeton n'affichait aucun canal.
func (s *Server) memberChannels(ctx context.Context, q persistence.Querier, memberID, orgID string) []view.OrgChannelRow {
	var rows []view.OrgChannelRow

	if orgID != "" {
		bound, err := s.Bindings.ListByOrg(ctx, q, orgID)
		if err != nil {
			// L'absence de cette liste ne justifie pas de refuser la
			// fiche entière : le reste de la page reste juste.
			s.Logger.ErrorContext(ctx, "web: lecture des canaux d'un membre", "member_id", memberID, "error", err)
		}
		for _, binding := range bound {
			// Un canal privé n'appartient qu'à son membre ; un groupe est
			// celui de toute l'organisation.
			if binding.MemberID != "" && binding.MemberID != memberID {
				continue
			}
			rows = append(rows, view.OrgChannelRow{
				PlatformType: s.ProviderTypeOf(binding.Provider),
				Name:         binding.DisplayName,
				Kind:         channelKindLabelFromScope(binding.Kind),
			})
		}
	}

	for _, ch := range s.Cfg.Channels {
		if ch.PrincipalID != memberID && !slices.Contains(ch.Members, memberID) {
			continue
		}
		rows = append(rows, view.OrgChannelRow{
			PlatformType: s.ProviderTypeOf(ch.Provider),
			Name:         s.ChannelDisplayName(ch),
			Kind:         core.ChannelKindLabel(ch.Kind),
		})
	}
	return rows
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
func (s *Server) buildMemberPage(ctx context.Context, tx *sql.Tx, w http.ResponseWriter, r *http.Request, member persistence.Member) (view.MemberPage, error) {
	page := view.MemberPage{
		Platforms: s.SidebarPlatforms(),
		CSRFToken: s.CSRFToken(w, r),
		ID:        member.ID,
		OrgID:     member.OrgID,
		OrgName:   s.OrgDisplayName(ctx, tx, member.OrgID),
		Name:      member.DisplayName,
		Role:      member.Role,
		Email:     member.Email,
		Linked:    member.Linked(),
	}

	if member.Linked() {
		page.Chips = append(page.Chips, view.Chip{Label: "Lié", Tone: "ok", Dot: true})
		providerType := s.ProviderTypeOf(member.Provider)
		if providerType == "" {
			providerType = member.Provider
		}
		page.LinkedIdentity = maskedIdentity(providerType, member.ExternalUserID)
		page.LinkedChannels = s.memberChannels(ctx, tx, member.ID, member.OrgID)

		monthFrom, monthTo := core.MonthBounds(s.Now())
		aggregates, err := s.Usage.AggregateUsage(ctx, tx, monthFrom, monthTo, nil, persistence.UsageFilter{PrincipalID: member.ID})
		if err != nil {
			return page, err
		}
		rate := s.CreditRate(ctx, tx)
		var total int64
		for _, agg := range aggregates {
			total += s.UsageCredits(agg.CostAmount, rate)
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

	token, found, err := s.LinkTokens.LatestByMember(ctx, tx, member.ID)
	if err != nil {
		return page, err
	}
	if found {
		status := token.Status
		if token.Expired(s.Now()) {
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

// handleMember — ADM-04.
func (s *Server) handleMember(w http.ResponseWriter, r *http.Request) {
	memberID := r.PathValue("id")
	now := s.Now()

	var (
		page  view.MemberPage
		found bool
	)
	ok := s.WithTx(w, r, func(tx *sql.Tx) error {
		member, exists, err := s.Members.FindByID(r.Context(), tx, memberID)
		if err != nil || !exists {
			return err
		}
		found = true

		page, err = s.buildMemberPage(r.Context(), tx, w, r, member)
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
		if value, ok := s.Reveals.Pop(key, now); ok && page.Token != nil {
			page.Token.Display = value.Display
			page.Token.Clipboard = value.Clear
		}
	}
	if key := r.URL.Query().Get("link"); key != "" {
		if value, ok := s.Reveals.Pop(key, now); ok {
			page.ProfileLink = value.Clear
		}
	}
	if r.URL.Query().Get("saved") == "1" {
		page.Flash = "Fiche enregistrée."
	}

	s.Render(w, r, http.StatusOK, view.AdminMember(page))
}

func (s *Server) handleMemberUpdate(w http.ResponseWriter, r *http.Request) {
	memberID := r.PathValue("id")
	now := s.Now()

	ok := s.WithTx(w, r, func(tx *sql.Tx) error {
		member, found, err := s.Members.FindByID(r.Context(), tx, memberID)
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

		return s.Members.Update(r.Context(), tx, member)
	})
	if !ok {
		return
	}

	http.Redirect(w, r, "/admin/members/"+memberID+"?saved=1", http.StatusFound)
}

// handleMemberProfileLink génère un lien de profil de test (lot A : la
// génération conversationnelle par l'agent arrive au lot B).
func (s *Server) handleMemberProfileLink(w http.ResponseWriter, r *http.Request) {
	memberID := r.PathValue("id")
	now := s.Now()

	id, secretHash, urlPath, err := weblink.NewProfileLink()
	if err != nil {
		http.Error(w, "erreur interne", http.StatusInternalServerError)
		return
	}

	ok := s.WithTx(w, r, func(tx *sql.Tx) error {
		return s.ProfileLinks.Insert(r.Context(), tx, persistence.ProfileLink{
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

	fullURL := strings.TrimSuffix(s.Cfg.Web.BaseURL, "/") + "/p/" + urlPath
	key, err := s.Reveals.Put(core.RevealValue{Clear: fullURL}, now)
	if err != nil {
		http.Error(w, "erreur interne", http.StatusInternalServerError)
		return
	}

	s.Logger.InfoContext(r.Context(), "web: lien de profil généré", "member_id", memberID, "link_id", id)
	http.Redirect(w, r, "/admin/members/"+memberID+"?link="+key, http.StatusFound)
}
