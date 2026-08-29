package agent_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

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

// Rappeler un outil d'écriture avec les mêmes arguments ne duplique pas
// l'action dans le plan : observé en production, publish_space proposé
// trois fois dans le même tour par un modèle qui ne comprenait pas
// « en attente de confirmation ». Le doublon reçoit un résultat qui le dit
// sans ambiguïté.
func TestPluginSubAgent_DuplicateWriteProposalIsCollapsed(t *testing.T) {
	caller := &fakePluginCaller{}

	var duplicateResult string
	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			switch turn {
			case 0:
				return scriptedToolCallResponse(llm.NewToolCall("c1", "email_send", `{"to":"yann@example.test","body":"Salut"}`)), nil
			case 1:
				return scriptedToolCallResponse(llm.NewToolCall("c2", "email_send", `{"body":"Salut","to":"yann@example.test"}`)), nil
			case 2:
				return scriptedToolCallResponse(llm.NewToolCall("c3", "email_send", `{"to":"yann@example.test","body":"Autre corps"}`)), nil
			default:
				for _, m := range opts.Messages {
					if strings.Contains(m.Content(), "ALREADY recorded") {
						duplicateResult = m.Content()
					}
				}
				return scriptedFinalResponse("Deux envois préparés, en attente de ta confirmation."), nil
			}
		},
	}

	subAgent := agent.NewPluginSubAgent(testPluginSpec(), client, caller, 0, nil)

	result, err := subAgent.Execute(context.Background(), delegation.Request{
		AgentID:  "email",
		Goal:     "Send hello emails",
		Identity: pluginTestIdentity(),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Le rappel à arguments identiques (ordre des clés indifférent) est
	// absorbé ; l'action aux arguments différents passe.
	if len(result.ProposedActions) != 2 {
		t.Fatalf("%d action(s) proposée(s), attendu 2: %+v", len(result.ProposedActions), result.ProposedActions)
	}
	if duplicateResult == "" {
		t.Fatal("le doublon doit recevoir un résultat qui dit que l'action est déjà enregistrée")
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
				if strings.Contains(m.Content(), "send it again") {
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
	if !strings.Contains(toolResult, "list_files") {
		t.Fatalf("il doit d'abord renvoyer l'agent vers son workspace: %q", toolResult)
	}
	if !strings.Contains(toolResult, "NO file is attached") {
		t.Fatalf("sans aucune pièce jointe, le résultat doit le dire pour couper court aux devinettes: %q", toolResult)
	}
	if transfer.putFilename != "" {
		t.Fatal("aucun PutFile ne devait être tenté")
	}
}

// Quand des pièces existent mais que le nom demandé est faux, le résultat
// liste les noms réellement disponibles : observé en production, douze
// noms devinés avant le bon.
func TestPluginSubAgent_ImportAttachmentMissListsAvailableNames(t *testing.T) {
	transfer := &fakeFileTransfer{putPath: "input.mp4"}
	caller := &fakePluginCaller{result: "ok"}

	var toolResult string
	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("c1", "import_attachment", `{"filename":"photo.jpg"}`)), nil
			}
			for _, m := range opts.Messages {
				if strings.Contains(m.Content(), "available for import") {
					toolResult = m.Content()
				}
			}
			return scriptedFinalResponse("Je prends le bon nom."), nil
		},
	}

	subAgent := agent.NewPluginSubAgent(fileCapableSpec(), client, caller, 0, nil).
		WithFiles(transfer, 32<<20)

	if _, err := subAgent.Execute(context.Background(), delegation.Request{
		AgentID:  "workspace",
		Goal:     "Use the photo",
		Identity: pluginTestIdentity(),
		Attachments: []media.Media{
			{Kind: media.KindImage, MimeType: "image/jpeg", Filename: "IMG_20260829.jpg", Data: []byte("jpeg")},
		},
		RecentAttachments: []media.Media{
			{Kind: media.KindVideo, MimeType: "video/mp4", Filename: "clip.mp4", Data: []byte("mp4")},
			{Kind: media.KindImage, MimeType: "image/jpeg", Filename: "IMG_20260829.jpg", Data: []byte("jpeg-ancien")},
		},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if toolResult == "" {
		t.Fatal("le résultat d'outil doit lister les fichiers disponibles")
	}
	for _, name := range []string{"IMG_20260829.jpg", "clip.mp4"} {
		if !strings.Contains(toolResult, name) {
			t.Errorf("le nom %q manque dans le résultat: %q", name, toolResult)
		}
	}
	if strings.Count(toolResult, "IMG_20260829.jpg") != 1 {
		t.Errorf("les noms doivent être dédoublonnés: %q", toolResult)
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

// L'agent voit l'image par le client multimodal : les octets partent au
// modèle de vision, jamais au modèle du sous-agent, et seule la
// description revient dans le résultat d'outil.
func TestPluginSubAgent_ViewFileAsksTheVisionModel(t *testing.T) {
	transfer := &fakeFileTransfer{
		getFilename: "frame.png",
		getMime:     "image/png",
		getData:     []byte("octets de l'image"),
	}
	caller := &fakePluginCaller{result: "ok"}

	var (
		visionAttachments int
		visionQuestion    string
	)
	vision := &fakeCompletionClient{
		responseFunc: func(_ int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			for _, msg := range opts.Messages {
				if msg.Role() == llm.RoleUser {
					visionQuestion = msg.Content()
					visionAttachments += len(msg.Attachments())
				}
			}
			return scriptedFinalResponse("The logo sits at 378, 2, 90x90 on a 478x850 image."), nil
		},
	}

	var toolResult string
	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("c1", "view_file",
					`{"path":"frame.png","question":"where is the logo?"}`)), nil
			}
			for _, msg := range opts.Messages {
				if msg.Role() == llm.RoleTool {
					toolResult = msg.Content()
				}
			}
			return scriptedFinalResponse("Je sais où est le logo."), nil
		},
	}

	subAgent := agent.NewPluginSubAgent(fileCapableSpec(), client, caller, 0, nil).
		WithFiles(transfer, 32<<20).
		WithVisionClient(vision)

	if _, err := subAgent.Execute(context.Background(), delegation.Request{
		AgentID:  "workspace",
		Goal:     "Remove the logo",
		Identity: pluginTestIdentity(),
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if transfer.getPath != "frame.png" {
		t.Fatalf("GetFile appelé avec %q", transfer.getPath)
	}
	if visionAttachments != 1 {
		t.Fatalf("images soumises au modèle de vision: got %d, expected 1", visionAttachments)
	}
	if !strings.Contains(visionQuestion, "where is the logo?") {
		t.Fatalf("question transmise au modèle de vision = %q", visionQuestion)
	}
	if !strings.Contains(toolResult, "378, 2") {
		t.Fatalf("la description n'est pas revenue à l'agent: %q", toolResult)
	}
}

// Une vidéo n'est pas soumise au modèle de vision : le résultat d'outil
// dit à l'agent d'en extraire une trame d'abord.
func TestPluginSubAgent_ViewFileRefusesNonImagesWithGuidance(t *testing.T) {
	transfer := &fakeFileTransfer{
		getFilename: "att-0.mp4",
		getMime:     "video/mp4",
		getData:     []byte("octets de la video"),
	}
	caller := &fakePluginCaller{result: "ok"}

	vision := &fakeCompletionClient{
		responseFunc: func(int, *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			t.Fatal("une vidéo ne doit jamais être soumise au modèle de vision")
			return nil, nil
		},
	}

	var toolResult string
	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("c1", "view_file",
					`{"path":"att-0.mp4","question":"where is the logo?"}`)), nil
			}
			for _, msg := range opts.Messages {
				if msg.Role() == llm.RoleTool {
					toolResult = msg.Content()
				}
			}
			return scriptedFinalResponse("J'extrais une trame."), nil
		},
	}

	subAgent := agent.NewPluginSubAgent(fileCapableSpec(), client, caller, 0, nil).
		WithFiles(transfer, 32<<20).
		WithVisionClient(vision)

	if _, err := subAgent.Execute(context.Background(), delegation.Request{
		AgentID:  "workspace",
		Goal:     "Remove the logo",
		Identity: pluginTestIdentity(),
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(toolResult, "frames:v") {
		t.Fatalf("le refus doit expliquer comment extraire une trame: %q", toolResult)
	}
}

// Sans client multimodal configuré, l'outil n'est pas montré au modèle :
// l'hôte ne propose que ce qu'il sait servir.
func TestPluginSubAgent_ViewFileAbsentWithoutVisionClient(t *testing.T) {
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

	subAgent := agent.NewPluginSubAgent(fileCapableSpec(), client, caller, 0, nil).
		WithFiles(transfer, 32<<20)

	if _, err := subAgent.Execute(context.Background(), delegation.Request{
		AgentID:  "workspace",
		Goal:     "Do nothing",
		Identity: pluginTestIdentity(),
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	for _, name := range toolNames {
		if name == "view_file" {
			t.Fatal("view_file monté sans client multimodal")
		}
	}
}

// Un fichier reçu à un message précédent reste importable : c'est le geste
// naturel (« voici la vidéo », puis « enlève le logo ») et il doit marcher.
func TestPluginSubAgent_ImportAttachmentFindsAnEarlierFile(t *testing.T) {
	transfer := &fakeFileTransfer{putPath: "video.mp4"}
	caller := &fakePluginCaller{result: "ok"}

	var input string
	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				for _, msg := range opts.Messages {
					if msg.Role() == llm.RoleUser {
						input = msg.Content()
					}
				}
				return scriptedToolCallResponse(llm.NewToolCall("c1", "import_attachment", `{"filename":"video.mp4"}`)), nil
			}
			return scriptedFinalResponse("Importée."), nil
		},
	}

	subAgent := agent.NewPluginSubAgent(fileCapableSpec(), client, caller, 0, nil).
		WithFiles(transfer, 32<<20)

	if _, err := subAgent.Execute(context.Background(), delegation.Request{
		AgentID:  "workspace",
		Goal:     "Remove the logo",
		Identity: pluginTestIdentity(),
		// Aucune pièce jointe sur le message courant : la vidéo est arrivée
		// au message précédent.
		RecentAttachments: []media.Media{{
			Kind:     media.KindVideo,
			MimeType: "video/mp4",
			Filename: "video.mp4",
			Data:     []byte("octets de la video"),
			ToolOnly: true,
		}},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if string(transfer.putData) != "octets de la video" {
		t.Fatalf("octets poussés au plugin = %q", string(transfer.putData))
	}
	if !strings.Contains(input, "video.mp4") {
		t.Fatalf("les fichiers antérieurs doivent être annoncés au modèle: %q", input)
	}
	if !strings.Contains(input, "earlier in this conversation") {
		t.Fatalf("l'annonce ne doit pas les présenter comme jointes au message courant: %q", input)
	}
}

// À noms égaux, le fichier du message courant l'emporte sur son homonyme
// plus ancien : c'est celui que l'utilisateur vient d'envoyer.
func TestPluginSubAgent_CurrentAttachmentWinsOverEarlierNamesake(t *testing.T) {
	transfer := &fakeFileTransfer{putPath: "video.mp4"}
	caller := &fakePluginCaller{result: "ok"}

	client := &fakeCompletionClient{
		responseFunc: func(turn int, _ *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("c1", "import_attachment", `{"filename":"video.mp4"}`)), nil
			}
			return scriptedFinalResponse("Importée."), nil
		},
	}

	subAgent := agent.NewPluginSubAgent(fileCapableSpec(), client, caller, 0, nil).
		WithFiles(transfer, 32<<20)

	if _, err := subAgent.Execute(context.Background(), delegation.Request{
		AgentID:  "workspace",
		Goal:     "Remove the logo",
		Identity: pluginTestIdentity(),
		Attachments: []media.Media{{
			Kind: media.KindVideo, MimeType: "video/mp4", Filename: "video.mp4",
			Data: []byte("la nouvelle"), ToolOnly: true,
		}},
		RecentAttachments: []media.Media{{
			Kind: media.KindVideo, MimeType: "video/mp4", Filename: "video.mp4",
			Data: []byte("l'ancienne"), ToolOnly: true,
		}},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if string(transfer.putData) != "la nouvelle" {
		t.Fatalf("le fichier du tour courant devait l'emporter, poussé: %q", string(transfer.putData))
	}
}

// La capacité est déclarée par le plugin ET servie par l'hôte : sans
// WithFiles, le sous-agent ne prétend pas savoir manipuler des fichiers.
func TestPluginSubAgent_SupportsFilesReflectsWhatTheHostCanServe(t *testing.T) {
	caller := &fakePluginCaller{result: "ok"}
	client := &fakeCompletionClient{
		responseFunc: func(int, *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			return scriptedFinalResponse("ok"), nil
		},
	}

	if agent.NewPluginSubAgent(fileCapableSpec(), client, caller, 0, nil).SupportsFiles() {
		t.Fatal("sans WithFiles, le sous-agent ne peut rien servir")
	}
	if !agent.NewPluginSubAgent(fileCapableSpec(), client, caller, 0, nil).
		WithFiles(&fakeFileTransfer{}, 32<<20).SupportsFiles() {
		t.Fatal("avec WithFiles et un plugin qui les déclare, la capacité doit être vraie")
	}
	if agent.NewPluginSubAgent(testPluginSpec(), client, caller, 0, nil).
		WithFiles(&fakeFileTransfer{}, 32<<20).SupportsFiles() {
		t.Fatal("un plugin qui ne déclare pas SupportsFiles ne doit jamais être FileCapable")
	}
}

// Une image jointe n'est jamais transmise au modèle d'un sous-agent à
// outils fichiers : le client des plugins peut être texte-seul, et il
// rejetterait alors la requête entière. L'agent en reçoit le nom, ce dont
// import_attachment a besoin.
func TestPluginSubAgent_NeverSendsAttachmentsToTheModel(t *testing.T) {
	transfer := &fakeFileTransfer{putPath: "photo.jpg"}
	caller := &fakePluginCaller{result: "ok"}

	var (
		sentAttachments int
		input           string
	)
	client := &fakeCompletionClient{
		responseFunc: func(_ int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			for _, msg := range opts.Messages {
				if msg.Role() == llm.RoleUser {
					input = msg.Content()
					sentAttachments += len(msg.Attachments())
				}
			}
			return scriptedFinalResponse("Vu."), nil
		},
	}

	subAgent := agent.NewPluginSubAgent(fileCapableSpec(), client, caller, 0, nil).
		WithFiles(transfer, 32<<20)

	if _, err := subAgent.Execute(context.Background(), delegation.Request{
		AgentID:  "workspace",
		Goal:     "Remove the logo from the photo",
		Identity: pluginTestIdentity(),
		// Une photo ordinaire : visible du modèle, donc NON marquée ToolOnly.
		Attachments: []media.Media{{
			Kind:     media.KindImage,
			MimeType: "image/jpeg",
			Filename: "photo.jpg",
			Data:     []byte("octets jpeg"),
		}},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if sentAttachments != 0 {
		t.Fatalf("aucune pièce jointe ne doit partir au modèle du sous-agent, %d transmise(s)", sentAttachments)
	}
	if !strings.Contains(input, "photo.jpg") {
		t.Fatalf("le nom du fichier doit être annoncé à l'agent: %q", input)
	}
}

// Un service de vision en panne est annoncé comme définitif pour le tour :
// sans cela, l'agent rappelle l'outil à chaque étape et épuise son budget
// sans rien produire.
func TestPluginSubAgent_ViewFileFailureStopsFurtherAttempts(t *testing.T) {
	transfer := &fakeFileTransfer{
		getFilename: "frame.png",
		getMime:     "image/png",
		getData:     []byte("octets"),
	}
	caller := &fakePluginCaller{result: "ok"}

	visionCalls := 0
	vision := &fakeCompletionClient{
		responseFunc: func(int, *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			visionCalls++
			return nil, errors.New("circuit breaker is open")
		},
	}

	var lastToolResult string
	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			for _, msg := range opts.Messages {
				if msg.Role() == llm.RoleTool {
					lastToolResult = msg.Content()
				}
			}
			if turn < 3 {
				return scriptedToolCallResponse(llm.NewToolCall("c", "view_file",
					`{"path":"frame.png","question":"where is the logo?"}`)), nil
			}
			return scriptedFinalResponse("Je n'ai pas pu regarder."), nil
		},
	}

	subAgent := agent.NewPluginSubAgent(fileCapableSpec(), client, caller, 0, nil).
		WithFiles(transfer, 32<<20).
		WithVisionClient(vision)

	if _, err := subAgent.Execute(context.Background(), delegation.Request{
		AgentID:  "workspace",
		Goal:     "Remove the logo",
		Identity: pluginTestIdentity(),
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if visionCalls != 1 {
		t.Fatalf("le modèle de vision ne devait être sollicité qu'une fois, %d appels", visionCalls)
	}
	if !strings.Contains(lastToolResult, "Do not call view_file again") {
		t.Fatalf("l'agent doit être averti que l'outil est hors service: %q", lastToolResult)
	}
}

// Le budget de temps clôt la boucle proprement : le sous-agent conclut avec
// ce qu'il a plutôt que de laisser expirer le délai du pipeline, ce qui ne
// rendrait rien à l'utilisateur après plusieurs minutes d'attente.
func TestPluginSubAgent_TimeBudgetConcludesInsteadOfExpiring(t *testing.T) {
	caller := &fakePluginCaller{result: "sortie de commande"}

	client := &fakeCompletionClient{
		responseFunc: func(turn int, _ *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("c1", "email_read", `{"id":"1"}`)), nil
			}
			return scriptedFinalResponse("Voici ce que j'ai pu faire."), nil
		},
	}

	// Le contexte n'a presque plus de temps : moins que la réserve de
	// conclusion. La boucle doit s'arrêter dès la première vérification.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	subAgent := agent.NewPluginSubAgent(testPluginSpec(), client, caller, 0, nil)

	result, err := subAgent.Execute(ctx, delegation.Request{
		AgentID:  "email",
		Goal:     "Read the mail",
		Identity: pluginTestIdentity(),
	})
	if err != nil {
		t.Fatalf("le budget de temps doit produire une conclusion, pas une erreur: %v", err)
	}
	if result.Summary == "" {
		t.Fatal("la conclusion ne doit pas être vide")
	}
}

// Une variante de conclusion en échec ne doit pas emporter tout le tour :
// c'est précisément ce que les suivantes existent pour rattraper. Un modèle
// à raisonnement répond « http 200 » sans contenu exploitable quand sa
// réponse part dans le canal de réflexion, et la variante qui désactive ce
// canal aboutit là où la première a échoué.
func TestPluginSubAgent_ConclusionFallsBackAfterAFailedAttempt(t *testing.T) {
	caller := &fakePluginCaller{result: "sortie"}

	var completions int
	client := &fakeCompletionClient{
		responseFunc: func(turn int, _ *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			completions++
			switch {
			case turn == 0:
				// La boucle consomme son unique appel d'outil.
				return scriptedToolCallResponse(llm.NewToolCall("c1", "email_read", `{"id":"1"}`)), nil
			case turn == 1:
				// Première variante de conclusion : échec du fournisseur.
				return nil, errors.New("http 200: ")
			default:
				return scriptedFinalResponse("Voici ce que j'ai trouvé."), nil
			}
		},
	}

	spec := testPluginSpec()
	spec.MaxToolCalls = 1

	subAgent := agent.NewPluginSubAgent(spec, client, caller, 0, nil)

	result, err := subAgent.Execute(context.Background(), delegation.Request{
		AgentID:  "email",
		Goal:     "Read the mail",
		Identity: pluginTestIdentity(),
	})
	if err != nil {
		t.Fatalf("une variante de repli devait aboutir: %v", err)
	}
	if result.Summary != "Voici ce que j'ai trouvé." {
		t.Fatalf("conclusion obtenue = %q", result.Summary)
	}
	if completions < 3 {
		t.Fatalf("la variante suivante devait être tentée après l'échec, %d appels", completions)
	}
}

// Un appel de modèle qui échoue EN COURS de boucle ne doit pas emporter le
// travail déjà fait : les outils ont tourné, le fichier est peut-être déjà
// produit. Le tour se conclut avec la matière acquise.
func TestPluginSubAgent_ModelFailureMidLoopStillConcludes(t *testing.T) {
	transfer := &fakeFileTransfer{
		getFilename: "sortie.jpg",
		getMime:     "image/jpeg",
		getData:     []byte("octets du resultat"),
	}
	caller := &fakePluginCaller{result: "ok"}

	client := &fakeCompletionClient{
		responseFunc: func(turn int, _ *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			switch turn {
			case 0:
				return scriptedToolCallResponse(llm.NewToolCall("c1", "attach_file", `{"path":"sortie.jpg"}`)), nil
			case 1:
				// La passerelle répond 200 avec un corps vide.
				return nil, errors.New("http 200: ")
			default:
				return scriptedFinalResponse("J'ai masqué le logo, le fichier est joint."), nil
			}
		},
	}

	subAgent := agent.NewPluginSubAgent(fileCapableSpec(), client, caller, 0, nil).
		WithFiles(transfer, 32<<20)

	result, err := subAgent.Execute(context.Background(), delegation.Request{
		AgentID:  "workspace",
		Goal:     "Remove the logo",
		Identity: pluginTestIdentity(),
	})
	if err != nil {
		t.Fatalf("le tour devait se conclure malgré l'échec: %v", err)
	}
	if result.Summary == "" {
		t.Fatal("la conclusion ne doit pas être vide")
	}
	// Le fichier produit doit survivre à l'incident : c'est tout l'enjeu.
	if len(result.Attachments) != 1 || result.Attachments[0].Filename != "sortie.jpg" {
		t.Fatalf("la pièce jointe déjà produite doit être conservée: %+v", result.Attachments)
	}
}

// Sans matière, il n'y a rien à sauver : l'erreur remonte telle quelle.
func TestPluginSubAgent_ModelFailureWithoutMaterialSurfaces(t *testing.T) {
	caller := &fakePluginCaller{result: "ok"}

	client := &fakeCompletionClient{
		responseFunc: func(int, *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			return nil, errors.New("http 500: fournisseur indisponible")
		},
	}

	subAgent := agent.NewPluginSubAgent(testPluginSpec(), client, caller, 0, nil)

	if _, err := subAgent.Execute(context.Background(), delegation.Request{
		AgentID:  "email",
		Goal:     "Read the mail",
		Identity: pluginTestIdentity(),
	}); err == nil {
		t.Fatal("une panne sans matière doit remonter")
	}
}

// La conclusion doit disposer de son propre temps : héritant du contexte
// épuisé de la boucle, toutes ses variantes de repli échouaient en
// « context deadline exceeded » et le travail du tour était perdu — alors
// même que le mécanisme de secours se déclenchait correctement.
func TestPluginSubAgent_ConclusionSurvivesAnExhaustedLoopDeadline(t *testing.T) {
	caller := &fakePluginCaller{result: "sortie"}

	client := &fakeCompletionClient{
		responseFunc: func(turn int, _ *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("c1", "email_read", `{"id":"1"}`)), nil
			}
			return scriptedFinalResponse("Voici ce que j'ai pu faire."), nil
		},
	}

	// Contexte quasi expiré : la boucle renonce immédiatement, et la
	// conclusion doit malgré tout aboutir.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	spec := testPluginSpec()
	spec.MaxToolCalls = 3

	result, err := agent.NewPluginSubAgent(spec, client, caller, 0, nil).
		Execute(ctx, delegation.Request{
			AgentID:  "email",
			Goal:     "Read the mail",
			Identity: pluginTestIdentity(),
		})
	if err != nil {
		t.Fatalf("la conclusion devait aboutir malgré l'échéance dépassée: %v", err)
	}
	if result.Summary == "" {
		t.Fatal("la conclusion ne doit pas être vide")
	}
}
