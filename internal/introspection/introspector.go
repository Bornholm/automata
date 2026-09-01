// Package introspection est la passe hebdomadaire par laquelle Automata
// réfléchit à ce qu'elle pourrait mieux faire pour chaque membre.
//
// Elle relit deux choses, et rien d'autre : les FRICTIONS déjà persistées
// (plans d'actions jamais confirmés ou en échec, rappels et tâches en
// échec) et les MOTIFS comportementaux que la réflexion épisodique a déjà
// rangés en mémoire. De ce dossier — métadonnées et textes déjà produits
// par des modèles, jamais de verbatim de conversation — elle tire AU PLUS
// UNE suggestion par membre et par semaine : automatiser un geste répété,
// activer une capacité inutilisée, corriger ce qui échoue.
//
// Le silence est le défaut. Une suggestion médiocre coûte la crédibilité de
// toutes les suivantes ; dix passes muettes ne coûtent rien. Même prudence
// que la réflexion épisodique, dont ce paquet est le prolongement agissant.
//
// La personne garde la main de bout en bout : les suggestions s'accumulent
// sur sa page de profil, seule une friction nette est poussée en
// conversation (au plus une par passe), et « ne plus rien me proposer »
// (members.suggestions_muted) éteint tout — collecte comprise.
package introspection

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bornholm/genai/llm"
	"github.com/robfig/cron/v3"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/usage"
	"github.com/bornholm/automata/internal/weblink"
)

// tickInterval est la période de vérification de l'échéance cron — la même
// mécanique que le consolidateur.
const tickInterval = time.Minute

// defaultCron est l'échéance de la passe : le lundi matin. L'introspection
// a besoin d'une semaine de matière, pas d'une nuit.
const defaultCron = "20 5 * * 1"

// taskPrefix préfixe l'ancrage maintenance_runs de chaque membre : une
// portée en retard se rattrape sans faire re-traiter les autres.
const taskPrefix = "introspection:"

// maxSuggestionChars borne titre et corps d'une suggestion.
const (
	maxTitleChars = 120
	maxBodyChars  = 600
)

// introspectionPrompt encadre la production de suggestions. Français, comme
// reflectionPrompt : seuls des textes déjà produits par des modèles et des
// métadonnées transitent ici.
const introspectionPrompt = `Tu es la conscience critique d'un assistant personnel. On te remet le dossier d'un utilisateur : ses frictions récentes (actions proposées jamais confirmées, rappels ou tâches en échec — en métadonnées, jamais leur contenu), ses habitudes observées, et les suggestions qu'on lui a déjà faites avec leur sort.

Ta mission : proposer AU PLUS UNE amélioration concrète que l'assistant pourrait offrir à cet utilisateur.

Règles strictes :
- Ne propose que ce qui s'appuie sur AU MOINS DEUX occurrences observées dans le dossier. Une friction isolée n'est pas un motif.
- Ne répète JAMAIS une suggestion proche d'une suggestion déjà émise, quel que soit son sort — et surtout pas une suggestion écartée (dismissed) : ce refus est une décision de l'utilisateur.
- kind : "automation" (un geste répété qui pourrait devenir une tâche programmée), "activation" (une capacité existante qui résoudrait une friction), "fix" (quelque chose échoue et mérite d'être signalé), "habit" (un ajustement de confort déduit des habitudes).
- push : true UNIQUEMENT pour une friction répétée dont la correction est un gain net et immédiat (une action qui expire à chaque fois, un rappel qui échoue chaque semaine). Les intuitions de confort restent push: false.
- title : une phrase courte et concrète. body : deux ou trois phrases, adressées à l'utilisateur (« vous »), qui disent ce qui a été observé et ce que l'assistant propose. Jamais de jargon technique, jamais d'identifiant.
- Au moindre doute, réponds [].

Réponds UNIQUEMENT par un tableau JSON : [] ou [{"kind": "...", "title": "...", "body": "...", "push": false}]. Aucun commentaire, aucun balisage.`

