// Package onboarding conduit la visite d'accueil : les quelques échanges
// qui suivent le rattachement d'un membre, où l'assistant se présente et
// apprend à le connaître.
//
// La visite tient dans la conversation, pas dans une interface : la personne
// vient d'écrire sur sa messagerie, l'y renvoyer ailleurs pour « configurer
// son profil » perdrait la moitié des arrivants. Chaque réponse devient un
// souvenir personnel ordinaire (Origin « onboarding ») — donc utile dès le
// tour suivant, consultable et effaçable comme n'importe quel autre.
//
// Deux règles président à l'écriture de ce paquet :
//
//   - Une visite se quitte. « passe », « plus tard », ou simplement une vraie
//     question posée à la place d'une réponse, y mettent fin et rendent la
//     main à l'assistant. Un questionnaire dont on ne sort pas n'accueille
//     personne, il retient.
//   - Une visite ne se répète pas. L'état vit sur le membre : écartée une
//     fois, elle ne revient pas sur un autre canal.
package onboarding

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bornholm/automata/internal/memory"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
)

// États de la visite, tels qu'ils sont persistés (members.onboarding_state).
const (
	// StateNone : jamais proposée.
	StateNone = ""
	// StateOffered : proposée, en attente d'un oui.
	StateOffered = "offered"
	// StateDone : terminée.
	StateDone = "done"
	// StateSkipped : écartée par la personne, définitivement.
	StateSkipped = "skipped"
)

// Origin marque les souvenirs issus de la visite. Il permet de les
// distinguer d'un fait extrait d'une conversation, et de les retrouver.
const Origin = "onboarding"

// Step est une étape de la visite : une question, et la façon d'en garder
// la réponse.
type Step struct {
	// State est la valeur persistée pendant que cette question est en
	// attente de réponse.
	State string
	// Question part vers la personne : en français, comme tout ce qui
	// s'écrit dans la conversation.
	Question string
	// Fact préfixe la réponse dans le souvenir. En anglais : un souvenir
	// est relu par le modèle.
	Fact string
}

// steps est la visite elle-même. Quatre questions, pas davantage : au-delà,
// ce n'est plus une prise de contact mais un formulaire, et les gens
// décrochent. Chacune sert vraiment aux tours suivants — rien n'est demandé
// « pour la forme ».
var steps = []Step{
	{
		State:    "q1",
		Question: "Comment préférez-vous que je vous appelle ?",
		Fact:     "Preferred name to address the user:",
	},
	{
		State: "q2",
		Question: "Dans quel fuseau horaire vivez-vous, et à quelles heures êtes-vous " +
			"généralement disponible ? Cela m'évitera de vous proposer un rappel à 3 h du matin.",
		Fact: "Time zone and usual availability:",
	},
	{
		State: "q3",
		Question: "Sur quoi travaillez-vous, en quelques mots ? Je saurai de quoi vous " +
			"parlez quand vous mentionnerez un projet ou un dossier.",
		Fact: "What the user works on:",
	},
	{
		State: "q4",
		Question: "Dernière question : préférez-vous des réponses brèves qui vont droit " +
			"au but, ou plus détaillées ?",
		Fact: "Preferred answer style:",
	},
}

// MemoryWriter est ce que la visite attend de la mémoire. L'interface
// restreint volontairement memory.Store à son écriture : la visite n'a
// jamais à relire quoi que ce soit.
type MemoryWriter interface {
	Remember(ctx context.Context, mem memory.NewMemory) (memory.Memory, error)
}

// Service conduit la visite.
type Service struct {
	db      *persistence.DB
	members *persistence.MemberRepository
	memory  MemoryWriter
	logger  *slog.Logger
	now     func() time.Time
}

// New construit le service. memory peut être nil : la visite se déroule
// alors sans rien retenir — dégradée, mais toujours accueillante.
func New(db *persistence.DB, mem MemoryWriter, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}

	return &Service{
		db:      db,
		members: persistence.NewMemberRepository(),
		memory:  mem,
		logger:  logger,
		now:     time.Now,
	}
}

