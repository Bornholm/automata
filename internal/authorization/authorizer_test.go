package authorization_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bornholm/automata/internal/apperr"
	"github.com/bornholm/automata/internal/authorization"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/model"
)

func testConfig() *config.Config {
	return &config.Config{
		Version: 1,
		Organization: config.Organization{
			ID:          "home",
			DisplayName: "Maison",
		},
		Identities: config.Identities{
			Roles: map[string]config.Role{
				"adult": {
					Permissions: []string{
						"memory.personal.read",
						"memory.personal.write",
						"memory.personal.delete",
						"memory.group.read",
						"memory.group.write",
						"memory.group.delete",
						"memory.org.read",
						"memory.org.write",
						"memory.org.delete",
					},
				},
				"child": {
					Permissions: []string{
						"memory.personal.read",
						"memory.personal.write",
						"memory.group.read",
						"memory.org.read",
					},
				},
			},
			Principals: []config.Principal{
				{ID: "alice", Kind: config.PrincipalKindHuman, DisplayName: "Alice", Roles: []string{"adult"}},
				{ID: "leo", Kind: config.PrincipalKindHuman, DisplayName: "Léo", Roles: []string{"child"}},
			},
		},
	}
}

func privateIdentity(principalID model.PrincipalID) model.ExecutionIdentity {
	return model.ExecutionIdentity{
		Trigger:        model.TriggerMessage,
		PrincipalID:    principalID,
		OrgID:          "home",
		ConversationID: model.ConversationID("whatsapp:" + string(principalID) + "-priv"),
		Provider:       "whatsapp",
		ChannelID:      string(principalID) + "-priv",
		ChannelKind:    model.ChannelPrivate,
		Scope:          model.ScopePersonal,
		ScopeID:        model.ScopeID(principalID),
	}
}

func groupIdentity(principalID model.PrincipalID) model.ExecutionIdentity {
	return model.ExecutionIdentity{
		Trigger:        model.TriggerMessage,
		PrincipalID:    principalID,
		OrgID:          "home",
		ConversationID: "whatsapp:group-1",
		Provider:       "whatsapp",
		ChannelID:      "group-1",
		ChannelKind:    model.ChannelGroup,
		Scope:          model.ScopeGroup,
		ScopeID:        "main-group",
	}
}

func TestAuthorizePersonalFromGroupRefused(t *testing.T) {
	a := authorization.NewAuthorizer(testConfig())

	err := a.Authorize(context.Background(), authorization.AuthorizationRequest{
		Identity:      groupIdentity("alice"),
		Permission:    "memory.personal.read",
		TargetOrgID:   "home",
		TargetScope:   model.ScopePersonal,
		TargetScopeID: "alice",
	})
	if !errors.Is(err, apperr.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}

func TestAuthorizeOrgWriteFromPrivateRefused(t *testing.T) {
	a := authorization.NewAuthorizer(testConfig())

	err := a.Authorize(context.Background(), authorization.AuthorizationRequest{
		Identity:      privateIdentity("alice"),
		Permission:    "memory.org.write",
		TargetOrgID:   "home",
		TargetScope:   model.ScopeOrg,
		TargetScopeID: "home",
	})
	if !errors.Is(err, apperr.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}

func TestAuthorizeOrgReadByChildAllowed(t *testing.T) {
	a := authorization.NewAuthorizer(testConfig())

	err := a.Authorize(context.Background(), authorization.AuthorizationRequest{
		Identity:      privateIdentity("leo"),
		Permission:    "memory.org.read",
		TargetOrgID:   "home",
		TargetScope:   model.ScopeOrg,
		TargetScopeID: "home",
	})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
}

func TestAuthorizePersonalWriteOwnerAllowed(t *testing.T) {
	a := authorization.NewAuthorizer(testConfig())

	err := a.Authorize(context.Background(), authorization.AuthorizationRequest{
		Identity:      privateIdentity("alice"),
		Permission:    "memory.personal.write",
		TargetOrgID:   "home",
		TargetScope:   model.ScopePersonal,
		TargetScopeID: "alice",
	})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
}

func TestAuthorizePersonalWriteOtherPrincipalRefused(t *testing.T) {
	a := authorization.NewAuthorizer(testConfig())

	err := a.Authorize(context.Background(), authorization.AuthorizationRequest{
		Identity:      privateIdentity("alice"),
		Permission:    "memory.personal.write",
		TargetOrgID:   "home",
		TargetScope:   model.ScopePersonal,
		TargetScopeID: "leo",
	})
	if !errors.Is(err, apperr.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}

func TestAuthorizeGroupDifferentGroupRefused(t *testing.T) {
	a := authorization.NewAuthorizer(testConfig())

	err := a.Authorize(context.Background(), authorization.AuthorizationRequest{
		Identity:      groupIdentity("alice"),
		Permission:    "memory.group.write",
		TargetOrgID:   "home",
		TargetScope:   model.ScopeGroup,
		TargetScopeID: "other-group",
	})
	if !errors.Is(err, apperr.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}

func TestAuthorizeGroupOwnGroupAllowed(t *testing.T) {
	a := authorization.NewAuthorizer(testConfig())

	err := a.Authorize(context.Background(), authorization.AuthorizationRequest{
		Identity:      groupIdentity("alice"),
		Permission:    "memory.group.write",
		TargetOrgID:   "home",
		TargetScope:   model.ScopeGroup,
		TargetScopeID: "main-group",
	})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
}

func TestAuthorizeOrgTraversalRefused(t *testing.T) {
	a := authorization.NewAuthorizer(testConfig())

	err := a.Authorize(context.Background(), authorization.AuthorizationRequest{
		Identity:      privateIdentity("alice"),
		Permission:    "memory.org.read",
		TargetOrgID:   "other-org",
		TargetScope:   model.ScopeOrg,
		TargetScopeID: "other-org",
	})
	if !errors.Is(err, apperr.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}

func TestAuthorizeCronPersonalRefusedByDefault(t *testing.T) {
	a := authorization.NewAuthorizer(testConfig())

	cronIdentity := model.ExecutionIdentity{
		Trigger:     model.TriggerCron,
		PrincipalID: "alice",
		OrgID:       "home",
		Provider:    "whatsapp",
		Scope:       model.ScopeOrg,
		ScopeID:     "home",
	}

	err := a.Authorize(context.Background(), authorization.AuthorizationRequest{
		Identity:      cronIdentity,
		Permission:    "memory.personal.read",
		TargetOrgID:   "home",
		TargetScope:   model.ScopePersonal,
		TargetScopeID: "alice",
	})
	if !errors.Is(err, apperr.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}

func TestAuthorizeCronOrgReadAllowed(t *testing.T) {
	a := authorization.NewAuthorizer(testConfig())

	cronIdentity := model.ExecutionIdentity{
		Trigger:     model.TriggerCron,
		PrincipalID: "alice",
		OrgID:       "home",
		Provider:    "whatsapp",
		Scope:       model.ScopeOrg,
		ScopeID:     "home",
	}

	err := a.Authorize(context.Background(), authorization.AuthorizationRequest{
		Identity:      cronIdentity,
		Permission:    "memory.org.read",
		TargetOrgID:   "home",
		TargetScope:   model.ScopeOrg,
		TargetScopeID: "home",
	})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
}
