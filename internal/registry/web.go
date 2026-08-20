package registry

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"time"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
)

// WebBootstrap importe les tenants de la configuration YAML dans les
// tables du socle SaaS (« automata web bootstrap ») : les organisations
// depuis cfg.Organizations et les membres humains depuis
// cfg.Identities.Principals, avec leur identité de messagerie quand
// cfg.Origins la connaît. Idempotent : les lignes existantes ne sont
// jamais écrasées — l'interface web devient la source de vérité dès
// qu'elle a la main.
func WebBootstrap(ctx context.Context, cfg *config.Config, out io.Writer) error {
	db, err := persistence.OpenWithEncryption(ctx, cfg.Storage.Application, cfg.Storage.EncryptionKey)
	if err != nil {
		return fmt.Errorf("registry: ouverture de la persistance: %w", err)
	}
	defer db.Close()

	orgs := persistence.NewOrganizationRepository()
	members := persistence.NewMemberRepository()
	now := time.Now().UTC()

	// L'identité de messagerie de chaque principal, si déclarée.
	origins := map[string]struct{ provider, externalUserID string }{}
	for _, origin := range cfg.Origins {
		origins[origin.PrincipalID] = struct{ provider, externalUserID string }{origin.Provider, origin.ExternalUserID}
	}

	var createdOrgs, createdMembers int

	err = db.WithTx(ctx, func(tx *sql.Tx) error {
		for _, org := range cfg.AllOrganizations() {
			_, exists, err := orgs.FindByID(ctx, tx, org.ID)
			if err != nil {
				return err
			}
			if exists {
				continue
			}

			if err := orgs.Insert(ctx, tx, persistence.Organization{
				ID:          org.ID,
				DisplayName: org.DisplayName,
				// Les organisations historiques de la configuration sont
				// offertes par la maison, allocation à régler dans l'admin.
				Offered:   true,
				CreatedAt: now,
				UpdatedAt: now,
			}, true); err != nil {
				return err
			}
			createdOrgs++
		}

		for _, principal := range cfg.Identities.Principals {
			if principal.Kind != config.PrincipalKindHuman {
				continue
			}

			for _, orgID := range principalOrgs(cfg, principal) {
				_, exists, err := members.FindByID(ctx, tx, principal.ID)
				if err != nil {
					return err
				}
				if exists {
					break
				}

				member := persistence.Member{
					ID:          principal.ID,
					OrgID:       orgID,
					DisplayName: principal.DisplayName,
					Role:        persistence.MemberRoleMember,
					CreatedAt:   now,
					UpdatedAt:   now,
				}
				if origin, ok := origins[principal.ID]; ok {
					member.Provider = origin.provider
					member.ExternalUserID = origin.externalUserID
					member.LinkedAt = now
				}

				if err := members.Insert(ctx, tx, member, true); err != nil {
					return err
				}
				createdMembers++
				break
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("registry: bootstrap des tenants: %w", err)
	}

	fmt.Fprintf(out, "bootstrap terminé : %d organisation(s) et %d membre(s) importés\n", createdOrgs, createdMembers)

	return nil
}

// principalOrgs retourne les organisations d'un principal : son champ Orgs
// s'il est renseigné, sinon l'unique organisation de l'instance.
func principalOrgs(cfg *config.Config, principal config.Principal) []string {
	if len(principal.Orgs) > 0 {
		return principal.Orgs
	}

	var ids []string
	for _, org := range cfg.AllOrganizations() {
		ids = append(ids, org.ID)
	}
	if len(ids) > 1 {
		return nil
	}
	return ids
}
