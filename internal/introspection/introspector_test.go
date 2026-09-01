package introspection

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/memory"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
)

func openTestDB(t *testing.T) *persistence.DB {
	t.Helper()

	db, err := persistence.Open(context.Background(), config.StorageApplication{
		Driver: "sqlite3",
		Path:   filepath.Join(t.TempDir(), "automata.sqlite"),
		Pragmas: config.Pragmas{
			ForeignKeys: true,
			JournalMode: "WAL",
			BusyTimeout: config.Duration(5 * time.Second),
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return db
}

// scriptedClient rejoue une réponse fixe et compte les appels.
type scriptedClient struct {
	mu       sync.Mutex
	response string
	prompts  []string
	calls    int
}

func (c *scriptedClient) ChatCompletion(_ context.Context, funcs ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	opts := &llm.ChatCompletionOptions{}
	for _, fn := range funcs {
		fn(opts)
	}

	c.mu.Lock()
	c.calls++
	for _, msg := range opts.Messages {
		c.prompts = append(c.prompts, msg.Content())
	}
	c.mu.Unlock()

	return llm.NewChatCompletionResponse(llm.NewMessage(llm.RoleAssistant, c.response), llm.NewChatCompletionUsage(1, 1, 2)), nil
}

func (c *scriptedClient) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// fakeNotifier note les messages poussés.
type fakeNotifier struct {
	mu   sync.Mutex
	sent []string
}

func (n *fakeNotifier) NotifyMember(_ context.Context, _, message string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, message)
	return nil
}

// fakeMemory rejoue des motifs.
type fakeMemory struct {
	memories []memory.Memory
}

func (m *fakeMemory) ListByScope(context.Context, model.OrgID, model.Scope, model.ScopeID) ([]memory.Memory, error) {
	return m.memories, nil
}

// seedMember crée l'organisation et un membre rattaché.
func seedMember(t *testing.T, db *persistence.DB, muted bool) persistence.Member {
	t.Helper()

	now := time.Now().UTC()
	member := persistence.Member{
		ID: "cam", OrgID: "atelier", DisplayName: "Cam", Role: "member",
		Provider: "whatsapp", ExternalUserID: "cam-ext", LinkedAt: now,
		SuggestionsMuted: muted, CreatedAt: now, UpdatedAt: now,
	}

	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		if err := persistence.NewOrganizationRepository().Insert(context.Background(), tx, persistence.Organization{
			ID: "atelier", DisplayName: "Atelier", CreatedAt: now, UpdatedAt: now,
		}, true); err != nil {
			return err
		}
		return persistence.NewMemberRepository().Insert(context.Background(), tx, member, true)
	})
	if err != nil {
		t.Fatalf("semis: %v", err)
	}

	return member
}

