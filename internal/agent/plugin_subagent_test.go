package agent_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/media"
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
	TimeoutSeconds         int
}

func (f *fakePluginCaller) CallPluginTool(_ context.Context, pluginName, toolName string, callCtx agent.PluginCallContext, argsJSON string, timeoutSeconds int) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakePluginCall{Plugin: pluginName, Tool: toolName, ArgsJSON: argsJSON, Ctx: callCtx, TimeoutSeconds: timeoutSeconds})
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

// fakeFileTransfer enregistre les transferts de fichiers hôte ↔ plugin.
type fakeFileTransfer struct {
	mu sync.Mutex

	putFilename string
	putMime     string
	putData     []byte
	putPath     string
	putIsError  bool
	putErrText  string
	putErr      error

	getPath     string
	getFilename string
	getMime     string
	getData     []byte
	getErr      error
}

func (f *fakeFileTransfer) PutPluginFile(_ context.Context, _ string, _ agent.PluginCallContext, filename, mimeType string, data []byte) (string, bool, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putFilename, f.putMime, f.putData = filename, mimeType, append([]byte(nil), data...)
	return f.putPath, f.putIsError, f.putErrText, f.putErr
}

func (f *fakeFileTransfer) GetPluginFile(_ context.Context, _ string, _ agent.PluginCallContext, path string, _ int64) (string, string, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getPath = path
	return f.getFilename, f.getMime, f.getData, f.getErr
}

func fileCapableSpec() agent.PluginSubAgentSpec {
	return agent.PluginSubAgentSpec{
		PluginName:       "workspace",
		SystemPrompt:     "You edit files in a sandbox.",
		Description:      "Edits files.",
		PermissionDomain: "workspace",
		SupportsFiles:    true,
		Tools: []agent.PluginToolSpec{
			{Name: "run_command", Description: "Run a command.", SchemaJSON: `{"type":"object","properties":{"script":{"type":"string"}}}`, ReadOnly: true, TimeoutSeconds: 330},
		},
	}
}

