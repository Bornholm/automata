package model_test

import (
	"context"
	"testing"

	"github.com/bornholm/automata/internal/model"
)

func TestExecutionIdentityContext(t *testing.T) {
	ctx := context.Background()

	if _, ok := model.ExecutionIdentityFromContext(ctx); ok {
		t.Fatalf("expected no execution identity in fresh context")
	}

	identity := model.ExecutionIdentity{
		Trigger:     model.TriggerMessage,
		PrincipalID: "alice",
		OrgID:       "home",
		Provider:    "whatsapp",
		ChannelID:   "alice-priv",
		ChannelKind: model.ChannelPrivate,
		Scope:       model.ScopePersonal,
		ScopeID:     "alice",
	}

	ctx = model.WithExecutionIdentity(ctx, identity)

	got, ok := model.ExecutionIdentityFromContext(ctx)
	if !ok {
		t.Fatalf("expected execution identity in context")
	}

	if got != identity {
		t.Fatalf("got = %+v, want %+v", got, identity)
	}
}