// MemberNotifier pousse un message dans la conversation privée d'un membre.
// Implémenté par le notifieur du registre.
type MemberNotifier interface {
	NotifyMember(ctx context.Context, memberID, message string) error
}

// Introspector conduit la passe.
type Introspector struct {
	db          *persistence.DB
	client      llm.ChatCompletionClient
	memory      memoryReader
	notifier    MemberNotifier
	suggestions *persistence.SuggestionRepository
	members     *persistence.MemberRepository
	orgs        *persistence.OrganizationRepository
	runs        *persistence.MaintenanceRunRepository
	schedule    cron.Schedule
	logger      *slog.Logger
	now         func() time.Time
	baseURL     string
}

// New construit l'introspecteur. memory et notifier peuvent être nil : la
// passe fonctionne alors sur les seules frictions, et sans push.
func New(db *persistence.DB, client llm.ChatCompletionClient, cfg config.Introspection, baseURL string, logger *slog.Logger) (*Introspector, error) {
	if logger == nil {
		logger = slog.Default()
	}

	spec := cfg.Cron
	if spec == "" {
		spec = defaultCron
	}
	schedule, err := cron.ParseStandard(spec)
	if err != nil {
		return nil, fmt.Errorf("introspection: expression cron %q invalide: %w", spec, err)
	}

	return &Introspector{
		db:          db,
		client:      client,
		suggestions: persistence.NewSuggestionRepository(),
		members:     persistence.NewMemberRepository(),
		orgs:        persistence.NewOrganizationRepository(),
		runs:        persistence.NewMaintenanceRunRepository(),
		schedule:    schedule,
		logger:      logger,
		now:         time.Now,
		baseURL:     strings.TrimRight(baseURL, "/"),
	}, nil
}

// WithMemory branche la lecture des motifs comportementaux.
func (i *Introspector) WithMemory(reader memoryReader) *Introspector {
	i.memory = reader
	return i
}

// WithNotifier branche le push en conversation.
func (i *Introspector) WithNotifier(notifier MemberNotifier) *Introspector {
	i.notifier = notifier
	return i
}

// WithClock remplace l'horloge (tests).
func (i *Introspector) WithClock(now func() time.Time) *Introspector {
	i.now = now
	return i
}

// Run vérifie l'échéance chaque minute jusqu'à l'annulation du contexte.
func (i *Introspector) Run(ctx context.Context) error {
	if err := i.Tick(ctx); err != nil && ctx.Err() == nil {
		i.logger.ErrorContext(ctx, "introspection: échec du tick initial", "error", err)
	}

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := i.Tick(ctx); err != nil && ctx.Err() == nil {
				i.logger.ErrorContext(ctx, "introspection: échec du tick", "error", err)
			}
		}
	}
}

