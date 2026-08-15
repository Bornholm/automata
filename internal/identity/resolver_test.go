package identity_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bornholm/automata/internal/apperr"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/identity"
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
				{ID: "bob", Kind: config.PrincipalKindHuman, DisplayName: "Bob", Roles: []string{"adult"}},
			},
		},
		Origins: []config.Origin{
			{Provider: "whatsapp", ExternalUserID: "alice-ext", PrincipalID: "alice"},
			{Provider: "whatsapp", ExternalUserID: "leo-ext", PrincipalID: "leo"},
			{Provider: "whatsapp", ExternalUserID: "bob-ext", PrincipalID: "bob"},
		},
		Channels: []config.Channel{
			{
				Provider:    "whatsapp",
				ChannelID:   "group-1",
				DisplayName: "Groupe principal",
				Kind:        config.ChannelKindGroup,
				OrgID:       "home",
				Scope:       config.ScopeGroup,
				ScopeID:     "main-group",
				Activation:  "mention",
				Members:     []string{"alice", "leo"},
			},
			{
				Provider:    "whatsapp",
				ChannelID:   "alice-priv",
				Kind:        config.ChannelKindPrivate,
				OrgID:       "home",
				Scope:       config.ScopePersonal,
				ScopeID:     "alice",
				PrincipalID: "alice",
			},
		},
	}
}

func TestResolveMessageKnownOriginPrivateChannel(t *testing.T) {
	r, err := identity.NewResolver(testConfig())
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	execIdentity, conv, err := r.ResolveMessage(context.Background(), "whatsapp", "alice-ext", "alice-priv")
	if err != nil {
		t.Fatalf("ResolveMessage: %v", err)
	}

	if execIdentity.PrincipalID != "alice" {
		t.Errorf("PrincipalID = %q, want alice", execIdentity.PrincipalID)
	}

	if execIdentity.OrgID != "home" {
		t.Errorf("OrgID = %q, want home", execIdentity.OrgID)
	}

	if execIdentity.ChannelKind != model.ChannelPrivate {
		t.Errorf("ChannelKind = %q, want private", execIdentity.ChannelKind)
	}

	if execIdentity.Scope != model.ScopePersonal {
		t.Errorf("Scope = %q, want personal", execIdentity.Scope)
	}

	if execIdentity.ScopeID != "alice" {
		t.Errorf("ScopeID = %q, want alice", execIdentity.ScopeID)
	}

	if execIdentity.Trigger != model.TriggerMessage {
		t.Errorf("Trigger = %q, want message", execIdentity.Trigger)
	}

	wantConvID := model.ConversationID("whatsapp:alice-priv")
	if execIdentity.ConversationID != wantConvID {
		t.Errorf("ConversationID = %q, want %q", execIdentity.ConversationID, wantConvID)
	}

	if conv.ID != wantConvID {
		t.Errorf("conv.ID = %q, want %q", conv.ID, wantConvID)
	}

	if conv.Kind != model.ChannelPrivate {
		t.Errorf("conv.Kind = %q, want private", conv.Kind)
	}
}

func TestResolveMessageUnknownOrigin(t *testing.T) {
	r, err := identity.NewResolver(testConfig())
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	_, _, err = r.ResolveMessage(context.Background(), "whatsapp", "unknown-ext", "alice-priv")
	if !errors.Is(err, apperr.ErrUnknownOrigin) {
		t.Fatalf("err = %v, want ErrUnknownOrigin", err)
	}
}

func TestResolveMessageKnownGroupChannel(t *testing.T) {
	r, err := identity.NewResolver(testConfig())
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	execIdentity, conv, err := r.ResolveMessage(context.Background(), "whatsapp", "alice-ext", "group-1")
	if err != nil {
		t.Fatalf("ResolveMessage: %v", err)
	}

	if execIdentity.ChannelKind != model.ChannelGroup {
		t.Errorf("ChannelKind = %q, want group", execIdentity.ChannelKind)
	}

	if execIdentity.Scope != model.ScopeGroup {
		t.Errorf("Scope = %q, want group", execIdentity.Scope)
	}

	if execIdentity.ScopeID != "main-group" {
		t.Errorf("ScopeID = %q, want main-group", execIdentity.ScopeID)
	}

	if conv.Scope != model.ScopeGroup {
		t.Errorf("conv.Scope = %q, want group", conv.Scope)
	}
}

func TestResolveMessageUnknownChannel(t *testing.T) {
	r, err := identity.NewResolver(testConfig())
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	_, _, err = r.ResolveMessage(context.Background(), "whatsapp", "alice-ext", "unknown-channel")
	if !errors.Is(err, apperr.ErrUnknownChannel) {
		t.Fatalf("err = %v, want ErrUnknownChannel", err)
	}
}

func TestResolveMessageNonMemberOfKnownGroup(t *testing.T) {
	r, err := identity.NewResolver(testConfig())
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	_, _, err = r.ResolveMessage(context.Background(), "whatsapp", "bob-ext", "group-1")
	if !errors.Is(err, apperr.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}

func TestEffectivePermissions(t *testing.T) {
	cfg := testConfig()

	perms, err := identity.EffectivePermissions(cfg, "home", "leo")
	if err != nil {
		t.Fatalf("EffectivePermissions: %v", err)
	}

	if _, ok := perms["memory.org.read"]; !ok {
		t.Errorf("expected memory.org.read to be granted to leo")
	}

	if _, ok := perms["memory.org.write"]; ok {
		t.Errorf("did not expect memory.org.write to be granted to leo")
	}
}

func TestEffectivePermissionsUnknownOrg(t *testing.T) {
	cfg := testConfig()

	_, err := identity.EffectivePermissions(cfg, "other-org", "leo")
	if !errors.Is(err, apperr.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}
