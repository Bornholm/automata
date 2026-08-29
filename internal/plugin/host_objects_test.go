package plugin

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/secretbox"
	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// newObjectHost monte un service hôte avec magasin d'objets sur une base
// réelle, sans manager ni binaire de plugin : les RPC du magasin se
// testent directement sur la vue scopée.
func newObjectHost(t *testing.T, limits ObjectStoreLimits) (*scopedHostService, *persistence.DB) {
	t.Helper()

	db, err := persistence.Open(context.Background(), config.StorageApplication{
		Driver: "sqlite3",
		Path:   filepath.Join(t.TempDir(), "app.sqlite"),
	})
	if err != nil {
		t.Fatalf("ouverture de la base: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	box, err := secretbox.NewPlugins(testSessionSecret)
	if err != nil {
		t.Fatalf("dérivation de la clé: %v", err)
	}

	host := NewHostService(db, box).WithObjectStore("https://automata.test/", limits)
	seedOrgAndMember(t, db, "atelier", "cam")

	return host.scopedTo("pages"), db
}

// fakePutStream rejoue une suite de tranches PutObject.
type fakePutStream struct {
	grpc.ServerStream
	ctx    context.Context
	chunks []*proto.PutObjectChunk
	result *proto.PutObjectResult
}

func (s *fakePutStream) Context() context.Context { return s.ctx }

func (s *fakePutStream) Recv() (*proto.PutObjectChunk, error) {
	if len(s.chunks) == 0 {
		return nil, io.EOF
	}
	chunk := s.chunks[0]
	s.chunks = s.chunks[1:]
	return chunk, nil
}

func (s *fakePutStream) SendAndClose(result *proto.PutObjectResult) error {
	s.result = result
	return nil
}

// fakeGetStream collecte les tranches GetObject.
type fakeGetStream struct {
	grpc.ServerStream
	ctx    context.Context
	chunks []*proto.GetObjectChunk
}

func (s *fakeGetStream) Context() context.Context { return s.ctx }

func (s *fakeGetStream) Send(chunk *proto.GetObjectChunk) error {
	s.chunks = append(s.chunks, chunk)
	return nil
}

func putTestObject(t *testing.T, scoped *scopedHostService, memberID, collection, key, content string) error {
	t.Helper()

	stream := &fakePutStream{ctx: context.Background(), chunks: []*proto.PutObjectChunk{
		{Payload: &proto.PutObjectChunk_Metadata{Metadata: &proto.PutObjectMetadata{
			OrgId: "atelier", MemberId: memberID, Collection: collection, Key: key, ContentType: "text/html",
		}}},
		{Payload: &proto.PutObjectChunk_Data{Data: []byte(content)}},
	}}
	return scoped.PutObject(stream)
}

func getTestObject(t *testing.T, scoped *scopedHostService, memberID, collection, key string) (string, *proto.GetObjectMetadata) {
	t.Helper()

	stream := &fakeGetStream{ctx: context.Background()}
	if err := scoped.GetObject(&proto.GetObjectRequest{
		OrgId: "atelier", MemberId: memberID, Collection: collection, Key: key,
	}, stream); err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if len(stream.chunks) == 0 {
		t.Fatal("aucune tranche reçue")
	}

	meta := stream.chunks[0].GetMetadata()
	if meta == nil {
		t.Fatal("la première tranche doit porter les métadonnées")
	}

	var data strings.Builder
	for _, chunk := range stream.chunks[1:] {
		data.Write(chunk.GetData())
	}
	return data.String(), meta
}

// L'aller-retour COMPLET à travers un vrai sous-processus : le streaming
// du client SDK (découpage en tranches, EOF) contre le serveur hôte réel,
// via l'outil echo_store du plugin d'essai. C'est le seul test où les deux
// bouts du contrat gRPC se rencontrent.
func TestObjectStoreEndToEnd(t *testing.T) {
	cfg := config.Plugins{Dir: t.TempDir()}
	data, err := os.ReadFile(echoBinary)
	if err != nil {
		t.Fatalf("lecture du binaire echo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfg.Dir, "echo"), data, 0o755); err != nil {
		t.Fatalf("installation du binaire echo: %v", err)
	}

	db, err := persistence.Open(context.Background(), config.StorageApplication{
		Driver: "sqlite3",
		Path:   filepath.Join(t.TempDir(), "app.sqlite"),
	})
	if err != nil {
		t.Fatalf("ouverture de la base: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	box, err := secretbox.NewPlugins(testSessionSecret)
	if err != nil {
		t.Fatalf("dérivation de la clé: %v", err)
	}

	host := NewHostService(db, box).WithObjectStore("https://automata.test", ObjectStoreLimits{})
	manager := NewManager(cfg, host, []string{"main"})
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(manager.Shutdown)

	seedOrgAndMember(t, db, "atelier", "cam")

	result, isError, err := manager.CallTool(context.Background(), "echo", "echo_store",
		CallContext{OrgID: "atelier", MemberID: "cam"}, "{}", 0)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if isError || !strings.HasPrefix(result, "store ok slug=") {
		t.Errorf("echo_store = (%q, %v)", result, isError)
	}
	if !strings.Contains(result, "url=https://automata.test/s/") {
		t.Errorf("url absente du résultat: %q", result)
	}
}

// L'aller-retour de base, et l'étanchéité du périmètre : un membre inconnu
// de l'organisation est refusé avant de toucher au magasin.
func TestObjectStoreRoundTrip(t *testing.T) {
	scoped, _ := newObjectHost(t, ObjectStoreLimits{})

	if err := putTestObject(t, scoped, "cam", "spaces/demo/draft", "index.html", "<html>ok</html>"); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	data, meta := getTestObject(t, scoped, "cam", "spaces/demo/draft", "index.html")
	if !meta.Found || data != "<html>ok</html>" || meta.ContentType != "text/html" {
		t.Errorf("objet = (%q, %+v)", data, meta)
	}

	// Objet absent : found=false, pas d'erreur.
	_, meta = getTestObject(t, scoped, "cam", "spaces/demo/draft", "missing.css")
	if meta.Found {
		t.Error("un objet absent doit répondre found=false")
	}

	// Membre d'une autre organisation : refus franc.
	err := putTestObject(t, scoped, "intrus", "spaces/demo/draft", "index.html", "pwn")
	if status.Code(err) != codes.NotFound {
		t.Errorf("membre inconnu = %v, attendu NotFound", err)
	}
}

// Les quotas du magasin : taille d'objet, volume total (remplacement
// déduit) et nombre d'objets.
func TestObjectStoreQuotas(t *testing.T) {
	scoped, _ := newObjectHost(t, ObjectStoreLimits{
		MaxObjectBytes: 10, MaxMemberBytes: 25, MaxMemberObjects: 2,
	})

	if err := putTestObject(t, scoped, "cam", "spaces/a/draft", "big.html", strings.Repeat("x", 11)); status.Code(err) != codes.ResourceExhausted {
		t.Errorf("objet trop gros = %v, attendu ResourceExhausted", err)
	}

	if err := putTestObject(t, scoped, "cam", "spaces/a/draft", "index.html", strings.Repeat("x", 10)); err != nil {
		t.Fatalf("premier objet: %v", err)
	}
	// Remplacer un objet ne compte pas deux fois sa taille.
	if err := putTestObject(t, scoped, "cam", "spaces/a/draft", "index.html", strings.Repeat("y", 10)); err != nil {
		t.Errorf("remplacement refusé: %v", err)
	}

	if err := putTestObject(t, scoped, "cam", "spaces/a/draft", "style.css", strings.Repeat("x", 10)); err != nil {
		t.Fatalf("second objet: %v", err)
	}

	// 20 octets et 2 objets consommés : le troisième objet dépasse le
	// compte avant le volume.
	if err := putTestObject(t, scoped, "cam", "spaces/a/draft", "extra.js", "x"); status.Code(err) != codes.ResourceExhausted {
		t.Errorf("troisième objet = %v, attendu ResourceExhausted", err)
	}
}

// Les noms de collections et de clés sont bornés : ils finissent dans des
// URLs publiques et des requêtes de préfixe.
func TestObjectStoreRejectsBadNames(t *testing.T) {
	scoped, _ := newObjectHost(t, ObjectStoreLimits{})

	for _, bad := range []struct{ collection, key string }{
		{"spaces/../secrets", "index.html"},
		{"/spaces/demo", "index.html"},
		{"spaces/demo/", "index.html"},
		{"spaces//demo", "index.html"},
		{"spaces/demo", "../escape.html"},
		{"spaces/demo", "Fichier Majuscule.html"},
		{"spaces/demo", ""},
	} {
		err := putTestObject(t, scoped, "cam", bad.collection, bad.key, "x")
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("(%q, %q) = %v, attendu InvalidArgument", bad.collection, bad.key, err)
		}
	}
}

// Publication : collection vide refusée, slug stable à la republication,
// dépublication idempotente, URL composée depuis la base configurée.
func TestObjectStorePublication(t *testing.T) {
	scoped, _ := newObjectHost(t, ObjectStoreLimits{})
	ctx := context.Background()

	if _, err := scoped.PublishCollection(ctx, &proto.PublishCollectionRequest{
		OrgId: "atelier", MemberId: "cam", Collection: "spaces/demo/live",
	}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("collection vide = %v, attendu FailedPrecondition", err)
	}

	if err := putTestObject(t, scoped, "cam", "spaces/demo/live", "index.html", "<html></html>"); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	first, err := scoped.PublishCollection(ctx, &proto.PublishCollectionRequest{
		OrgId: "atelier", MemberId: "cam", Collection: "spaces/demo/live",
	})
	if err != nil {
		t.Fatalf("PublishCollection: %v", err)
	}
	if len(first.Slug) != slugLength || first.Slug != strings.ToLower(first.Slug) {
		t.Errorf("slug = %q", first.Slug)
	}
	if first.Url != "https://automata.test/s/"+first.Slug+"/" {
		t.Errorf("url = %q", first.Url)
	}

	second, err := scoped.PublishCollection(ctx, &proto.PublishCollectionRequest{
		OrgId: "atelier", MemberId: "cam", Collection: "spaces/demo/live",
	})
	if err != nil || second.Slug != first.Slug {
		t.Errorf("republication = (%+v, %v), le slug doit être stable", second, err)
	}

	publications, err := scoped.ListPublications(ctx, &proto.ListPublicationsRequest{OrgId: "atelier", MemberId: "cam"})
	if err != nil || len(publications.Publications) != 1 {
		t.Fatalf("ListPublications = (%+v, %v)", publications, err)
	}

	unpub, err := scoped.UnpublishCollection(ctx, &proto.UnpublishCollectionRequest{
		OrgId: "atelier", MemberId: "cam", Collection: "spaces/demo/live",
	})
	if err != nil || !unpub.Existed {
		t.Errorf("UnpublishCollection = (%+v, %v)", unpub, err)
	}
	unpub, err = scoped.UnpublishCollection(ctx, &proto.UnpublishCollectionRequest{
		OrgId: "atelier", MemberId: "cam", Collection: "spaces/demo/live",
	})
	if err != nil || unpub.Existed {
		t.Errorf("seconde dépublication = (%+v, %v)", unpub, err)
	}
}

// PreviewCollection : refus sans fabrique câblée, refus d'une collection
// vide, sinon l'URL et l'échéance de la fabrique.
func TestObjectStorePreview(t *testing.T) {
	scoped, _ := newObjectHost(t, ObjectStoreLimits{})
	ctx := context.Background()

	request := &proto.PreviewCollectionRequest{OrgId: "atelier", MemberId: "cam", Collection: "spaces/demo/draft"}

	if _, err := scoped.PreviewCollection(ctx, request); status.Code(err) != codes.Unavailable {
		t.Errorf("sans fabrique = %v, attendu Unavailable", err)
	}

	expires := time.Now().Add(time.Hour).UTC()
	scoped.HostService.WithPreviewMinter(func(pluginName, orgID, memberID, collection string) (string, time.Time, error) {
		if pluginName != "pages" || orgID != "atelier" || memberID != "cam" || collection != "spaces/demo/draft" {
			t.Errorf("fabrique appelée avec (%q, %q, %q, %q)", pluginName, orgID, memberID, collection)
		}
		return "https://automata.test/d/jeton/", expires, nil
	})

	if _, err := scoped.PreviewCollection(ctx, request); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("collection vide = %v, attendu FailedPrecondition", err)
	}

	if err := putTestObject(t, scoped, "cam", "spaces/demo/draft", "index.html", "<html></html>"); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	resp, err := scoped.PreviewCollection(ctx, request)
	if err != nil {
		t.Fatalf("PreviewCollection: %v", err)
	}
	if resp.Url != "https://automata.test/d/jeton/" || resp.ExpiresAt != expires.Format(time.RFC3339) {
		t.Errorf("réponse = %+v", resp)
	}
}

// CopyCollection remplace la cible, et DeleteCollection emporte la
// publication dans la même transaction : pas de lien public orphelin.
func TestObjectStoreCopyAndDelete(t *testing.T) {
	scoped, _ := newObjectHost(t, ObjectStoreLimits{})
	ctx := context.Background()

	if err := putTestObject(t, scoped, "cam", "spaces/demo/draft", "index.html", "v2"); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if err := putTestObject(t, scoped, "cam", "spaces/demo/live", "obsolete.css", "v1"); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	copied, err := scoped.CopyCollection(ctx, &proto.CopyCollectionRequest{
		OrgId: "atelier", MemberId: "cam", From: "spaces/demo/draft", To: "spaces/demo/live",
	})
	if err != nil || copied.Copied != 1 {
		t.Fatalf("CopyCollection = (%+v, %v)", copied, err)
	}

	objects, err := scoped.ListObjects(ctx, &proto.ListObjectsRequest{
		OrgId: "atelier", MemberId: "cam", Collection: "spaces/demo/live",
	})
	if err != nil || len(objects.Entries) != 1 || objects.Entries[0].Key != "index.html" {
		t.Errorf("live = (%+v, %v)", objects, err)
	}

	if _, err := scoped.PublishCollection(ctx, &proto.PublishCollectionRequest{
		OrgId: "atelier", MemberId: "cam", Collection: "spaces/demo/live",
	}); err != nil {
		t.Fatalf("PublishCollection: %v", err)
	}

	deleted, err := scoped.DeleteCollection(ctx, &proto.DeleteCollectionRequest{
		OrgId: "atelier", MemberId: "cam", Collection: "spaces/demo/live",
	})
	if err != nil || deleted.Deleted != 1 {
		t.Fatalf("DeleteCollection = (%+v, %v)", deleted, err)
	}

	publications, err := scoped.ListPublications(ctx, &proto.ListPublicationsRequest{OrgId: "atelier", MemberId: "cam"})
	if err != nil || len(publications.Publications) != 0 {
		t.Errorf("la publication doit tomber avec la collection: (%+v, %v)", publications, err)
	}
}