// seedExpiredPlans insère des plans d'actions expirés du membre, chacun
// avec sa première action — c'est la trace de friction principale.
func seedExpiredPlans(t *testing.T, db *persistence.DB, member persistence.Member, count int) {
	t.Helper()

	now := time.Now().UTC().Format(time.RFC3339)
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		// action_plans référence conversations : la conversation d'ancrage
		// doit exister.
		if _, err := tx.ExecContext(context.Background(), `INSERT OR IGNORE INTO conversations
			(id, org_id, provider, external_channel_id, kind, scope, scope_id, created_at, updated_at)
			VALUES ('conv-1', ?, 'whatsapp', 'cam-ext', 'private', 'personal', ?, ?, ?)`,
			member.OrgID, member.ID, now, now); err != nil {
			return err
		}
		for i := 0; i < count; i++ {
			planID := "plan-" + string(rune('a'+i))
			if _, err := tx.ExecContext(context.Background(), `INSERT INTO action_plans
				(id, org_id, conversation_id, created_by, scope, scope_id, status, expires_at, created_at, updated_at)
				VALUES (?, ?, 'conv-1', ?, 'personal', ?, 'expired', ?, ?, ?)`,
				planID, member.OrgID, member.ID, member.ID, now, now, now); err != nil {
				return err
			}
			if _, err := tx.ExecContext(context.Background(), `INSERT INTO actions
				(id, plan_id, position, agent_id, mcp_server, tool_name, arguments_json, summary,
				 required_permission, requires_confirmation, status, created_at)
				VALUES (?, ?, 0, 'main', 'email', 'email_send', '{}', 'CONTENU PRIVÉ', 'email.personal.write', 1, 'expired', ?)`,
				planID+"-a", planID, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("semis des plans: %v", err)
	}
}

func newIntrospector(t *testing.T, db *persistence.DB, client llm.ChatCompletionClient) *Introspector {
	t.Helper()

	i, err := New(db, client, config.Introspection{}, "https://automata.test", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return i
}

// Le parcours nominal : trois plans expirés, une suggestion émise et
// poussée, et le contenu privé du plan n'atteint jamais le modèle.
func TestIntrospection_FrictionYieldsOneSuggestion(t *testing.T) {
	db := openTestDB(t)
	member := seedMember(t, db, false)
	seedExpiredPlans(t, db, member, 3)

	client := &scriptedClient{response: `[{"kind":"fix","title":"Vos envois de courriels expirent","body":"Trois envois proposés n'ont jamais été confirmés. Voulez-vous que je vous les rappelle avant expiration ?","push":true}]`}
	notifier := &fakeNotifier{}
	introspector := newIntrospector(t, db, client).WithNotifier(notifier)

	if err := introspector.introspectMember(context.Background(), member); err != nil {
		t.Fatalf("introspectMember: %v", err)
	}

	// Le dossier ne porte JAMAIS le résumé du plan : c'est la frontière de
	// confidentialité du paquet.
	for _, prompt := range client.prompts {
		if strings.Contains(prompt, "CONTENU PRIVÉ") {
			t.Fatal("le résumé d'un plan d'actions a fuité vers le modèle")
		}
	}
	// Les métadonnées, elles, y sont.
	joined := strings.Join(client.prompts, "\n")
	if !strings.Contains(joined, "email_send") {
		t.Error("le dossier devrait nommer l'outil du plan expiré")
	}

	var suggestions []persistence.Suggestion
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		suggestions, err = persistence.NewSuggestionRepository().ListByMember(context.Background(), tx, member.OrgID, member.ID, 10)
		return err
	})
	if err != nil {
		t.Fatalf("relecture: %v", err)
	}
	if len(suggestions) != 1 {
		t.Fatalf("%d suggestions, une attendue", len(suggestions))
	}
	if suggestions[0].Status != persistence.SuggestionStatusDelivered {
		t.Errorf("statut = %q, attendu delivered (push demandé)", suggestions[0].Status)
	}
	if len(notifier.sent) != 1 {
		t.Errorf("%d messages poussés, un attendu", len(notifier.sent))
	}
}

// push: false n'envoie rien : la suggestion attend sur la page de profil.
func TestIntrospection_QuietSuggestionIsNotPushed(t *testing.T) {
	db := openTestDB(t)
	member := seedMember(t, db, false)
	seedExpiredPlans(t, db, member, 2)

	client := &scriptedClient{response: `[{"kind":"habit","title":"Des réponses plus brèves","body":"Vous semblez préférer les réponses courtes.","push":false}]`}
	notifier := &fakeNotifier{}
	introspector := newIntrospector(t, db, client).WithNotifier(notifier)

	if err := introspector.introspectMember(context.Background(), member); err != nil {
		t.Fatalf("introspectMember: %v", err)
	}

	if len(notifier.sent) != 0 {
		t.Errorf("%d messages poussés, aucun attendu", len(notifier.sent))
	}
}

// Un dossier vide ne part jamais au modèle : c'est la parcimonie qui rend
// la passe gratuite pour la plupart des semaines.
func TestIntrospection_EmptyDossierSkipsTheModel(t *testing.T) {
	db := openTestDB(t)
	member := seedMember(t, db, false)

	client := &scriptedClient{response: `[]`}
	introspector := newIntrospector(t, db, client)

	if err := introspector.introspectMember(context.Background(), member); err != nil {
		t.Fatalf("introspectMember: %v", err)
	}
	if client.count() != 0 {
		t.Errorf("%d appels LLM, aucun attendu sans matière", client.count())
	}
}

// Un membre muet n'est jamais traité — ni collecté, ni soumis au modèle.
func TestIntrospection_MutedMemberIsNeverProcessed(t *testing.T) {
	db := openTestDB(t)
	seedMember(t, db, true)

	client := &scriptedClient{response: `[]`}
	introspector := newIntrospector(t, db, client)

	members, err := introspector.eligibleMembers(context.Background())
	if err != nil {
		t.Fatalf("eligibleMembers: %v", err)
	}
	if len(members) != 0 {
		t.Errorf("%d membres éligibles, aucun attendu (muet)", len(members))
	}
}