// WithClock remplace l'horloge (tests).
func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

// Offer marque la visite comme proposée et retourne l'invitation à joindre
// au message de bienvenue. Appelé au rattachement du membre.
func Offer() string {
	return "Voulez-vous qu'on fasse connaissance en quatre questions ? " +
		"Répondez « oui » — ou posez-moi directement ce dont vous avez besoin, " +
		"on fera connaissance en chemin."
}

// Handle traite un message entrant du point de vue de la visite.
//
// handled=true signifie que la visite a répondu et que l'assistant ne doit
// pas être consulté pour ce tour. handled=false rend la main : soit il n'y a
// pas de visite en cours, soit la personne vient d'en sortir — et son
// message doit alors être traité normalement, pas perdu.
func (s *Service) Handle(ctx context.Context, identity model.ExecutionIdentity, text string) (reply string, handled bool, err error) {
	memberID := string(identity.PrincipalID)
	if memberID == "" {
		return "", false, nil
	}

	var member persistence.Member
	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		var found bool
		var err error
		member, found, err = s.members.FindByID(ctx, tx, memberID)
		if err != nil {
			return err
		}
		if !found {
			member = persistence.Member{}
		}
		return nil
	})
	if err != nil {
		return "", false, fmt.Errorf("onboarding: lecture du membre: %w", err)
	}

	switch member.OnboardingState {
	case StateNone, StateDone, StateSkipped:
		return "", false, nil
	case StateOffered:
		return s.handleOffer(ctx, member, text)
	default:
		return s.handleAnswer(ctx, identity, member, text)
	}
}

// handleOffer traite la réponse à l'invitation.
func (s *Service) handleOffer(ctx context.Context, member persistence.Member, text string) (string, bool, error) {
	if !accepts(text) {
		// Tout ce qui n'est pas un oui franc vaut refus, et le message part
		// à l'assistant : quelqu'un qui répond par sa vraie question veut
		// une réponse, pas un questionnaire.
		if err := s.setState(ctx, member.ID, StateSkipped); err != nil {
			return "", false, err
		}
		return "", false, nil
	}

	if err := s.setState(ctx, member.ID, steps[0].State); err != nil {
		return "", false, err
	}

	return steps[0].Question, true, nil
}

// handleAnswer enregistre une réponse et enchaîne.
func (s *Service) handleAnswer(ctx context.Context, identity model.ExecutionIdentity, member persistence.Member, text string) (string, bool, error) {
	index := stepIndex(member.OnboardingState)
	if index < 0 {
		// État inconnu (visite raccourcie entre deux versions) : ne jamais
		// bloquer quelqu'un sur une étape qui n'existe plus.
		if err := s.setState(ctx, member.ID, StateDone); err != nil {
			return "", false, err
		}
		return "", false, nil
	}

	answer := strings.TrimSpace(text)
	if answer == "" || quits(answer) {
		if err := s.setState(ctx, member.ID, StateSkipped); err != nil {
			return "", false, err
		}
		if quits(answer) {
			return "Entendu, on s'arrête là. Je suis à votre disposition quand vous voulez.", true, nil
		}
		return "", false, nil
	}

	// Une vraie question posée à la place d'une réponse : la personne a
	// besoin d'autre chose. La visite s'efface, et son message part à
	// l'assistant plutôt que d'être rangé comme un souvenir absurde.
	if asksSomethingElse(answer) {
		if err := s.setState(ctx, member.ID, StateSkipped); err != nil {
			return "", false, err
		}
		return "", false, nil
	}

	s.remember(ctx, identity, steps[index].Fact, answer)

	if index+1 < len(steps) {
		if err := s.setState(ctx, member.ID, steps[index+1].State); err != nil {
			return "", false, err
		}
		return steps[index+1].Question, true, nil
	}

	if err := s.setState(ctx, member.ID, StateDone); err != nil {
		return "", false, err
	}

	return "Merci, je note tout ça. Vous pouvez maintenant me demander ce que vous voulez : " +
		"retenir quelque chose, vous rappeler une échéance, travailler sur un fichier que vous " +
		"m'envoyez, ou garder un document au chaud. Dites-moi simplement ce dont vous avez besoin.", true, nil
}

