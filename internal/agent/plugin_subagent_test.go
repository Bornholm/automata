package agent_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/model"
)

// fakePluginCaller enregistre les appels d'outils reçus.
type fakePluginCaller struct {
	mu     sync.Mutex
	calls  []fakePluginCall
	result string
}

type fakePluginCall struct {
	Plugin, Tool, ArgsJSON string
	Ctx                    agent.PluginCallContext
}

func (f *fakePluginCaller) CallPluginTool(_ context.Context, pluginName, toolName string, callCtx agent.PluginCallContext, argsJSON string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakePluginCall{Plugin: pluginName, Tool: toolName, ArgsJSON: argsJSON, Ctx: callCtx})
	return f.result, false, nil
}

func pluginTestIdentity() model.ExecutionIdentity {
	return model.ExecutionIdentity{
		Trigger:              model.TriggerMessage,
		PrincipalID:          "member-42",
		PrincipalDisplayName: "Camille",
		OrgID:                "atelier",
		ChannelKind:          model.ChannelPrivate,
		Scope:                model.ScopePersonal,
		ScopeID:              "member-42",
	}
}

func testPluginSpec() agent.PluginSubAgentSpec {
	return agent.PluginSubAgentSpec{
		PluginName:       "email",
		SystemPrompt:     "You are an email assistant.",
		Description:      "Reads and drafts emails.",
		PermissionDomain: "email",
		Tools: []agent.PluginToolSpec{
			{Name: "email_read", Description: "Read an email.", SchemaJSON: `{"type":"object","properties":{"id":{"type":"string"}}}`, ReadOnly: true},
			{Name: "email_send", Description: "Send an email.", SchemaJSON: `{"type":"object","properties":{"to":{"type":"string"},"body":{"type":"string"}}}`},
		},
	}
}

// Un outil de lecture est exécuté pendant le tour, avec l'identité
// construite par l'hôte — jamais par le modèle.
func TestPluginSubAgent_ReadToolExecutesWithHostIdentity(t *testing.T) {
	caller := &fakePluginCaller{result: "Subject: Bonjour"}

	client := &fakeCompletionClient{
		responseFunc: func(turn int, _ *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("c1", "email_read", `{"id":"INBOX:12"}`)), nil
			}
			return scriptedFinalResponse("L'email dit bonjour."), nil
		},
	}

	subAgent := agent.NewPluginSubAgent(testPluginSpec(), client, caller, 0, nil)

	result, err := subAgent.Execute(context.Background(), delegation.Request{
		AgentID:  "email",
		Goal:     "Read email INBOX:12",
		Identity: pluginTestIdentity(),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Summary != "L'email dit bonjour." {
		t.Errorf("résumé inattendu: %q", result.Summary)
	}

	if len(caller.calls) != 1 {
		t.Fatalf("%d appel(s) d'outil, attendu 1", len(caller.calls))
	}
	call := caller.calls[0]
	if call.Plugin != "email" || call.Tool != "email_read" {
		t.Errorf("appel inattendu: %+v", call)
	}
	if call.Ctx.OrgID != "atelier" || call.Ctx.MemberID != "member-42" || call.Ctx.Scope != "personal" {
		t.Errorf("contexte d'appel inattendu: %+v", call.Ctx)
	}
	if call.Ctx.IdempotencyKey != "" {
		t.Errorf("une lecture ne porte pas de clé d'idempotence: %+v", call.Ctx)
	}
}

// Un outil d'écriture n'est JAMAIS exécuté pendant le tour : il devient
// une action proposée, à confirmer par l'utilisateur. C'est l'invariant
// central — aucun réglage ne peut le débrayer.
func TestPluginSubAgent_WriteToolProposesInsteadOfExecuting(t *testing.T) {
	caller := &fakePluginCaller{}

	client := &fakeCompletionClient{
		responseFunc: func(turn int, _ *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("c1", "email_send", `{"to":"yann@example.test","body":"Salut"}`)), nil
			}
			return scriptedFinalResponse("J'ai préparé l'envoi, en attente de ta confirmation."), nil
		},
	}

	subAgent := agent.NewPluginSubAgent(testPluginSpec(), client, caller, 0, nil)

	result, err := subAgent.Execute(context.Background(), delegation.Request{
		AgentID:  "email",
		Goal:     "Send a hello email to Yann",
		Identity: pluginTestIdentity(),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(caller.calls) != 0 {
		t.Fatalf("l'outil d'écriture a été exécuté pendant le tour: %+v", caller.calls)
	}

	if len(result.ProposedActions) != 1 {
		t.Fatalf("%d action(s) proposée(s), attendu 1", len(result.ProposedActions))
	}
	action := result.ProposedActions[0]
	if action.MCPServer != "plugin:email" || action.ToolName != "email_send" {
		t.Errorf("action inattendue: %+v", action)
	}
	if action.RequiredPermission != "email.personal.write" {
		t.Errorf("permission requise = %q, attendu email.personal.write", action.RequiredPermission)
	}
	if action.Arguments["to"] != "yann@example.test" {
		t.Errorf("arguments perdus: %+v", action.Arguments)
	}
}

// Le PrincipalID est un identifiant interne : il part au plugin (processus
// de confiance) mais jamais dans les messages envoyés au modèle.
func TestPluginSubAgent_PrincipalIDNeverReachesTheModel(t *testing.T) {
	caller := &fakePluginCaller{result: "ok"}

	client := &fakeCompletionClient{
		responseFunc: func(turn int, _ *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			return scriptedFinalResponse("Fini."), nil
		},
	}

	subAgent := agent.NewPluginSubAgent(testPluginSpec(), client, caller, 0, nil)

	if _, err := subAgent.Execute(context.Background(), delegation.Request{
		AgentID:  "email",
		Goal:     "Check the inbox",
		Identity: pluginTestIdentity(),
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	for _, opts := range client.optsHistory {
		for _, msg := range opts.Messages {
			if strings.Contains(msg.Content(), "member-42") {
				t.Fatalf("le PrincipalID apparaît dans un message au modèle: %q", msg.Content())
			}
		}
	}
}
