// Package e2e_test éprouve la chaîne complète « vidéo reçue → sous-agent de
// plugin → bac à sable LeaSH → vidéo renvoyée », avec de vrais
// sous-processus et un vrai serveur d'exécution.
//
// Le test ne s'exécute que si l'environnement fournit un serveur LeaSH :
//
//	AUTOMATA_E2E_LEASH_URL=http://127.0.0.1:18443 \
//	AUTOMATA_E2E_LEASH_KEY=<clé> \
//	AUTOMATA_E2E_FIXTURE=/chemin/exemple-whatsapp.mp4 \
//	go test ./internal/e2e/...
//
// Sans ces variables il est ignoré : la suite normale du dépôt ne doit
// dépendre ni de Docker ni du réseau.
package e2e_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/media"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/plugin"
	"github.com/bornholm/automata/internal/secretbox"
)

const e2eSessionSecret = "un-secret-de-session-de-test-32-octets"

// scriptedClient rejoue une suite d'appels d'outils décidée à l'avance :
// le but du banc est d'éprouver la plomberie, pas le modèle.
type scriptedClient struct {
	mu          sync.Mutex
	turn        int
	steps       []func(turn int) llm.ChatCompletionResponse
	toolOutputs []string
}

// commandOutputs restitue tout ce que les outils ont renvoyé au modèle
// simulé : c'est la sortie réelle du bac à sable, la seule preuve que la
// transformation a eu lieu.
func commandOutputs(c *scriptedClient) []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]string(nil), c.toolOutputs...)
}

func (c *scriptedClient) ChatCompletion(_ context.Context, funcs ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	opts := llm.NewChatCompletionOptions(funcs...)

	c.mu.Lock()
	turn := c.turn
	c.turn++
	// Les résultats d'outils reviennent dans les messages du tour suivant :
	// c'est là que le banc lit ce que le bac à sable a réellement produit.
	for _, msg := range opts.Messages {
		if msg.Role() == llm.RoleTool {
			c.toolOutputs = append(c.toolOutputs, msg.Content())
		}
	}
	c.mu.Unlock()

	if turn >= len(c.steps) {
		return scriptedText("Terminé."), nil
	}

	return c.steps[turn](turn), nil
}

func scriptedText(text string) llm.ChatCompletionResponse {
	return llm.NewChatCompletionResponse(
		llm.NewMessage(llm.RoleAssistant, text),
		llm.NewChatCompletionUsage(1, 1, 2),
	)
}

func scriptedCall(id, name, args string) llm.ChatCompletionResponse {
	return llm.NewChatCompletionResponse(
		llm.NewMessage(llm.RoleAssistant, ""),
		llm.NewChatCompletionUsage(1, 1, 2),
		llm.NewToolCall(id, name, args),
	)
}

// visionSpy tient lieu de modèle multimodal : il n'interprète rien, il
// retient ce qui lui a été réellement soumis. C'est ce qui prouve qu'une
// trame extraite dans le bac à sable ressort jusqu'au client de vision.
type visionSpy struct {
	mu          sync.Mutex
	calls       int
	attachments int
	question    string
	answer      string
}

func (v *visionSpy) ChatCompletion(_ context.Context, funcs ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	opts := llm.NewChatCompletionOptions(funcs...)

	v.mu.Lock()
	defer v.mu.Unlock()

	v.calls++
	for _, msg := range opts.Messages {
		if msg.Role() == llm.RoleUser {
			v.question = msg.Content()
			v.attachments += len(msg.Attachments())
		}
	}

	return scriptedText(v.answer), nil
}

// pluginCaller et fileTransfer relient le sous-agent au vrai gestionnaire
// de plugins — mêmes adaptateurs qu'internal/registry, recopiés ici pour
// que le banc ne dépende que de ce qu'il éprouve.
type pluginCaller struct{ manager *plugin.Manager }

func (c pluginCaller) CallPluginTool(ctx context.Context, pluginName, toolName string, callCtx agent.PluginCallContext, argsJSON string, timeoutSeconds int) (string, bool, error) {
	return c.manager.CallTool(ctx, pluginName, toolName, toPluginCtx(callCtx), argsJSON, timeoutSeconds)
}

type fileTransfer struct{ manager *plugin.Manager }

func (t fileTransfer) PutPluginFile(ctx context.Context, pluginName string, callCtx agent.PluginCallContext, filename, mimeType string, data []byte) (string, bool, string, error) {
	return t.manager.PutFile(ctx, pluginName, toPluginCtx(callCtx), filename, mimeType, data)
}

func (t fileTransfer) GetPluginFile(ctx context.Context, pluginName string, callCtx agent.PluginCallContext, path string, maxBytes int64) (string, string, []byte, error) {
	return t.manager.GetFile(ctx, pluginName, toPluginCtx(callCtx), path, maxBytes)
}

