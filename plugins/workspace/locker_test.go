package main

import (
	"context"
	"strings"
	"testing"

	"github.com/bornholm/automata/pkg/pluginsdk"
	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// fakeHost tient lieu de magasin d'objets de l'hôte. Il note ce qui a été
// scellé : c'est la promesse que le casier fait à la personne, et rien
// d'autre dans ce plugin ne la vérifie.
type fakeHost struct {
	pluginsdk.UnimplementedHostClient
	objects map[string][]byte
	types   map[string]string
	sealed  map[string]bool
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		objects: map[string][]byte{},
		types:   map[string]string{},
		sealed:  map[string]bool{},
	}
}

func (h *fakeHost) PutObjectSealed(_ context.Context, _, _, collection, key, contentType string, data []byte) error {
	h.objects[collection+"/"+key] = data
	h.types[collection+"/"+key] = contentType
	h.sealed[collection+"/"+key] = true
	return nil
}

func (h *fakeHost) PutObject(_ context.Context, _, _, collection, key, contentType string, data []byte) error {
	h.objects[collection+"/"+key] = data
	h.types[collection+"/"+key] = contentType
	h.sealed[collection+"/"+key] = false
	return nil
}

func (h *fakeHost) GetObject(_ context.Context, _, _, collection, key string) ([]byte, string, bool, error) {
	data, ok := h.objects[collection+"/"+key]
	return data, h.types[collection+"/"+key], ok, nil
}

func (h *fakeHost) DeleteObject(_ context.Context, _, _, collection, key string) (bool, error) {
	if _, ok := h.objects[collection+"/"+key]; !ok {
		return false, nil
	}
	delete(h.objects, collection+"/"+key)
	return true, nil
}

func (h *fakeHost) ListObjects(_ context.Context, _, _, collection string) ([]pluginsdk.ObjectEntry, error) {
	var entries []pluginsdk.ObjectEntry
	for path, data := range h.objects {
		key, found := strings.CutPrefix(path, collection+"/")
		if !found {
			continue
		}
		entries = append(entries, pluginsdk.ObjectEntry{
			Key: key, Size: int64(len(data)), Sealed: h.sealed[path],
		})
	}
	return entries, nil
}

func newLockerPlugin(t *testing.T) (*Plugin, *fakeLeash, *fakeHost) {
	t.Helper()

	p, leash := newTestPlugin(t)
	host := newFakeHost()
	p.SetHostClient(host)

	return p, leash, host
}

func callLocker(t *testing.T, p *Plugin, name, args string) *proto.CallToolOutput {
	t.Helper()

	out, err := p.CallTool(context.Background(), &proto.CallToolInput{
		Name: name, ArgumentsJson: args, Ctx: testCallContext("cam"),
	})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return out
}

// Le geste central : produire un fichier, le ranger, le retrouver. C'est
// tout l'objet du casier — le bac à sable, lui, aurait perdu le fichier.
func TestLocker_SaveThenGetRoundTrip(t *testing.T) {
	p, leash, host := newLockerPlugin(t)
	leash.files["rapport.pdf"] = []byte("%PDF-1.7 contenu")

	out := callLocker(t, p, "locker_save", `{"path":"rapport.pdf"}`)
	if out.IsError {
		t.Fatalf("locker_save a échoué: %s", out.ResultText)
	}
	if !host.sealed["locker/rapport.pdf"] {
		t.Error("le fichier doit être scellé : c'est la promesse du casier")
	}

	// Le bac à sable est purgé, comme il l'est après son TTL.
	delete(leash.files, "rapport.pdf")

	out = callLocker(t, p, "locker_get", `{"name":"rapport.pdf"}`)
	if out.IsError {
		t.Fatalf("locker_get a échoué: %s", out.ResultText)
	}
	if got := string(leash.files["rapport.pdf"]); got != "%PDF-1.7 contenu" {
		t.Errorf("fichier restitué = %q, attendu le contenu d'origine", got)
	}
}

