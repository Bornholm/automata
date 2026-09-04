package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// fakeLeash rejoue les réponses d'un serveur LeaSH : MCP Streamable HTTP
// sur "/", endpoints fichiers sur "/files/".
type fakeLeash struct {
	mu sync.Mutex

	// workspaces enregistre le header X-Workspace reçu sur chaque requête.
	workspaces []string
	// files est le contenu déposé, par chemin.
	files map[string][]byte
	// script mémorise le dernier script reçu par execute_shell.
	script string
	// authorization mémorise la clé présentée sur la dernière requête : la
	// séparation des clés est ce qui sépare l'atelier étanche du sandbox
	// réseau, elle mérite d'être vérifiée et pas seulement relue.
	authorization string

	// execResult est le texte renvoyé par execute_shell.
	execResult string
	// execIsError marque le résultat comme un échec métier.
	execIsError bool
	// putStatus et putBody, s'ils sont renseignés, remplacent la réponse
	// nominale d'un PUT.
	putStatus int
	putBody   string
}

func newFakeLeash() *fakeLeash {
	return &fakeLeash{files: map[string][]byte{}, execResult: "## EXIT CODE\n0\n"}
}

func (f *fakeLeash) record(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.workspaces = append(f.workspaces, r.Header.Get("X-Workspace"))
}

func (f *fakeLeash) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.authorization = r.Header.Get("Authorization")
	f.mu.Unlock()

	if r.Header.Get("Authorization") != "Bearer test-key" && r.Header.Get("Authorization") != "Bearer fetch-key" {
		http.Error(w, "invalid API key", http.StatusUnauthorized)
		return
	}
	f.record(r)

	if strings.HasPrefix(r.URL.Path, "/files/") {
		f.serveFiles(w, r)
		return
	}

	f.serveMCP(w, r)
}

func (f *fakeLeash) serveFiles(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/files/")

	switch {
	case r.Method == http.MethodPut:
		if f.putStatus != 0 {
			http.Error(w, f.putBody, f.putStatus)
			return
		}
		data, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.files[rel] = data
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(FileEntry{Path: rel, Size: int64(len(data))})

	case r.Method == http.MethodGet && rel == "":
		f.mu.Lock()
		listing := struct {
			Files []FileEntry `json:"files"`
			Total int64       `json:"total_bytes"`
		}{}
		for path, data := range f.files {
			listing.Files = append(listing.Files, FileEntry{Path: path, Size: int64(len(data))})
			listing.Total += int64(len(data))
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(listing)

	case r.Method == http.MethodGet:
		f.mu.Lock()
		data, ok := f.files[rel]
		f.mu.Unlock()
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write(data)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// serveMCP répond au strict minimum du protocole : initialize, la
// notification initialized, puis tools/call.
func (f *fakeLeash) serveMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string `json:"name"`
			Arguments struct {
				Script string `json:"script"`
			} `json:"arguments"`
		} `json:"params"`
	}
	body, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	switch req.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", "test-session")
		writeJSONRPC(w, req.ID, map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "fake-leash", "version": "1.0.0"},
		})

	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)

	case "tools/call":
		f.mu.Lock()
		f.script = req.Params.Arguments.Script
		result, isError := f.execResult, f.execIsError
		f.mu.Unlock()
		writeJSONRPC(w, req.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": result}},
			"isError": isError,
		})

	default:
		w.WriteHeader(http.StatusAccepted)
	}
}

func writeJSONRPC(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"result":  result,
	})
}

func newTestPlugin(t *testing.T) (*Plugin, *fakeLeash) {
	t.Helper()
	fake := newFakeLeash()
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)

	return newPlugin(&LeashClient{
		baseURL: ts.URL,
		apiKey:  "test-key",
		http:    ts.Client(),
	}), fake
}

func testCallContext(memberID string) *proto.CallContext {
	return &proto.CallContext{OrgId: "atelier", MemberId: memberID, Scope: "personal", ScopeId: memberID}
}

