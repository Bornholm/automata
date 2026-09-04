package registry

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/authorization"
	"github.com/bornholm/automata/internal/identity"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/weblink"
)

// tenantSource adosse à la persistance les interfaces des tenants
// enregistrés en ligne : résolution d'identité dynamique
// (identity.DynamicSource), rôles des membres
// (authorization.MemberRoleSource) et génération de liens de profil
// (agent.ProfileLinkGenerator). Un seul type les porte toutes : elles
// lisent les mêmes tables et partagent la même connexion.
type tenantSource struct {
	db      *persistence.DB
	baseURL string

	orgs         *persistence.OrganizationRepository
	orgSettings  *persistence.OrgSettingsRepository
	members      *persistence.MemberRepository
	bindings     *persistence.ChannelBindingRepository
	profileLinks *persistence.ProfileLinkRepository
}

// newTenantSource construit la source des tenants. baseURL sert à composer
// les liens de profil (cfg.Web.BaseURL).
func newTenantSource(db *persistence.DB, baseURL string) *tenantSource {
	return &tenantSource{
		db:           db,
		baseURL:      baseURL,
		orgs:         persistence.NewOrganizationRepository(),
		orgSettings:  persistence.NewOrgSettingsRepository(),
		members:      persistence.NewMemberRepository(),
		bindings:     persistence.NewChannelBindingRepository(),
		profileLinks: persistence.NewProfileLinkRepository(),
	}
}

// FindMemberByOrigin implémente identity.DynamicSource.
func (s *tenantSource) FindMemberByOrigin(ctx context.Context, provider, externalUserID, orgID string) (identity.DynamicMember, bool, error) {
	var (
		member persistence.Member
		found  bool
	)
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		member, found, err = s.members.FindByExternalUserInOrg(ctx, tx, provider, externalUserID, orgID)
		return err
	})
	if err != nil || !found {
		return identity.DynamicMember{}, false, err
	}

	return identity.DynamicMember{
		ID:          member.ID,
		OrgID:       member.OrgID,
		DisplayName: member.DisplayName,
		Role:        member.Role,
		Locale:      member.Locale,
	}, true, nil
}

// FindChannel implémente identity.DynamicSource.
func (s *tenantSource) FindChannel(ctx context.Context, provider, channelID string) (identity.DynamicChannel, bool, error) {
	var (
		binding persistence.ChannelBinding
		found   bool
	)
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		binding, found, err = s.bindings.Find(ctx, tx, provider, channelID)
		return err
	})
	if err != nil || !found {
		return identity.DynamicChannel{}, false, err
	}

	return identity.DynamicChannel{
		OrgID:       binding.OrgID,
		Kind:        model.ChannelKind(binding.Kind),
		Scope:       model.Scope(binding.Scope),
		ScopeID:     model.ScopeID(binding.ScopeID),
		DisplayName: binding.DisplayName,
		MemberID:    binding.MemberID,
	}, true, nil
}

// OrgDisplayName implémente identity.DynamicSource.
func (s *tenantSource) OrgDisplayName(ctx context.Context, orgID string) (string, bool, error) {
	var (
		org   persistence.Organization
		found bool
	)
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		org, found, err = s.orgs.FindByID(ctx, tx, orgID)
		return err
	})
	if err != nil || !found {
		return "", false, err
	}

	return org.DisplayName, true, nil
}

// MemberRole implémente authorization.MemberRoleSource. L'organisation est
// vérifiée ici : un membre n'a de droits que dans la sienne.
func (s *tenantSource) MemberRole(ctx context.Context, orgID, memberID string) (string, bool, error) {
	var (
		member persistence.Member
		found  bool
	)
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		member, found, err = s.members.FindByID(ctx, tx, memberID)
		return err
	})
	if err != nil || !found || member.OrgID != orgID {
		return "", false, err
	}

	return member.Role, true, nil
}

// profileLinkTTL borne la validité d'un lien de profil : le temps d'une
// visite, pas davantage (même durée que la session courte du serveur web).
const profileLinkTTL = 15 * time.Minute

// GenerateProfileLink implémente agent.ProfileLinkGenerator.
func (s *tenantSource) GenerateProfileLink(ctx context.Context, orgID, principalID string) (string, bool, error) {
	if s.baseURL == "" {
		return "", false, nil
	}

	id, secretHash, urlPath, err := weblink.NewProfileLink()
	if err != nil {
		return "", false, err
	}

	now := time.Now()
	ok := false

	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		member, found, err := s.members.FindByID(ctx, tx, principalID)
		if err != nil {
			return err
		}
		// Un principal déclaré en configuration (hors socle SaaS) n'a pas de
		// page de profil : l'outil le dit au modèle plutôt que d'inventer un
		// lien mort.
		if !found || member.OrgID != orgID {
			return nil
		}

		ok = true

		return s.profileLinks.Insert(ctx, tx, persistence.ProfileLink{
			ID:        id,
			MemberID:  member.ID,
			TokenHash: secretHash,
			Status:    persistence.ProfileLinkStatusPending,
			ExpiresAt: now.Add(profileLinkTTL),
			CreatedAt: now,
		})
	})
	if err != nil || !ok {
		return "", false, err
	}

	return strings.TrimSuffix(s.baseURL, "/") + "/p/" + urlPath, true, nil
}

// CustomizationFor implémente agent.OrgCustomizer : la personnalisation
// est lue à chaque tour, pour qu'un changement dans l'administration
// s'applique à la conversation suivante sans redémarrage.
func (s *tenantSource) CustomizationFor(ctx context.Context, orgID string) (agent.OrgCustomization, error) {
	var settings persistence.OrgSettings

	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		settings, _, err = s.orgSettings.Get(ctx, tx, orgID)
		return err
	})
	if err != nil {
		return agent.OrgCustomization{}, err
	}

	return agent.OrgCustomization{
		PromptExtra:    settings.PromptExtra,
		DisabledAgents: settings.DisabledAgents,
		MaxToolCalls:   settings.MaxToolCalls,
	}, nil
}

var (
	_ identity.DynamicSource         = &tenantSource{}
	_ agent.OrgCustomizer            = &tenantSource{}
	_ authorization.MemberRoleSource = &tenantSource{}
	_ agent.ProfileLinkGenerator     = &tenantSource{}
)
