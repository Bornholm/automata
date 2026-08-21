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
	if r.Header.Get("Authorization") != "Bearer test-key" {
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