func TestDescribe_DeclaresFileSupportAndSubAgent(t *testing.T) {
	p, _ := newTestPlugin(t)

	desc, err := p.Describe(context.Background(), &proto.DescribeRequest{})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if desc.Name != "workspace" || desc.PermissionDomain != "workspace" {
		t.Fatalf("descripteur = %+v", desc)
	}
	if !desc.SupportsFiles {
		t.Fatal("le plugin doit déclarer supports_files")
	}
	if desc.SubAgent == nil || desc.SubAgent.Description == "" || desc.SubAgent.SystemPrompt == "" {
		t.Fatal("le sous-agent doit être décrit")
	}
}

func TestListTools_ExecutionToolsAreReadOnlyOnPurpose(t *testing.T) {
	p, _ := newTestPlugin(t)

	out, err := p.ListTools(context.Background(), &proto.ListToolsInput{Ctx: testCallContext("member-1")})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(out.Tools) != 2 {
		t.Fatalf("outils = %d, attendu 2", len(out.Tools))
	}
	for _, tool := range out.Tools {
		if !tool.ReadOnly {
			t.Fatalf("l'outil %q devrait être read_only (le sandbox est la frontière de sécurité)", tool.Name)
		}
	}
	for _, tool := range out.Tools {
		if tool.Name == "run_command" && tool.TimeoutSeconds <= 300 {
			t.Fatalf("run_command doit dépasser le max_duration de LeaSH, got %d", tool.TimeoutSeconds)
		}
	}
}