// Les suggestions déjà émises figurent dans le dossier avec leur sort :
// c'est ce qui permet au prompt d'interdire la répétition.
func TestIntrospection_PreviousSuggestionsReachTheModel(t *testing.T) {
	db := openTestDB(t)
	member := seedMember(t, db, false)
	seedExpiredPlans(t, db, member, 2)

	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return persistence.NewSuggestionRepository().Insert(context.Background(), tx, persistence.Suggestion{
			ID: "sug-1", OrgID: member.OrgID, MemberID: member.ID,
			Kind: "automation", Title: "Programmer la météo", Body: "b",
			Status: persistence.SuggestionStatusDismissed, CreatedAt: time.Now().UTC(),
		})
	})
	if err != nil {
		t.Fatalf("semis: %v", err)
	}

	client := &scriptedClient{response: `[]`}
	introspector := newIntrospector(t, db, client)

	if err := introspector.introspectMember(context.Background(), member); err != nil {
		t.Fatalf("introspectMember: %v", err)
	}

	joined := strings.Join(client.prompts, "\n")
	if !strings.Contains(joined, "Programmer la météo — dismissed") {
		t.Error("la suggestion écartée devrait figurer dans le dossier avec son sort")
	}
}

// L'ancrage initial ne déclenche RIEN : un membre fraîchement rattaché
// n'est pas introspecté sur un dossier d'une heure.
func TestIntrospection_FirstTickOnlyAnchors(t *testing.T) {
	db := openTestDB(t)
	member := seedMember(t, db, false)
	seedExpiredPlans(t, db, member, 3)

	client := &scriptedClient{response: `[]`}
	introspector := newIntrospector(t, db, client)

	if err := introspector.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if client.count() != 0 {
		t.Error("le premier tick doit ancrer sans introspection")
	}

	// L'échéance passée, la passe a bien lieu.
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return persistence.NewMaintenanceRunRepository().SetLastRun(context.Background(), tx,
			taskPrefix+member.OrgID+"/"+member.ID, time.Now().UTC().AddDate(0, 0, -8))
	})
	if err != nil {
		t.Fatalf("recul de l'ancrage: %v", err)
	}

	if err := introspector.Tick(context.Background()); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if client.count() != 1 {
		t.Errorf("%d appels LLM, un attendu après l'échéance", client.count())
	}
}

// Une réponse hors contrat est rejetée en bloc : nature inventée, plusieurs
// suggestions, titre vide.
func TestParseDraft_RejectsAnythingOffContract(t *testing.T) {
	for _, raw := range []string{
		`[{"kind":"miracle","title":"t","body":"b"}]`,
		`[{"kind":"fix","title":"a","body":"b"},{"kind":"fix","title":"c","body":"d"}]`,
		`[{"kind":"fix","title":"","body":"b"}]`,
		`pas du JSON`,
	} {
		if _, ok, err := parseDraft(raw); err == nil && ok {
			t.Errorf("parseDraft(%q) accepté, refus attendu", raw)
		}
	}

	if _, ok, err := parseDraft("```json\n[]\n```"); err != nil || ok {
		t.Errorf("un [] balisé devrait être un silence propre, err=%v ok=%v", err, ok)
	}
}

// Seuls les motifs issus de la réflexion entrent au dossier : les souvenirs
// ordinaires (« retiens que… ») sont du contenu confié, pas des
// observations, et n'ont rien à faire dans une passe d'introspection.
func TestIntrospection_OnlyReflectionPatternsEnterTheDossier(t *testing.T) {
	db := openTestDB(t)
	member := seedMember(t, db, false)

	mem := &fakeMemory{memories: []memory.Memory{
		{Content: "Semble planifier ses journées le dimanche soir.", Metadata: map[string]string{"origin": "episode_reflection"}},
		{Content: "Le code du portail est 4712.", Metadata: map[string]string{"origin": ""}},
	}}
	client := &scriptedClient{response: `[]`}
	introspector := newIntrospector(t, db, client).WithMemory(mem)

	if err := introspector.introspectMember(context.Background(), member); err != nil {
		t.Fatalf("introspectMember: %v", err)
	}

	joined := strings.Join(client.prompts, "\n")
	if !strings.Contains(joined, "dimanche soir") {
		t.Error("le motif de réflexion devrait figurer au dossier")
	}
	if strings.Contains(joined, "4712") {
		t.Fatal("un souvenir ordinaire a fuité vers l'introspection")
	}
}
