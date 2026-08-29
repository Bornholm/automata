package main

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/bornholm/automata/pkg/pluginsdk"
	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// memoryHost implémente le magasin d'objets en mémoire, avec la même
// sémantique que l'hôte (CopyCollection remplace, DeleteCollection emporte
// la publication, slug stable).
type memoryHost struct {
	pluginsdk.UnimplementedHostClient

	objects  map[string][]byte // "collection|key" → data
	types    map[string]string
	sites    map[string]string // collection → slug
	nextSlug int
}

func newMemoryHost() *memoryHost {
	return &memoryHost{objects: map[string][]byte{}, types: map[string]string{}, sites: map[string]string{}}
}

func objectKey(collection, key string) string { return collection + "|" + key }

func (h *memoryHost) PutObject(_ context.Context, _, _, collection, key, contentType string, data []byte) error {
	h.objects[objectKey(collection, key)] = append([]byte(nil), data...)
	h.types[objectKey(collection, key)] = contentType
	return nil
}

func (h *memoryHost) GetObject(_ context.Context, _, _, collection, key string) ([]byte, string, bool, error) {
	data, found := h.objects[objectKey(collection, key)]
	return data, h.types[objectKey(collection, key)], found, nil
}

func (h *memoryHost) DeleteObject(_ context.Context, _, _, collection, key string) (bool, error) {
	_, found := h.objects[objectKey(collection, key)]
	delete(h.objects, objectKey(collection, key))
	return found, nil
}

func (h *memoryHost) DeleteCollection(_ context.Context, _, _, collection string) (int64, error) {
	var deleted int64
	for full := range h.objects {
		if strings.HasPrefix(full, collection+"|") {
			delete(h.objects, full)
			deleted++
		}
	}
	delete(h.sites, collection)
	return deleted, nil
}

