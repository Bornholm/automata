package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/bornholm/automata/pkg/pluginsdk"
	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

const (
	testUser     = "cam@example.test"
	testPassword = "mot-de-passe-imap"
)

// startIMAPServer lance un serveur IMAP mémoire avec quelques messages.
func startIMAPServer(t *testing.T, messages ...string) (host string, port int) {
	t.Helper()

	user := imapmemserver.NewUser(testUser, testPassword)
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatalf("création d'INBOX: %v", err)
	}
	for _, raw := range messages {
		buf := bytes.NewBufferString(raw)
		if _, err := user.Append("INBOX", &memLiteral{Buffer: buf, size: int64(buf.Len())}, &imap.AppendOptions{}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	mem := imapmemserver.New()
	mem.AddUser(user)

	server := imapserver.New(&imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = server.Serve(ln) }()

	addr := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", addr.Port
}

type memLiteral struct {
	*bytes.Buffer
	size int64
}

func (l *memLiteral) Size() int64 { return l.size }

const sampleEmail = "From: yann@example.test\r\n" +
	"To: cam@example.test\r\n" +
	"Subject: Reunion demain\r\n" +
	"Message-ID: <msg-1@example.test>\r\n" +
	"Date: Mon, 17 Aug 2026 10:00:00 +0200\r\n" +
	"\r\n" +
	"Peux-tu confirmer ta presence a la reunion de demain ?\r\n"

// fakeHost implémente pluginsdk.HostClient en mémoire.
type fakeHost struct {
	mu      sync.Mutex
	configs map[string]string
	secrets map[string]string
}

func newFakeHost() *fakeHost {
	return &fakeHost{configs: map[string]string{}, secrets: map[string]string{}}
}

func key(orgID, memberID string) string { return orgID + ":" + memberID }

func (f *fakeHost) GetConfig(_ context.Context, orgID, memberID string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, ok := f.configs[key(orgID, memberID)]
	return raw, ok, nil
}
func (f *fakeHost) SaveConfig(_ context.Context, orgID, memberID, raw string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configs[key(orgID, memberID)] = raw
	return nil
}
func (f *fakeHost) ListConfigs(context.Context) ([]pluginsdk.ConfigEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var entries []pluginsdk.ConfigEntry
	for k, raw := range f.configs {
		parts := strings.SplitN(k, ":", 2)
		entries = append(entries, pluginsdk.ConfigEntry{OrgID: parts[0], MemberID: parts[1], ConfigJSON: raw})
	}
	return entries, nil
}
func (f *fakeHost) GetSecret(_ context.Context, orgID, memberID, k string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.secrets[key(orgID, memberID)+":"+k]
	return v, ok, nil
}
func (f *fakeHost) SetSecret(_ context.Context, orgID, memberID, k, v string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.secrets[key(orgID, memberID)+":"+k] = v
	return nil
}
func (f *fakeHost) DeleteSecret(_ context.Context, orgID, memberID, k string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.secrets, key(orgID, memberID)+":"+k)
	return nil
}
func (f *fakeHost) Notify(context.Context, string, string, string) error { return nil }

func newTestPlugin(t *testing.T, cfg memberConfig) (*Plugin, *fakeHost) {
	t.Helper()

	plugin := newPlugin()
	host := newFakeHost()
	plugin.SetHostClient(host)

	_ = host.SaveConfig(context.Background(), "atelier", "cam", cfg.marshal())
	_ = host.SetSecret(context.Background(), "atelier", "cam", secretKeyPassword, testPassword)

	return plugin, host
}

func testCallContext() *proto.CallContext {
	return &proto.CallContext{OrgId: "atelier", MemberId: "cam", Scope: "personal", ScopeId: "cam"}
}

func imapConfig(host string, port int, allowRead, allowWrite bool) memberConfig {
	return memberConfig{
		IMAPHost: host, IMAPPort: port, IMAPInsecure: true,
		SMTPHost: "127.0.0.1", SMTPPort: 1, SMTPInsecure: true,
		Username: testUser, From: testUser,
		AllowRead: allowRead, AllowWrite: allowWrite,
	}
}

// Les interrupteurs du MEMBRE gouvernent les outils exposés : lecture
// seule = aucun outil d'envoi, rien à refuser puisque rien à voir.
func TestListTools_FollowsMemberSwitches(t *testing.T) {
	host, port := startIMAPServer(t)

	cases := []struct {
		name       string
		read, send bool
		want       []string
	}{
		{"lecture seule", true, false, []string{"email_list_recent", "email_read", "email_search"}},
		{"lecture et écriture", true, true, []string{"email_list_recent", "email_read", "email_search", "email_send", "email_reply"}},
		{"écriture seule", false, true, []string{"email_send", "email_reply"}},
		{"tout coupé", false, false, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plugin, _ := newTestPlugin(t, imapConfig(host, port, tc.read, tc.send))

			out, err := plugin.ListTools(context.Background(), &proto.ListToolsInput{Ctx: testCallContext()})
			if err != nil {
				t.Fatalf("ListTools: %v", err)
			}

			var got []string
			for _, tool := range out.Tools {
				got = append(got, tool.Name)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("outils = %v, attendu %v", got, tc.want)
			}

			// Les outils d'envoi ne sont jamais read_only : l'hôte les
			// soumet à confirmation.
			for _, tool := range out.Tools {
				isSend := tool.Name == "email_send" || tool.Name == "email_reply"
				if isSend && tool.ReadOnly {
					t.Errorf("%s annoncé read_only", tool.Name)
				}
				if !isSend && !tool.ReadOnly {
					t.Errorf("%s non annoncé read_only", tool.Name)
				}
			}
		})
	}
}