func TestPluginSubAgent_ImportAttachmentPushesTheRightBytes(t *testing.T) {
	transfer := &fakeFileTransfer{putPath: "input.mp4"}
	caller := &fakePluginCaller{result: "ok"}
	video := []byte("octets mp4 de la video")

	client := &fakeCompletionClient{
		responseFunc: func(turn int, _ *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("c1", "import_attachment", `{"filename":"clip.mp4"}`)), nil
			}
			return scriptedFinalResponse("Fichier importé."), nil
		},
	}

	subAgent := agent.NewPluginSubAgent(fileCapableSpec(), client, caller, 0, nil).
		WithFiles(transfer, 32<<20)

	_, err := subAgent.Execute(context.Background(), delegation.Request{
		AgentID:  "workspace",
		Goal:     "Crop the video",
		Identity: pluginTestIdentity(),
		Attachments: []media.Media{
			{Kind: media.KindVideo, MimeType: "video/mp4", Filename: "clip.mp4", Data: video, ToolOnly: true},
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if transfer.putFilename != "clip.mp4" || transfer.putMime != "video/mp4" {
		t.Fatalf("PutFile appelé avec %q / %q", transfer.putFilename, transfer.putMime)
	}
	if string(transfer.putData) != string(video) {
		t.Fatal("PutFile n'a pas reçu les octets de la pièce jointe")
	}
}

func TestPluginSubAgent_ImportAttachmentMissingGuidesTheAgent(t *testing.T) {
	transfer := &fakeFileTransfer{putPath: "input.mp4"}
	caller := &fakePluginCaller{result: "ok"}

	var toolResult string
	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("c1", "import_attachment", `{"filename":"absent.mp4"}`)), nil
			}
			for _, m := range opts.Messages {
				if strings.Contains(m.Content(), "send the file again") {
					toolResult = m.Content()
				}
			}
			return scriptedFinalResponse("Peux-tu renvoyer le fichier ?"), nil
		},
	}

	subAgent := agent.NewPluginSubAgent(fileCapableSpec(), client, caller, 0, nil).
		WithFiles(transfer, 32<<20)

	if _, err := subAgent.Execute(context.Background(), delegation.Request{
		AgentID:  "workspace",
		Goal:     "Crop the video",
		Identity: pluginTestIdentity(),
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if toolResult == "" {
		t.Fatal("le résultat d'outil devrait orienter l'agent vers un renvoi du fichier")
	}
	if transfer.putFilename != "" {
		t.Fatal("aucun PutFile ne devait être tenté")
	}
}

func TestPluginSubAgent_AttachFileCollectsTheMedia(t *testing.T) {
	transfer := &fakeFileTransfer{
		getFilename: "sortie.mp4",
		getMime:     "video/mp4",
		getData:     []byte("octets du resultat"),
	}
	caller := &fakePluginCaller{result: "ok"}

	client := &fakeCompletionClient{
		responseFunc: func(turn int, _ *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("c1", "attach_file", `{"path":"sortie.mp4"}`)), nil
			}
			return scriptedFinalResponse("Voilà la vidéo recadrée."), nil
		},
	}

	subAgent := agent.NewPluginSubAgent(fileCapableSpec(), client, caller, 0, nil).
		WithFiles(transfer, 32<<20)

	result, err := subAgent.Execute(context.Background(), delegation.Request{
		AgentID:  "workspace",
		Goal:     "Crop the video",
		Identity: pluginTestIdentity(),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if transfer.getPath != "sortie.mp4" {
		t.Fatalf("GetFile appelé avec %q", transfer.getPath)
	}
	if len(result.Attachments) != 1 {
		t.Fatalf("pièces jointes du résultat: got %d, expected 1", len(result.Attachments))
	}
	got := result.Attachments[0]
	if got.Filename != "sortie.mp4" || got.MimeType != "video/mp4" || string(got.Data) != "octets du resultat" {
		t.Fatalf("média collecté = %+v", media.Media{Filename: got.Filename, MimeType: got.MimeType})
	}
	if got.ToolOnly {
		t.Fatal("un fichier renvoyé à l'utilisateur ne doit pas être marqué ToolOnly")
	}
}

func TestPluginSubAgent_AttachFileFailureIsRelayedNotFatal(t *testing.T) {
	transfer := &fakeFileTransfer{getErr: context.DeadlineExceeded}
	caller := &fakePluginCaller{result: "ok"}

	client := &fakeCompletionClient{
		responseFunc: func(turn int, _ *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("c1", "attach_file", `{"path":"sortie.mp4"}`)), nil
			}
			return scriptedFinalResponse("Je n'ai pas pu joindre le fichier."), nil
		},
	}

	subAgent := agent.NewPluginSubAgent(fileCapableSpec(), client, caller, 0, nil).
		WithFiles(transfer, 32<<20)

	result, err := subAgent.Execute(context.Background(), delegation.Request{
		AgentID:  "workspace",
		Goal:     "Crop the video",
		Identity: pluginTestIdentity(),
	})
	if err != nil {
		t.Fatalf("un échec d'outil ne doit jamais avorter le tour: %v", err)
	}
	if len(result.Attachments) != 0 {
		t.Fatal("aucune pièce jointe ne devait être collectée")
	}
}

func TestPluginSubAgent_FileToolsAbsentWithoutSupport(t *testing.T) {
	transfer := &fakeFileTransfer{}
	caller := &fakePluginCaller{result: "ok"}

	var toolNames []string
	client := &fakeCompletionClient{
		responseFunc: func(_ int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			for _, tool := range opts.Tools {
				toolNames = append(toolNames, tool.Name())
			}
			return scriptedFinalResponse("Rien à faire."), nil
		},
	}

	// testPluginSpec() ne déclare pas SupportsFiles.
	subAgent := agent.NewPluginSubAgent(testPluginSpec(), client, caller, 0, nil).
		WithFiles(transfer, 32<<20)

	if _, err := subAgent.Execute(context.Background(), delegation.Request{
		AgentID:  "email",
		Goal:     "Do nothing",
		Identity: pluginTestIdentity(),
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	for _, name := range toolNames {
		if name == "import_attachment" || name == "attach_file" {
			t.Fatalf("outil fichier %q monté alors que le plugin ne les déclare pas", name)
		}
	}
}

func TestPluginSubAgent_ToolTimeoutIsForwardedToTheHost(t *testing.T) {
	caller := &fakePluginCaller{result: "ok"}

	client := &fakeCompletionClient{
		responseFunc: func(turn int, _ *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("c1", "run_command", `{"script":"ls"}`)), nil
			}
			return scriptedFinalResponse("Fait."), nil
		},
	}

	subAgent := agent.NewPluginSubAgent(fileCapableSpec(), client, caller, 0, nil)

	if _, err := subAgent.Execute(context.Background(), delegation.Request{
		AgentID:  "workspace",
		Goal:     "List files",
		Identity: pluginTestIdentity(),
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(caller.calls) != 1 || caller.calls[0].TimeoutSeconds != 330 {
		t.Fatalf("appels = %+v", caller.calls)
	}
}