func (h *memoryHost) ListObjects(_ context.Context, _, _, collection string) ([]pluginsdk.ObjectEntry, error) {
	var entries []pluginsdk.ObjectEntry
	for full, data := range h.objects {
		if key, ok := strings.CutPrefix(full, collection+"|"); ok {
			entries = append(entries, pluginsdk.ObjectEntry{Key: key, Size: int64(len(data))})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	return entries, nil
}

func (h *memoryHost) ListCollections(_ context.Context, _, _, prefix string) ([]string, error) {
	seen := map[string]struct{}{}
	for full := range h.objects {
		collection, _, _ := strings.Cut(full, "|")
		if strings.HasPrefix(collection, prefix) {
			seen[collection] = struct{}{}
		}
	}
	var collections []string
	for collection := range seen {
		collections = append(collections, collection)
	}
	sort.Strings(collections)
	return collections, nil
}

func (h *memoryHost) CopyCollection(ctx context.Context, orgID, memberID, from, to string) (int64, error) {
	if _, err := h.DeleteCollection(ctx, orgID, memberID, to); err != nil {
		return 0, err
	}
	var copied int64
	for full, data := range h.objects {
		if key, ok := strings.CutPrefix(full, from+"|"); ok {
			h.objects[objectKey(to, key)] = data
			h.types[objectKey(to, key)] = h.types[full]
			copied++
		}
	}
	return copied, nil
}

func (h *memoryHost) PublishCollection(_ context.Context, _, _, collection string) (string, string, error) {
	if slug, found := h.sites[collection]; found {
		return slug, "https://automata.test/s/" + slug + "/", nil
	}
	h.nextSlug++
	slug := fmt.Sprintf("slug%06d", h.nextSlug)
	h.sites[collection] = slug
	return slug, "https://automata.test/s/" + slug + "/", nil
}

func (h *memoryHost) UnpublishCollection(_ context.Context, _, _, collection string) (bool, error) {
	_, found := h.sites[collection]
	delete(h.sites, collection)
	return found, nil
}

func (h *memoryHost) ListPublications(_ context.Context, _, _ string) ([]pluginsdk.Publication, error) {
	var publications []pluginsdk.Publication
	for collection, slug := range h.sites {
		publications = append(publications, pluginsdk.Publication{
			Collection: collection, Slug: slug, URL: "https://automata.test/s/" + slug + "/",
		})
	}
	return publications, nil
}

func newTestPlugin() (*Plugin, *memoryHost) {
	plugin := newPlugin()
	host := newMemoryHost()
	plugin.SetHostClient(host)
	return plugin, host
}

func call(t *testing.T, plugin *Plugin, tool, argsJSON, idempotencyKey string) *proto.CallToolOutput {
	t.Helper()

	out, err := plugin.CallTool(context.Background(), &proto.CallToolInput{
		Ctx:           &proto.CallContext{OrgId: "atelier", MemberId: "cam", IdempotencyKey: idempotencyKey},
		Name:          tool,
		ArgumentsJson: argsJSON,
	})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", tool, err)
	}
	return out
}

// Le parcours nominal : créer, écrire (avec append), publier, vérifier que
// les éditions suivantes restent privées jusqu'à la republication.
func TestPagesLifecycle(t *testing.T) {
	plugin, host := newTestPlugin()

	if out := call(t, plugin, "create_space", `{"name":"invitation"}`, ""); out.IsError {
		t.Fatalf("create_space: %s", out.ResultText)
	}
	// Le nom est pris.
	if out := call(t, plugin, "create_space", `{"name":"invitation"}`, ""); !out.IsError {
		t.Error("recréer un espace existant doit échouer")
	}

	call(t, plugin, "write_file", `{"space":"invitation","path":"index.html","content":"<html>partie 1"}`, "")
	if out := call(t, plugin, "write_file", `{"space":"invitation","path":"index.html","content":" partie 2</html>","append":true}`, ""); out.IsError {
		t.Fatalf("append: %s", out.ResultText)
	}

	if out := call(t, plugin, "read_file", `{"space":"invitation","path":"index.html"}`, ""); out.ResultText != "<html>partie 1 partie 2</html>" {
		t.Errorf("read_file = %q", out.ResultText)
	}

	out := call(t, plugin, "publish_space", `{"space":"invitation"}`, "action-1")
	if out.IsError || !strings.Contains(out.ResultText, "https://automata.test/s/") {
		t.Fatalf("publish_space: %s", out.ResultText)
	}

	// Une confirmation rejouée ne republie pas.
	if out := call(t, plugin, "publish_space", `{"space":"invitation"}`, "action-1"); !strings.Contains(out.ResultText, "already") {
		t.Errorf("rejeu = %q", out.ResultText)
	}

	// Modifier après publication ne touche que le brouillon.
	call(t, plugin, "write_file", `{"space":"invitation","path":"index.html","content":"<html>v2</html>"}`, "")
	if live := host.objects[objectKey("spaces/invitation/live", "index.html")]; string(live) != "<html>partie 1 partie 2</html>" {
		t.Errorf("la version publiée a changé sans republication: %q", live)
	}

	// list_spaces annonce l'URL publique.
	if out := call(t, plugin, "list_spaces", "", ""); !strings.Contains(out.ResultText, "published: https://automata.test/s/") {
		t.Errorf("list_spaces = %q", out.ResultText)
	}
}

// La dépublication tue le lien et garde le brouillon ; la suppression
// emporte tout.
func TestPagesUnpublishAndDelete(t *testing.T) {
	plugin, host := newTestPlugin()

	call(t, plugin, "create_space", `{"name":"demo"}`, "")
	call(t, plugin, "publish_space", `{"space":"demo"}`, "a1")

	if out := call(t, plugin, "unpublish_space", `{"space":"demo"}`, "a2"); out.IsError {
		t.Fatalf("unpublish_space: %s", out.ResultText)
	}
	if len(host.sites) != 0 {
		t.Error("le lien public doit être mort")
	}
	if _, found := host.objects[objectKey("spaces/demo/draft", "index.html")]; !found {
		t.Error("le brouillon doit survivre à la dépublication")
	}

	if out := call(t, plugin, "delete_space", `{"space":"demo"}`, "a3"); out.IsError {
		t.Fatalf("delete_space: %s", out.ResultText)
	}
	if len(host.objects) != 0 {
		t.Errorf("des objets survivent à la suppression: %v", host.objects)
	}
}

// Les gardes : chemins et noms invalides, extensions non textuelles,
// espace inexistant, publication sans index.html.
func TestPagesGuards(t *testing.T) {
	plugin, _ := newTestPlugin()

	if out := call(t, plugin, "create_space", `{"name":"Majuscule"}`, ""); !out.IsError {
		t.Error("un nom en majuscules doit être refusé")
	}
	if out := call(t, plugin, "write_file", `{"space":"absent","path":"index.html","content":"x"}`, ""); !out.IsError {
		t.Error("écrire dans un espace inexistant doit échouer")
	}

	call(t, plugin, "create_space", `{"name":"demo"}`, "")
	for _, path := range []string{"../evil.html", "/abs.html", "photo.jpg", "sans-extension"} {
		if out := call(t, plugin, "write_file", fmt.Sprintf(`{"space":"demo","path":%q,"content":"x"}`, path), ""); !out.IsError {
			t.Errorf("write_file(%q) doit être refusé", path)
		}
	}

	// Publier sans index.html : l'espace « vide » n'existe que par un
	// autre fichier.
	call(t, plugin, "write_file", `{"space":"demo","path":"style.css","content":"body{}"}`, "")
	call(t, plugin, "delete_file", `{"space":"demo","path":"index.html"}`, "")
	if out := call(t, plugin, "publish_space", `{"space":"demo"}`, "a1"); !out.IsError {
		t.Error("publier sans index.html doit échouer")
	}
}

// La limite d'espaces protège le quota du membre.
func TestPagesSpaceLimit(t *testing.T) {
	plugin, _ := newTestPlugin()

	for i := range maxSpaces {
		if out := call(t, plugin, "create_space", fmt.Sprintf(`{"name":"espace-%d"}`, i), ""); out.IsError {
			t.Fatalf("create_space %d: %s", i, out.ResultText)
		}
	}
	if out := call(t, plugin, "create_space", `{"name":"de-trop"}`, ""); !out.IsError {
		t.Error("l'espace au-delà de la limite doit être refusé")
	}
}

// fakePutFileStream simule l'hôte qui pousse une pièce jointe.
type fakePutFileStream struct {
	proto.AutomataPlugin_PutFileServer
	chunks []*proto.PutFileChunk
	result *proto.PutFileResult
}

func (s *fakePutFileStream) Context() context.Context { return context.Background() }

func (s *fakePutFileStream) Recv() (*proto.PutFileChunk, error) {
	if len(s.chunks) == 0 {
		return nil, io.EOF
	}
	chunk := s.chunks[0]
	s.chunks = s.chunks[1:]
	return chunk, nil
}

func (s *fakePutFileStream) SendAndClose(result *proto.PutFileResult) error {
	s.result = result
	return nil
}

// fakeGetFileStream collecte ce que le plugin renvoie à l'hôte.
type fakeGetFileStream struct {
	proto.AutomataPlugin_GetFileServer
	chunks []*proto.FileChunk
}

func (s *fakeGetFileStream) Context() context.Context { return context.Background() }

func (s *fakeGetFileStream) Send(chunk *proto.FileChunk) error {
	s.chunks = append(s.chunks, chunk)
	return nil
}

// Une pièce jointe importée atterrit dans « imports » sous un nom assaini,
// puis use_file la place dans l'espace.
func TestPagesImportAndUseFile(t *testing.T) {
	plugin, host := newTestPlugin()
	call(t, plugin, "create_space", `{"name":"demo"}`, "")

	stream := &fakePutFileStream{chunks: []*proto.PutFileChunk{
		{Payload: &proto.PutFileChunk_Metadata{Metadata: &proto.PutFileMetadata{
			Ctx:      &proto.CallContext{OrgId: "atelier", MemberId: "cam"},
			Filename: "Ma Photo (1).JPG",
			MimeType: "image/jpeg",
		}}},
		{Payload: &proto.PutFileChunk_Data{Data: []byte("jpegdata")}},
	}}
	if err := plugin.PutFile(stream); err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if stream.result == nil || stream.result.IsError {
		t.Fatalf("PutFile result = %+v", stream.result)
	}
	if stream.result.Path != "imports/ma-photo-1-.jpg" {
		t.Errorf("path = %q", stream.result.Path)
	}

	out := call(t, plugin, "use_file", fmt.Sprintf(`{"space":"demo","import_path":%q,"target_path":"photos/une.jpg"}`, stream.result.Path), "")
	if out.IsError {
		t.Fatalf("use_file: %s", out.ResultText)
	}
	if data := host.objects[objectKey("spaces/demo/draft", "photos/une.jpg")]; string(data) != "jpegdata" {
		t.Errorf("fichier placé = %q", data)
	}
}

// GetFile sert un fichier du brouillon et l'archive zip de l'espace.
func TestPagesGetFile(t *testing.T) {
	plugin, _ := newTestPlugin()
	call(t, plugin, "create_space", `{"name":"demo"}`, "")
	call(t, plugin, "write_file", `{"space":"demo","path":"style.css","content":"body{margin:0}"}`, "")

	stream := &fakeGetFileStream{}
	if err := plugin.GetFile(&proto.GetFileRequest{
		Ctx:  &proto.CallContext{OrgId: "atelier", MemberId: "cam"},
		Path: "demo/style.css",
	}, stream); err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	meta := stream.chunks[0].GetMetadata()
	if meta.Filename != "style.css" || meta.MimeType != "text/css" {
		t.Errorf("meta = %+v", meta)
	}

	zipStream := &fakeGetFileStream{}
	if err := plugin.GetFile(&proto.GetFileRequest{
		Ctx:  &proto.CallContext{OrgId: "atelier", MemberId: "cam"},
		Path: "demo.zip",
	}, zipStream); err != nil {
		t.Fatalf("GetFile zip: %v", err)
	}
	if meta := zipStream.chunks[0].GetMetadata(); meta.Filename != "demo.zip" || meta.MimeType != "application/zip" {
		t.Errorf("meta zip = %+v", meta)
	}

	var payload []byte
	for _, chunk := range zipStream.chunks[1:] {
		payload = append(payload, chunk.GetData()...)
	}
	reader, err := zip.NewReader(bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("zip illisible: %v", err)
	}
	names := make([]string, 0, len(reader.File))
	for _, file := range reader.File {
		names = append(names, file.Name)
	}
	sort.Strings(names)
	if len(names) != 2 || names[0] != "demo/index.html" || names[1] != "demo/style.css" {
		t.Errorf("archive = %v", names)
	}
}