// remember range la réponse en mémoire personnelle. Un échec n'interrompt
// pas la visite : perdre un souvenir est regrettable, interrompre l'accueil
// de quelqu'un l'est davantage.
func (s *Service) remember(ctx context.Context, identity model.ExecutionIdentity, fact, answer string) {
	if s.memory == nil {
		return
	}

	if _, err := s.memory.Remember(ctx, memory.NewMemory{
		Content:              fact + " " + answer,
		Scope:                identity.Scope,
		ScopeID:              identity.ScopeID,
		OrgID:                identity.OrgID,
		OwnerPrincipalID:     identity.PrincipalID,
		CreatedBy:            identity.PrincipalID,
		SourceConversationID: identity.ConversationID,
		Origin:               Origin,
	}); err != nil {
		// Le contenu de la réponse ne va PAS dans les journaux : c'est une
		// information personnelle, au même titre qu'un message.
		s.logger.WarnContext(ctx, "onboarding: souvenir non enregistré",
			"org_id", identity.OrgID, "principal_id", identity.PrincipalID, "error", err)
	}
}

func (s *Service) setState(ctx context.Context, memberID, state string) error {
	if err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		return s.members.SetOnboardingState(ctx, tx, memberID, state)
	}); err != nil {
		return fmt.Errorf("onboarding: enregistrement de l'état: %w", err)
	}

	return nil
}

// stepIndex retrouve l'étape correspondant à un état, ou -1.
func stepIndex(state string) int {
	for i, step := range steps {
		if step.State == state {
			return i
		}
	}
	return -1
}

// acceptWords reconnaît un oui. La liste est courte et volontairement
// littérale : dans le doute, on rend la main plutôt que d'imposer la visite.
var acceptWords = []string{"oui", "ok", "d'accord", "daccord", "volontiers", "go", "yes", "allons-y", "c'est parti"}

func accepts(text string) bool {
	normalized := strings.ToLower(strings.TrimSpace(text))
	normalized = strings.Trim(normalized, " .!…")
	if normalized == "" {
		return false
	}

	for _, word := range acceptWords {
		if normalized == word {
			return true
		}
	}

	// « oui, avec plaisir » : un oui qui s'étoffe reste un oui, mais on
	// n'accepte le préfixe que sur une phrase courte — « oui mais d'abord,
	// peux-tu… » n'est pas une acceptation.
	if len(normalized) <= 30 {
		for _, word := range acceptWords {
			if strings.HasPrefix(normalized, word+" ") || strings.HasPrefix(normalized, word+",") {
				return true
			}
		}
	}

	return false
}

// quitWords reconnaît une sortie explicite.
var quitWords = []string{"passe", "passer", "plus tard", "stop", "non", "annuler", "arrête", "arrete", "skip"}

func quits(text string) bool {
	normalized := strings.ToLower(strings.TrimSpace(text))
	normalized = strings.Trim(normalized, " .!…")

	for _, word := range quitWords {
		if normalized == word {
			return true
		}
	}

	return false
}

// asksSomethingElse distingue une vraie question d'une réponse qui contient
// un point d'interrogation.
//
// C'est une heuristique, et elle est assumée comme telle : elle se trompe au
// pire en écourtant la visite, jamais en retenant quelqu'un qui veut autre
// chose. Une réponse courte reste une réponse — « Paris, plutôt le soir ? »
// répond bien à la question posée ; une longue phrase interrogative, non.
func asksSomethingElse(text string) bool {
	return strings.Contains(text, "?") && len([]rune(text)) > 60
}