func TestCallTool_ListsAndReads(t *testing.T) {
	host, port := startIMAPServer(t, sampleEmail)
	plugin, _ := newTestPlugin(t, imapConfig(host, port, true, false))

	out, err := plugin.CallTool(context.Background(), &proto.CallToolInput{
		Ctx: testCallContext(), Name: "email_list_recent", ArgumentsJson: `{"count":5}`,
	})
	if err != nil || out.IsError {
		t.Fatalf("email_list_recent: %v / %s", err, out.GetResultText())
	}
	if !strings.Contains(out.ResultText, "Reunion demain") || !strings.Contains(out.ResultText, "yann@example.test") {
		t.Errorf("liste inattendue: %s", out.ResultText)
	}

	out, err = plugin.CallTool(context.Background(), &proto.CallToolInput{
		Ctx: testCallContext(), Name: "email_read", ArgumentsJson: `{"id":"1"}`,
	})
	if err != nil || out.IsError {
		t.Fatalf("email_read: %v / %s", err, out.GetResultText())
	}
	if !strings.Contains(out.ResultText, "confirmer ta presence") {
		t.Errorf("corps absent: %s", out.ResultText)
	}
}

// La désactivation de la lecture est aussi vérifiée à l'APPEL : même si un
// outil traînait dans un tour déjà ouvert, il refuserait.
func TestCallTool_EnforcesSwitchesAtCallTime(t *testing.T) {
	host, port := startIMAPServer(t, sampleEmail)
	plugin, _ := newTestPlugin(t, imapConfig(host, port, false, false))

	out, err := plugin.CallTool(context.Background(), &proto.CallToolInput{
		Ctx: testCallContext(), Name: "email_list_recent",
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !out.IsError {
		t.Fatal("la lecture désactivée a été servie")
	}
}

// Une confirmation rejouée ne renvoie pas l'email : la clé d'idempotence
// est mémorisée à la première soumission.
func TestCallTool_SendIsIdempotent(t *testing.T) {
	host, port := startIMAPServer(t)
	smtpHost, smtpPort, received := startSMTPServer(t)

	cfg := imapConfig(host, port, true, true)
	cfg.SMTPHost, cfg.SMTPPort = smtpHost, smtpPort
	plugin, _ := newTestPlugin(t, cfg)

	call := &proto.CallToolInput{
		Ctx:           &proto.CallContext{OrgId: "atelier", MemberId: "cam", IdempotencyKey: "act-1"},
		Name:          "email_send",
		ArgumentsJson: `{"to":"yann@example.test","subject":"Salut","body":"Bonjour Yann"}`,
	}

	out, err := plugin.CallTool(context.Background(), call)
	if err != nil || out.IsError {
		t.Fatalf("premier envoi: %v / %s", err, out.GetResultText())
	}

	out, err = plugin.CallTool(context.Background(), call)
	if err != nil || out.IsError {
		t.Fatalf("rejeu: %v / %s", err, out.GetResultText())
	}
	if !strings.Contains(out.ResultText, "already sent") {
		t.Errorf("le rejeu n'a pas été reconnu: %s", out.ResultText)
	}

	if n := len(received()); n != 1 {
		t.Fatalf("%d message(s) SMTP reçu(s), attendu 1", n)
	}
	if !strings.Contains(received()[0], "Bonjour Yann") {
		t.Errorf("corps absent du message SMTP")
	}
}

// Une erreur d'authentification ne divulgue jamais le mot de passe.
func TestCallTool_AuthFailureLeaksNoSecret(t *testing.T) {
	host, port := startIMAPServer(t)
	plugin, fake := newTestPlugin(t, imapConfig(host, port, true, false))
	_ = fake.SetSecret(context.Background(), "atelier", "cam", secretKeyPassword, "mauvais-mot-de-passe")

	out, err := plugin.CallTool(context.Background(), &proto.CallToolInput{
		Ctx: testCallContext(), Name: "email_list_recent",
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !out.IsError {
		t.Fatal("l'authentification refusée n'a pas été signalée")
	}
	if strings.Contains(out.ResultText, "mauvais-mot-de-passe") {
		t.Errorf("le secret apparaît dans l'erreur: %s", out.ResultText)
	}
}