// Ranger ne doit jamais écraser en silence : la version précédente est le
// travail de quelqu'un. L'outil qui écrase est distinct, et confirmé.
func TestLocker_SaveRefusesToOverwrite(t *testing.T) {
	p, leash, _ := newLockerPlugin(t)
	leash.files["notes.txt"] = []byte("version 1")

	if out := callLocker(t, p, "locker_save", `{"path":"notes.txt"}`); out.IsError {
		t.Fatalf("premier rangement: %s", out.ResultText)
	}

	leash.files["notes.txt"] = []byte("version 2")
	out := callLocker(t, p, "locker_save", `{"path":"notes.txt"}`)
	if !out.IsError {
		t.Fatal("le second rangement devrait être refusé")
	}
	if !strings.Contains(out.ResultText, "locker_replace") {
		t.Errorf("le refus doit nommer l'outil qui écrase, reçu: %q", out.ResultText)
	}
}

func TestLocker_ReplaceOverwrites(t *testing.T) {
	p, leash, host := newLockerPlugin(t)
	leash.files["notes.txt"] = []byte("version 1")
	if out := callLocker(t, p, "locker_save", `{"path":"notes.txt"}`); out.IsError {
		t.Fatalf("rangement initial: %s", out.ResultText)
	}

	leash.files["notes.txt"] = []byte("version 2")
	if out := callLocker(t, p, "locker_replace", `{"path":"notes.txt","name":"notes.txt"}`); out.IsError {
		t.Fatalf("locker_replace: %s", out.ResultText)
	}

	if got := string(host.objects["locker/notes.txt"]); got != "version 2" {
		t.Errorf("contenu = %q, attendu la version 2", got)
	}
}

func TestLocker_DeleteAndMissingFile(t *testing.T) {
	p, leash, _ := newLockerPlugin(t)
	leash.files["vieux.txt"] = []byte("à jeter")
	if out := callLocker(t, p, "locker_save", `{"path":"vieux.txt"}`); out.IsError {
		t.Fatalf("rangement: %s", out.ResultText)
	}

	if out := callLocker(t, p, "locker_delete", `{"name":"vieux.txt"}`); out.IsError {
		t.Fatalf("locker_delete: %s", out.ResultText)
	}

	// Un nom absent est une erreur métier, jamais une erreur Go : le modèle
	// doit pouvoir l'expliquer plutôt que faire échouer le tour.
	if out := callLocker(t, p, "locker_get", `{"name":"vieux.txt"}`); !out.IsError {
		t.Error("relire un fichier supprimé devrait être une erreur métier")
	}
}

// La suppression et l'écrasement ne sont PAS read_only : l'hôte les fait
// alors confirmer. C'est l'invariant du projet, et il se vérifie ici parce
// qu'un simple oubli d'annotation le romprait sans bruit.
func TestLockerTools_DestructiveOnesRequireConfirmation(t *testing.T) {
	readOnly := map[string]bool{}
	for _, tool := range lockerTools() {
		readOnly[tool.Name] = tool.ReadOnly
	}

	for _, name := range []string{"locker_list", "locker_save", "locker_get"} {
		if !readOnly[name] {
			t.Errorf("%s devrait être read_only", name)
		}
	}
	for _, name := range []string{"locker_replace", "locker_delete"} {
		if readOnly[name] {
			t.Errorf("%s ne doit PAS être read_only : il détruit et doit être confirmé", name)
		}
	}
}

func TestLockerName_NormalizesToStoreKeys(t *testing.T) {
	cases := map[string]string{
		"Rapport Final.PDF":    "rapport-final.pdf",
		"dir/sous-dir/a b.txt": "a-b.txt",
		"  espaces.md  ":       "espaces.md",
		"???":                  "",
	}
	for input, expected := range cases {
		if got := lockerName(input); got != expected {
			t.Errorf("lockerName(%q) = %q, attendu %q", input, got, expected)
		}
	}
}