func toPluginCtx(callCtx agent.PluginCallContext) plugin.CallContext {
	return plugin.CallContext{
		OrgID:    callCtx.OrgID,
		MemberID: callCtx.MemberID,
		Scope:    callCtx.Scope,
		ScopeID:  callCtx.ScopeID,
	}
}

// TestWorkspacePlugin_VideoRoundTrip prouve le scénario de référence :
// une vidéo reçue par messagerie est importée dans le bac à sable,
// inspectée, recadrée par ffmpeg, puis renvoyée en pièce jointe.
func TestWorkspacePlugin_VideoRoundTrip(t *testing.T) {
	serverURL := os.Getenv("AUTOMATA_E2E_LEASH_URL")
	apiKey := os.Getenv("AUTOMATA_E2E_LEASH_KEY")
	fixture := os.Getenv("AUTOMATA_E2E_FIXTURE")
	if serverURL == "" || apiKey == "" || fixture == "" {
		t.Skip("banc de bout en bout non configuré (AUTOMATA_E2E_LEASH_URL / _KEY / _FIXTURE)")
	}

	video, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("lecture de la fixture: %v", err)
	}

	// Le gestionnaire transmet son propre environnement au sous-processus
	// du plugin : c'est ainsi que le plugin reçoit sa configuration.
	t.Setenv("LEASH_SERVER_URL", serverURL)
	t.Setenv("LEASH_API_KEY", apiKey)

	manager := startWorkspacePlugin(t)

	spec := agent.PluginSubAgentSpec{
		PluginName:       "workspace",
		SystemPrompt:     "You edit files in a sandbox.",
		Description:      "Edits files.",
		PermissionDomain: "workspace",
		SupportsFiles:    true,
		MaxToolCalls:     25,
		Tools: []agent.PluginToolSpec{
			{
				Name:           "run_command",
				Description:    "Run a shell script.",
				SchemaJSON:     `{"type":"object","properties":{"script":{"type":"string"}},"required":["script"]}`,
				ReadOnly:       true,
				TimeoutSeconds: 330,
			},
		},
	}

	client := &scriptedClient{steps: []func(int) llm.ChatCompletionResponse{
		func(int) llm.ChatCompletionResponse {
			return scriptedCall("c1", "import_attachment", `{"filename":"exemple-whatsapp.mp4"}`)
		},
		func(int) llm.ChatCompletionResponse {
			return scriptedCall("c2", "run_command",
				`{"script":"ffprobe -v error -show_entries stream=codec_name,width,height -of default=nw=1 exemple-whatsapp.mp4"}`)
		},
		func(int) llm.ChatCompletionResponse {
			// Extraction d'une trame : c'est ainsi qu'un agent regarde une
			// vidéo, le modèle de vision n'acceptant que des images.
			return scriptedCall("c3", "run_command",
				`{"script":"ffmpeg -y -v error -i exemple-whatsapp.mp4 -ss 1 -frames:v 1 frame.png"}`)
		},
		func(int) llm.ChatCompletionResponse {
			return scriptedCall("c4", "view_file",
				`{"path":"frame.png","question":"where is the logo?"}`)
		},
		func(int) llm.ChatCompletionResponse {
			// Recadrage réel : on retire 100 pixels en bas de l'image et on
			// ré-encode, comme le ferait un retrait de filigrane.
			return scriptedCall("c5", "run_command",
				`{"script":"ffmpeg -y -v error -i exemple-whatsapp.mp4 -vf crop=478:750:0:0 -c:v libx264 -preset veryfast -crf 28 -c:a copy sortie.mp4 && ls -l sortie.mp4"}`)
		},
		func(int) llm.ChatCompletionResponse {
			return scriptedCall("c6", "attach_file", `{"path":"sortie.mp4"}`)
		},
		func(int) llm.ChatCompletionResponse {
			return scriptedText("La vidéo recadrée est jointe.")
		},
	}}

	vision := &visionSpy{answer: "The logo is at 378, 2, 90x90 on a 478x850 image."}

	subAgent := agent.NewPluginSubAgent(spec, client, pluginCaller{manager}, 0, nil).
		WithFiles(fileTransfer{manager}, 16<<20).
		WithVision(vision)

	result, err := subAgent.Execute(context.Background(), delegation.Request{
		AgentID: "workspace",
		Goal:    "Crop the bottom of the video",
		Identity: model.ExecutionIdentity{
			Trigger:     model.TriggerMessage,
			PrincipalID: "member-e2e",
			OrgID:       "atelier-e2e",
			ChannelKind: model.ChannelPrivate,
			Scope:       model.ScopePersonal,
			ScopeID:     "member-e2e",
		},
		// La vidéo est arrivée au message PRÉCÉDENT : le membre l'envoie,
		// puis demande la transformation au message suivant. C'est le geste
		// naturel, et c'est le chemin le plus fragile — il traverse
		// l'historique, la capacité déclarée et l'import.
		RecentAttachments: []media.Media{{
			Kind:     media.KindVideo,
			MimeType: "video/mp4",
			Filename: "exemple-whatsapp.mp4",
			Data:     video,
			ToolOnly: true,
		}},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if vision.calls != 1 || vision.attachments != 1 {
		t.Fatalf("modèle de vision: %d appels, %d images soumises (attendu 1 et 1)", vision.calls, vision.attachments)
	}
	if !strings.Contains(vision.question, "where is the logo?") {
		t.Fatalf("question soumise au modèle de vision = %q", vision.question)
	}

	if len(result.Attachments) != 1 {
		t.Fatalf("pièces jointes du résultat: got %d, expected 1", len(result.Attachments))
	}

	out := result.Attachments[0]
	if out.MimeType != "video/mp4" {
		t.Fatalf("type de la pièce jointe = %q", out.MimeType)
	}
	if len(out.Data) == 0 {
		t.Fatal("la pièce jointe est vide")
	}
	if int64(len(out.Data)) >= int64(len(video)) {
		t.Fatalf("la sortie (%d octets) devrait être plus petite que l'entrée (%d octets)", len(out.Data), len(video))
	}

	// Le fichier produit doit être un MP4 lisible : la boîte « ftyp » se
	// trouve toujours aux octets 4..8 d'un conteneur ISO-BMFF.
	if len(out.Data) < 12 || string(out.Data[4:8]) != "ftyp" {
		t.Fatal("la sortie n'est pas un conteneur MP4")
	}

	produced := filepath.Join(t.TempDir(), "sortie.mp4")
	if err := os.WriteFile(produced, out.Data, 0o600); err != nil {
		t.Fatalf("écriture du fichier produit: %v", err)
	}
	t.Logf("fichier produit: %s (%d octets)", produced, len(out.Data))

	// Vérification par un ffprobe local quand il est disponible : c'est la
	// preuve demandée, et elle vaut mieux qu'un test de signature.
	ffprobe, lookErr := exec.LookPath("ffprobe")
	if lookErr != nil {
		t.Log("ffprobe absent de la machine : vérification limitée à la signature du conteneur")
		return
	}

	probe, probeErr := exec.Command(ffprobe, "-v", "error",
		"-show_entries", "stream=codec_name,width,height",
		"-of", "default=nw=1", produced).CombinedOutput()
	if probeErr != nil {
		t.Fatalf("ffprobe sur le fichier produit: %v\n%s", probeErr, probe)
	}
	t.Logf("ffprobe:\n%s", probe)

	if !strings.Contains(string(probe), "height=750") {
		t.Fatalf("le recadrage n'a pas été appliqué:\n%s", probe)
	}
}

// startWorkspacePlugin compile le vrai binaire du plugin et le fait
// charger par un vrai gestionnaire, sous-processus et gRPC compris.
func startWorkspacePlugin(t *testing.T) *plugin.Manager {
	t.Helper()

	dir := t.TempDir()
	binary := filepath.Join(dir, "workspace")

	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = filepath.Join("..", "..", "plugins", "workspace")
	build.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("compilation du plugin workspace: %v\n%s", err, out)
	}

	db, err := persistence.Open(context.Background(), config.StorageApplication{
		Driver: "sqlite3",
		Path:   filepath.Join(t.TempDir(), "app.sqlite"),
	})
	if err != nil {
		t.Fatalf("ouverture de la base: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	box, err := secretbox.NewPlugins(e2eSessionSecret)
	if err != nil {
		t.Fatalf("dérivation de la clé: %v", err)
	}

	manager := plugin.NewManager(config.Plugins{Dir: dir}, plugin.NewHostService(db, box), nil)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("démarrage du gestionnaire: %v", err)
	}
	t.Cleanup(manager.Shutdown)

	statuses := manager.Statuses()
	if len(statuses) != 1 || statuses[0].Name != "workspace" || !statuses[0].Running {
		t.Fatalf("le plugin workspace n'est pas chargé: %+v", statuses)
	}

	return manager
}

// TestWorkspacePlugin_DocumentRoundTrip prouve la chaîne bureautique : un
// document reçu par messagerie est lu, modifié, puis renvoyé dans un autre
// format — ici un .docx édité et rendu en PDF.
//
// Le document de départ est fabriqué par le bac à sable lui-même : le banc
// n'a pas besoin d'une fixture binaire, et la conversion markdown → docx
// fait déjà partie de ce qui est éprouvé.
func TestWorkspacePlugin_DocumentRoundTrip(t *testing.T) {
	serverURL := os.Getenv("AUTOMATA_E2E_LEASH_URL")
	apiKey := os.Getenv("AUTOMATA_E2E_LEASH_KEY")
	if serverURL == "" || apiKey == "" {
		t.Skip("banc de bout en bout non configuré (AUTOMATA_E2E_LEASH_URL / _KEY)")
	}

	t.Setenv("LEASH_SERVER_URL", serverURL)
	t.Setenv("LEASH_API_KEY", apiKey)

	manager := startWorkspacePlugin(t)

	spec := agent.PluginSubAgentSpec{
		PluginName:       "workspace",
		SystemPrompt:     "You edit documents in a sandbox.",
		Description:      "Edits documents.",
		PermissionDomain: "workspace",
		SupportsFiles:    true,
		MaxToolCalls:     25,
		Tools: []agent.PluginToolSpec{
			{
				Name:           "run_command",
				Description:    "Run a shell script.",
				SchemaJSON:     `{"type":"object","properties":{"script":{"type":"string"}},"required":["script"]}`,
				ReadOnly:       true,
				TimeoutSeconds: 330,
			},
		},
	}

	client := &scriptedClient{steps: []func(int) llm.ChatCompletionResponse{
		func(int) llm.ChatCompletionResponse {
			// Le document de départ, tel qu'un utilisateur l'aurait envoyé.
			return scriptedCall("d1", "run_command",
				`{"script":"printf '# Compte rendu\n\nRédigé par **Automata**.\n\n- premier point\n- second point\n' > source.md && pandoc source.md -o source.docx && ls -l source.docx"}`)
		},
		func(int) llm.ChatCompletionResponse {
			// Lecture : c'est ainsi qu'un agent prend connaissance d'un docx.
			return scriptedCall("d2", "run_command",
				`{"script":"pandoc source.docx -t markdown -o lu.md && cat lu.md"}`)
		},
		func(int) llm.ChatCompletionResponse {
			// Édition puis retour au format d'origine.
			return scriptedCall("d3", "run_command",
				`{"script":"sed 's/second point/deuxième point, corrigé/' lu.md > edite.md && pandoc edite.md -o rapport.docx && ls -l rapport.docx"}`)
		},
		func(int) llm.ChatCompletionResponse {
			// Rendu PDF par LibreOffice, le point le plus fragile de la chaîne.
			return scriptedCall("d4", "run_command",
				// head -12 et non -5 : pdftotext rend chaque puce sur sa
				// propre ligne, le texte des points arrive bien plus bas
				// qu'on ne l'attend en lisant le markdown source.
				`{"script":"office-convert pdf rapport.docx && pdftotext rapport.pdf - | head -12"}`)
		},
		func(int) llm.ChatCompletionResponse {
			return scriptedCall("d5", "attach_file", `{"path":"rapport.pdf"}`)
		},
		func(int) llm.ChatCompletionResponse {
			return scriptedText("Le rapport corrigé est joint en PDF.")
		},
	}}

	subAgent := agent.NewPluginSubAgent(spec, client, pluginCaller{manager}, 0, nil).
		WithFiles(fileTransfer{manager}, 16<<20)

	result, err := subAgent.Execute(context.Background(), delegation.Request{
		AgentID: "workspace",
		Goal:    "Fix the second bullet and send it back as a PDF",
		Identity: model.ExecutionIdentity{
			Trigger:     model.TriggerMessage,
			PrincipalID: "member-e2e-doc",
			OrgID:       "atelier-e2e",
			ChannelKind: model.ChannelPrivate,
			Scope:       model.ScopePersonal,
			ScopeID:     "member-e2e-doc",
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(result.Attachments) != 1 {
		t.Fatalf("pièces jointes du résultat: got %d, expected 1", len(result.Attachments))
	}

	out := result.Attachments[0]
	if out.MimeType != "application/pdf" {
		t.Fatalf("type de la pièce jointe = %q", out.MimeType)
	}
	// Un PDF commence toujours par « %PDF- ».
	if len(out.Data) < 5 || string(out.Data[:5]) != "%PDF-" {
		t.Fatal("la sortie n'est pas un PDF")
	}

	// La correction doit se retrouver dans le document produit : sans cette
	// vérification, un PDF vide passerait le test.
	if !strings.Contains(strings.Join(commandOutputs(client), "\n"), "deuxième point, corrigé") {
		t.Fatal("le texte corrigé n'apparaît pas dans le PDF produit")
	}

	produced := filepath.Join(t.TempDir(), "rapport.pdf")
	if err := os.WriteFile(produced, out.Data, 0o600); err != nil {
		t.Fatalf("écriture du fichier produit: %v", err)
	}
	t.Logf("fichier produit: %s (%d octets)", produced, len(out.Data))
}