// Tick traite les membres dont l'échéance est passée. L'ancrage est par
// membre : au tout premier passage d'un membre, l'horodatage est posé SANS
// introspection — sans cela, chaque nouveau membre serait introspecté à
// l'instant de son rattachement, sur un dossier vide.
func (i *Introspector) Tick(ctx context.Context) error {
	now := i.now().UTC()

	members, err := i.eligibleMembers(ctx)
	if err != nil {
		return err
	}

	var firstErr error
	for _, member := range members {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := i.tickMember(ctx, member, now); err != nil {
			i.logger.WarnContext(ctx, "introspection: passe en échec pour un membre",
				"org_id", member.OrgID, "member_id", member.ID, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	return firstErr
}

// eligibleMembers liste les membres rattachés et non muets.
func (i *Introspector) eligibleMembers(ctx context.Context) ([]persistence.Member, error) {
	var eligible []persistence.Member
	err := i.db.WithTx(ctx, func(tx *sql.Tx) error {
		orgs, err := i.orgs.List(ctx, tx, "")
		if err != nil {
			return err
		}
		for _, org := range orgs {
			members, err := i.members.ListByOrg(ctx, tx, org.ID)
			if err != nil {
				return err
			}
			for _, member := range members {
				// Un membre muet n'est pas seulement épargné du push : il
				// n'est jamais collecté ni soumis au modèle. « Ne me
				// propose plus rien » veut dire ne plus y penser du tout.
				if !member.Linked() || member.SuggestionsMuted {
					continue
				}
				eligible = append(eligible, member)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("introspection: liste des membres: %w", err)
	}

	return eligible, nil
}

// tickMember vérifie l'échéance d'un membre et le traite le cas échéant.
func (i *Introspector) tickMember(ctx context.Context, member persistence.Member, now time.Time) error {
	task := taskPrefix + member.OrgID + "/" + member.ID

	var (
		lastRun time.Time
		found   bool
	)
	if err := i.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		lastRun, found, err = i.runs.GetLastRun(ctx, tx, task)
		return err
	}); err != nil {
		return err
	}

	if !found {
		// Ancrage initial sans exécution : la première introspection aura
		// lieu à la prochaine échéance, sur une vraie semaine de matière.
		return i.recordRun(ctx, task, now)
	}
	if i.schedule.Next(lastRun).After(now) {
		return nil
	}

	if err := i.introspectMember(ctx, member); err != nil {
		return err
	}

	return i.recordRun(ctx, task, now)
}

func (i *Introspector) recordRun(ctx context.Context, task string, now time.Time) error {
	return i.db.WithTx(ctx, func(tx *sql.Tx) error {
		return i.runs.SetLastRun(ctx, tx, task, now)
	})
}

// suggestionDraft est la réponse attendue du modèle.
type suggestionDraft struct {
	Kind  string `json:"kind"`
	Title string `json:"title"`
	Body  string `json:"body"`
	Push  bool   `json:"push"`
}

// validKinds énumère les natures acceptées : une valeur inventée par le
// modèle invalide la réponse entière.
var validKinds = map[string]bool{
	"automation": true, "activation": true, "fix": true, "habit": true,
}

// introspectMember conduit la passe d'un membre : dossier, modèle,
// enregistrement, et push éventuel.
func (i *Introspector) introspectMember(ctx context.Context, member persistence.Member) error {
	d, err := i.collect(ctx, member)
	if err != nil {
		return err
	}
	if d.empty() {
		// Aucune matière : aucun appel LLM. La parcimonie est ce qui rend
		// la passe gratuite pour l'immense majorité des semaines.
		return nil
	}

	// Comptabilité d'usage : la passe est facturée à l'organisation du
	// membre, comme la consolidation l'est à sa portée.
	callCtx := usage.ContextWithAttribution(ctx, usage.Attribution{
		OrgID:     member.OrgID,
		Component: usage.ComponentIntrospection,
	})

	response, err := i.client.ChatCompletion(callCtx,
		llm.WithMessages(
			llm.NewMessage(llm.RoleSystem, introspectionPrompt),
			llm.NewMessage(llm.RoleUser, renderDossier(d)),
		),
	)
	if err != nil {
		return fmt.Errorf("appel du client llm: %w", err)
	}

	draft, ok, err := parseDraft(response.Message().Content())
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	suggestion := persistence.Suggestion{
		OrgID:     member.OrgID,
		MemberID:  member.ID,
		Kind:      draft.Kind,
		Title:     truncate(draft.Title, maxTitleChars),
		Body:      truncate(draft.Body, maxBodyChars),
		Status:    persistence.SuggestionStatusProposed,
		CreatedAt: i.now().UTC(),
	}
	if suggestion.ID, err = weblink.RandomCrockford(16); err != nil {
		return err
	}

	if err := i.db.WithTx(ctx, func(tx *sql.Tx) error {
		return i.suggestions.Insert(ctx, tx, suggestion)
	}); err != nil {
		return err
	}

	i.logger.InfoContext(ctx, "introspection: suggestion émise",
		"org_id", member.OrgID, "member_id", member.ID,
		"kind", suggestion.Kind, "push", draft.Push)

	if draft.Push && i.notifier != nil {
		i.push(ctx, member, suggestion)
	}

	return nil
}

// push remet la suggestion en conversation. Un échec n'est pas une erreur
// de passe : la suggestion reste visible sur la page de profil.
func (i *Introspector) push(ctx context.Context, member persistence.Member, s persistence.Suggestion) {
	message := "💡 " + s.Title + "\n\n" + s.Body +
		"\n\nRépondez-moi si cela vous intéresse — ou retrouvez mes suggestions dans votre profil, où vous pouvez aussi les désactiver."

	if err := i.notifier.NotifyMember(ctx, member.ID, message); err != nil {
		i.logger.WarnContext(ctx, "introspection: suggestion non poussée",
			"org_id", member.OrgID, "member_id", member.ID, "error", err)
		return
	}

	if err := i.db.WithTx(ctx, func(tx *sql.Tx) error {
		return i.suggestions.MarkDelivered(ctx, tx, s.ID, i.now().UTC())
	}); err != nil {
		i.logger.WarnContext(ctx, "introspection: remise non enregistrée", "error", err)
	}
}

// renderDossier met le dossier en texte pour le modèle.
func renderDossier(d dossier) string {
	var b strings.Builder

	b.WriteString("## Frictions récentes (30 jours)\n")
	if len(d.Frictions) == 0 {
		b.WriteString("(aucune)\n")
	}
	for _, f := range d.Frictions {
		fmt.Fprintf(&b, "- %s : %s (%s)\n", f.Kind, f.Detail, f.At.Format("2006-01-02"))
	}

	b.WriteString("\n## Habitudes observées\n")
	if len(d.Patterns) == 0 {
		b.WriteString("(aucune)\n")
	}
	for _, p := range d.Patterns {
		fmt.Fprintf(&b, "- %s\n", p)
	}

	b.WriteString("\n## Suggestions déjà émises (ne pas répéter)\n")
	if len(d.Previous) == 0 {
		b.WriteString("(aucune)\n")
	}
	for _, p := range d.Previous {
		fmt.Fprintf(&b, "- %s\n", p)
	}

	return b.String()
}

// parseDraft lit la réponse du modèle : [] ou un tableau d'exactement une
// suggestion valide. Tout le reste est rejeté — la passe préfère se taire
// qu'enregistrer une réponse douteuse.
func parseDraft(raw string) (suggestionDraft, bool, error) {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimPrefix(trimmed, "```json")
	trimmed = strings.TrimPrefix(trimmed, "```")
	trimmed = strings.TrimSuffix(trimmed, "```")
	trimmed = strings.TrimSpace(trimmed)

	var drafts []suggestionDraft
	if err := json.Unmarshal([]byte(trimmed), &drafts); err != nil {
		return suggestionDraft{}, false, fmt.Errorf("réponse illisible: %w", err)
	}

	if len(drafts) == 0 {
		return suggestionDraft{}, false, nil
	}
	if len(drafts) > 1 {
		return suggestionDraft{}, false, fmt.Errorf("réponse refusée: %d suggestions, maximum 1", len(drafts))
	}

	draft := drafts[0]
	if !validKinds[draft.Kind] {
		return suggestionDraft{}, false, fmt.Errorf("réponse refusée: nature %q inconnue", draft.Kind)
	}
	if strings.TrimSpace(draft.Title) == "" || strings.TrimSpace(draft.Body) == "" {
		return suggestionDraft{}, false, fmt.Errorf("réponse refusée: titre ou corps vide")
	}

	return draft, true, nil
}

// truncate borne une chaîne en runes.
func truncate(s string, max int) string {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= max {
		return string(runes)
	}
	return string(runes[:max])
}