func TestRunCommand_RoundTrip(t *testing.T) {
	p, fake := newTestPlugin(t)
	fake.execResult = "## STDOUT\ncodec_name=h264\n\n## EXIT CODE\n0\n"

	out, err := p.CallTool(context.Background(), &proto.CallToolInput{
		Ctx:           testCallContext("member-1"),
		Name:          "run_command",
		ArgumentsJson: `{"script":"ffprobe input.mp4"}`,
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if out.IsError {
		t.Fatalf("résultat en erreur: %s", out.ResultText)
	}
	if !strings.Contains(out.ResultText, "codec_name=h264") {
		t.Fatalf("résultat = %q", out.ResultText)
	}
	if fake.script != "ffprobe input.mp4" {
		t.Fatalf("script reçu = %q", fake.script)
	}
}

func TestRunCommand_MissingScriptIsABusinessError(t *testing.T) {
	p, _ := newTestPlugin(t)

	out, err := p.CallTool(context.Background(), &proto.CallToolInput{
		Ctx:           testCallContext("member-1"),
		Name:          "run_command",
		ArgumentsJson: `{}`,
	})
	if err != nil {
		t.Fatalf("un paramètre manquant ne doit jamais être une erreur Go: %v", err)
	}
	if !out.IsError {
		t.Fatal("le résultat devrait être marqué en erreur")
	}
}

func TestRunCommand_LeashBusinessErrorIsRelayed(t *testing.T) {
	p, fake := newTestPlugin(t)
	fake.execResult = "## BLOCKED\n- `curl`: command not in allowlist\n"
	fake.execIsError = true

	out, err := p.CallTool(context.Background(), &proto.CallToolInput{
		Ctx:           testCallContext("member-1"),
		Name:          "run_command",
		ArgumentsJson: `{"script":"curl https://example.com"}`,
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !out.IsError || !strings.Contains(out.ResultText, "not in allowlist") {
		t.Fatalf("résultat = %+v", out)
	}
}

func TestWorkspaceHeaderIsDistinctPerMember(t *testing.T) {
	p, fake := newTestPlugin(t)

	for _, member := range []string{"member-1", "member-2"} {
		if _, err := p.CallTool(context.Background(), &proto.CallToolInput{
			Ctx:           testCallContext(member),
			Name:          "run_command",
			ArgumentsJson: `{"script":"ls"}`,
		}); err != nil {
			t.Fatalf("CallTool(%s): %v", member, err)
		}
	}

	seen := map[string]bool{}
	for _, ws := range fake.workspaces {
		seen[ws] = true
	}
	if !seen["atelier/member-1"] || !seen["atelier/member-2"] {
		t.Fatalf("discriminants vus = %v", fake.workspaces)
	}
}

// fakePutStream rejoue le flux gRPC entrant de PutFile.
type fakePutStream struct {
	grpc.ServerStream
	chunks []*proto.PutFileChunk
	next   int
	result *proto.PutFileResult
}

func (s *fakePutStream) Context() context.Context { return context.Background() }
func (s *fakePutStream) SetHeader(metadata.MD) error {
	return nil
}
func (s *fakePutStream) Recv() (*proto.PutFileChunk, error) {
	if s.next >= len(s.chunks) {
		return nil, io.EOF
	}
	chunk := s.chunks[s.next]
	s.next++
	return chunk, nil
}
func (s *fakePutStream) SendAndClose(result *proto.PutFileResult) error {
	s.result = result
	return nil
}

// fakeGetStream collecte le flux gRPC sortant de GetFile.
type fakeGetStream struct {
	grpc.ServerStream
	meta *proto.FileMetadata
	data []byte
}

func (s *fakeGetStream) Context() context.Context { return context.Background() }
func (s *fakeGetStream) Send(chunk *proto.FileChunk) error {
	switch payload := chunk.Payload.(type) {
	case *proto.FileChunk_Metadata:
		s.meta = payload.Metadata
	case *proto.FileChunk_Data:
		s.data = append(s.data, payload.Data...)
	}
	return nil
}

func TestPutFileThenGetFile(t *testing.T) {
	p, fake := newTestPlugin(t)
	payload := []byte("octets de la video envoyee par le membre")

	put := &fakePutStream{chunks: []*proto.PutFileChunk{
		{Payload: &proto.PutFileChunk_Metadata{Metadata: &proto.PutFileMetadata{
			Ctx:      testCallContext("member-1"),
			Filename: "clip.mp4",
			MimeType: "video/mp4",
			Size:     uint64(len(payload)),
		}}},
		{Payload: &proto.PutFileChunk_Data{Data: payload}},
	}}

	if err := p.PutFile(put); err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if put.result == nil || put.result.IsError || put.result.Path != "clip.mp4" {
		t.Fatalf("résultat PutFile = %+v", put.result)
	}
	if string(fake.files["clip.mp4"]) != string(payload) {
		t.Fatal("les octets déposés ne correspondent pas")
	}

	get := &fakeGetStream{}
	if err := p.GetFile(&proto.GetFileRequest{Ctx: testCallContext("member-1"), Path: "clip.mp4"}, get); err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if get.meta == nil || get.meta.Filename != "clip.mp4" || get.meta.MimeType != "video/mp4" {
		t.Fatalf("métadonnées = %+v", get.meta)
	}
	if string(get.data) != string(payload) {
		t.Fatal("les octets récupérés ne correspondent pas")
	}
}

func TestPutFile_LeashErrorCarriesTheResponseBody(t *testing.T) {
	p, fake := newTestPlugin(t)
	fake.putStatus = http.StatusInsufficientStorage
	fake.putBody = "workspace quota exceeded"

	put := &fakePutStream{chunks: []*proto.PutFileChunk{
		{Payload: &proto.PutFileChunk_Metadata{Metadata: &proto.PutFileMetadata{
			Ctx:      testCallContext("member-1"),
			Filename: "clip.mp4",
			MimeType: "video/mp4",
		}}},
		{Payload: &proto.PutFileChunk_Data{Data: []byte("data")}},
	}}

	if err := p.PutFile(put); err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if put.result == nil || !put.result.IsError {
		t.Fatalf("résultat = %+v", put.result)
	}
	// Un code nu n'apprend rien : le corps de LeaSH doit remonter.
	if !strings.Contains(put.result.ErrorText, "workspace quota exceeded") {
		t.Fatalf("erreur remontée = %q", put.result.ErrorText)
	}
}

func TestListFilesTool(t *testing.T) {
	p, fake := newTestPlugin(t)
	fake.files["sortie.mp4"] = []byte("1234567890")

	out, err := p.CallTool(context.Background(), &proto.CallToolInput{
		Ctx:  testCallContext("member-1"),
		Name: "list_files",
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !strings.Contains(out.ResultText, "sortie.mp4") || !strings.Contains(out.ResultText, "10 bytes") {
		t.Fatalf("résultat = %q", out.ResultText)
	}
}

func TestSafeFilename(t *testing.T) {
	cases := map[string]string{
		"clip.mp4":               "clip.mp4",
		"../../etc/passwd":       "passwd",
		"/absolu/video.mp4":      "video.mp4",
		`C:\Users\bob\video.mp4`: "video.mp4",
		"..":                     "input.mp4",
		"":                       "input.mp4",
	}

	for input, want := range cases {
		if got := safeFilename(input, "video/mp4"); got != want {
			t.Fatalf("safeFilename(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestCallTool_WithoutIdentityIsRefused(t *testing.T) {
	p, _ := newTestPlugin(t)

	out, err := p.CallTool(context.Background(), &proto.CallToolInput{
		Name:          "run_command",
		ArgumentsJson: `{"script":"ls"}`,
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !out.IsError {
		t.Fatal("un appel sans identité doit être refusé")
	}
}

// Les outils de téléchargement n'existent QUE si l'exploitant a configuré
// la clé de la policy réseau. Un outil monté sans elle ferait perdre au
// modèle un appel et une explication.
func TestListTools_FetchToolsFollowTheOperatorKey(t *testing.T) {
	closed := newPlugin(&LeashClient{baseURL: "http://leash", apiKey: "k"})
	out, err := closed.ListTools(context.Background(), &proto.ListToolsInput{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range out.Tools {
		if _, ok := lookupFetchCapability(tool.Name); ok {
			t.Errorf("%s est monté sans clé de policy réseau", tool.Name)
		}
	}

	open := newPlugin(&LeashClient{baseURL: "http://leash", apiKey: "k", fetchAPIKey: "f"})
	out, err = open.ListTools(context.Background(), &proto.ListToolsInput{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	mounted := map[string]*proto.ToolDescriptor{}
	for _, tool := range out.Tools {
		mounted[tool.Name] = tool
	}
	for _, capability := range fetchCapabilities {
		tool, ok := mounted[capability.Tool]
		if !ok {
			t.Fatalf("%s n'est pas monté alors que la clé est là", capability.Tool)
		}
		// La liste des domaines est reprise dans chaque description : le
		// modèle choisit son outil sur cette seule ligne.
		if !strings.Contains(tool.Description, "youtube.com") {
			t.Errorf("%s ne dit pas quels sites sont autorisés: %s", capability.Tool, tool.Description)
		}
		// Le plafond doit rester au-dessus du max_duration de la policy
		// « fetch » (600 s), sinon l'hôte coupe avant le verdict de LeaSH.
		if tool.TimeoutSeconds <= 600 {
			t.Errorf("%s expire en %d s, avant la policy réseau", capability.Tool, tool.TimeoutSeconds)
		}
	}
}

// Le schéma est construit, pas recopié : il doit porter url, name et les
// paramètres propres à la capacité, avec leur défaut visible. Un modèle
// qui ne lit pas le défaut dans le schéma invente une valeur.
func TestFetchSchema_CarriesEveryParameter(t *testing.T) {
	for _, capability := range fetchCapabilities {
		var schema struct {
			Properties map[string]struct {
				Type        string `json:"type"`
				Description string `json:"description"`
			} `json:"properties"`
			Required []string `json:"required"`
		}
		if err := json.Unmarshal([]byte(fetchSchema(capability)), &schema); err != nil {
			t.Fatalf("%s: schéma illisible: %v", capability.Tool, err)
		}

		for _, name := range []string{"url", "name"} {
			if _, ok := schema.Properties[name]; !ok {
				t.Errorf("%s: le schéma ne déclare pas %q", capability.Tool, name)
			}
		}
		if len(schema.Required) != 1 || schema.Required[0] != "url" {
			t.Errorf("%s: required = %v, attendu [url]", capability.Tool, schema.Required)
		}
		for _, param := range capability.Params {
			property, ok := schema.Properties[param.Name]
			if !ok {
				t.Fatalf("%s: le schéma ne déclare pas %q", capability.Tool, param.Name)
			}
			if param.Default != "" && !strings.Contains(property.Description, param.Default) {
				t.Errorf("%s: le schéma tait le défaut de %q: %s", capability.Tool, param.Name, property.Description)
			}
		}
	}
}

// Un modèle nomme mal ses paramètres : le chemin générique doit accepter
// les synonymes observés plutôt que de rendre une erreur que le modèle
// interprète comme une panne de l'outil.
func TestStringArg_AcceptsTheSynonyms(t *testing.T) {
	args := map[string]any{"video_url": "https://youtu.be/x", "output": "clip", "count": 3}

	if got := stringArg(args, "url", "video_url", "link"); got != "https://youtu.be/x" {
		t.Errorf("url = %q", got)
	}
	if got := stringArg(args, "name", "output"); got != "clip" {
		t.Errorf("name = %q", got)
	}
	// Une valeur d'un autre type ne doit pas faire échouer l'appel sur une
	// erreur de désérialisation que le modèle ne saurait pas corriger.
	if got := stringArg(args, "count"); got != "" {
		t.Errorf("count = %q, chaîne vide attendue", got)
	}
	if got := stringArg(args, "absent"); got != "" {
		t.Errorf("absent = %q", got)
	}
}

// Le contrat entre le Go et les scripts de misc/toolbox est POSITIONNEL :
// l'URL, le nom de sortie, puis les paramètres de la capacité dans l'ordre
// du tableau. Rien ne le vérifie à l'exécution — un décalage se lirait
// comme une vidéo téléchargée dans la mauvaise langue, ou pire.
func TestFetch_CallsTheScriptWithItsPositionalArguments(t *testing.T) {
	p, fake := newTestPlugin(t)
	p.leash.fetchAPIKey = "fetch-key"
	fake.execResult = "## STDOUT\nfetch-subtitles: subtitles written to talk.vtt (language: fr)\n\n## EXIT CODE\n0\n"

	out, err := p.CallTool(context.Background(), &proto.CallToolInput{
		Ctx:           testCallContext("member-1"),
		Name:          "download_subtitles",
		ArgumentsJson: `{"url":"https://www.youtube.com/watch?v=abc","name":"talk","lang":"fr"}`,
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if out.IsError {
		t.Fatalf("résultat en erreur: %s", out.ResultText)
	}
	if fake.script != `fetch-subtitles 'https://www.youtube.com/watch?v=abc' 'talk' 'fr'` {
		t.Fatalf("script reçu = %q", fake.script)
	}
	// La clé de la policy réseau, jamais celle de l'atelier : c'est elle
	// qui décide de l'ouverture du réseau côté LeaSH.
	if fake.authorization != "Bearer fetch-key" {
		t.Errorf("clé présentée = %q", fake.authorization)
	}
	if !strings.Contains(out.ResultText, "summarize-video-from-subtitles") {
		t.Errorf("le succès n'oriente pas vers la compétence: %q", out.ResultText)
	}
}

// Le défaut du paramètre doit atteindre le script : le laisser vide
// donnerait « fetch-subtitles url name ” », que le script prendrait pour
// une liste de langues vide.
func TestFetch_OmittedParameterFallsBackToItsDefault(t *testing.T) {
	p, fake := newTestPlugin(t)
	p.leash.fetchAPIKey = "fetch-key"

	if _, err := p.CallTool(context.Background(), &proto.CallToolInput{
		Ctx:           testCallContext("member-1"),
		Name:          "download_subtitles",
		ArgumentsJson: `{"url":"https://youtu.be/abc"}`,
	}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if fake.script != `fetch-subtitles 'https://youtu.be/abc' 'video' 'fr,en'` {
		t.Fatalf("script reçu = %q", fake.script)
	}
}

// Une URL hors liste blanche ne doit jamais atteindre LeaSH : le refus est
// prononcé côté plugin, avant l'ouverture du réseau.
func TestFetch_RefusedURLNeverReachesTheSandbox(t *testing.T) {
	p, fake := newTestPlugin(t)
	p.leash.fetchAPIKey = "fetch-key"

	out, err := p.CallTool(context.Background(), &proto.CallToolInput{
		Ctx:           testCallContext("member-1"),
		Name:          "download_subtitles",
		ArgumentsJson: `{"url":"https://192.168.30.10/interne"}`,
	})
	if err != nil {
		t.Fatalf("un refus ne doit jamais être une erreur Go: %v", err)
	}
	if !out.IsError {
		t.Fatal("le refus devrait être marqué en erreur")
	}
	if fake.script != "" {
		t.Fatalf("le sandbox a été sollicité: %q", fake.script)
	}
}
