// Package privacy rassemble ce qu'Automata détient sur une personne, et
// sait l'effacer — l'export et la suppression que le RGPD rend exigibles.
//
// Deux principes gouvernent ce paquet, et expliquent ce qu'il ne fait pas :
//
//   - une conversation de groupe appartient à ses participants, pas à un
//     seul : effacer un membre n'efface jamais ce que les autres ont écrit,
//     ni les échanges du groupe ;
//   - les traces de consommation servent de pièces comptables : elles sont
//     conservées mais dissociées de la personne (le principal disparaît,
//     les montants restent), plutôt que supprimées.
package privacy

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/bornholm/automata/internal/memory"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
)

// Export est l'ensemble des données rattachées à une personne, dans une
// forme lisible et portable.
type Export struct {
	GeneratedAt  time.Time         `json:"genere_le"`
	Member       ExportMember      `json:"compte"`
	Channels     []ExportChannel   `json:"conversations"`
	Messages     []ExportMessage   `json:"messages"`
	Memories     []ExportMemory    `json:"souvenirs"`
	Reminders    []ExportReminder  `json:"rappels"`
	Usage        []ExportUsage     `json:"consommation"`
	Explanations map[string]string `json:"a_propos_de_cet_export"`
}

type ExportMember struct {
	DisplayName  string     `json:"nom_affiche"`
	Organization string     `json:"organisation"`
	Role         string     `json:"role"`
	Email        string     `json:"courriel_de_recuperation,omitempty"`
	LinkedAt     *time.Time `json:"rattache_le,omitempty"`
	CreatedAt    time.Time  `json:"cree_le"`
}

type ExportChannel struct {
	Platform string `json:"plateforme"`
	Name     string `json:"nom"`
	Kind     string `json:"type"`
}

type ExportMessage struct {
	At      time.Time `json:"le"`
	Channel string    `json:"conversation"`
	Role    string    `json:"auteur"`
	Content string    `json:"contenu"`
}

type ExportMemory struct {
	At      time.Time `json:"retenu_le"`
	Content string    `json:"contenu"`
	Origin  string    `json:"origine"`
}

type ExportReminder struct {
	DueAt   time.Time `json:"echeance"`
	Message string    `json:"message"`
	Status  string    `json:"etat"`
}

type ExportUsage struct {
	Month   string `json:"mois"`
	Calls   int64  `json:"appels"`
	Tokens  int64  `json:"tokens"`
	Credits int64  `json:"credits"`
}

// Service produit les exports et applique les suppressions.
type Service struct {
	db     *persistence.DB
	memory memory.Store
	// creditRate convertit un coût mesuré en crédits pour l'export.
	creditRate func(ctx context.Context, q persistence.Querier) float64
	// personalChannels énumère les conversations privées d'un membre.
	// Fournie par l'appelant, qui seul connaît les canaux déclarés en
	// configuration en plus de ceux liés par jeton.
	personalChannels func(memberID string) []string

	members       *persistence.MemberRepository
	organizations *persistence.OrganizationRepository
	messages      *persistence.MessageRepository
	usage         *persistence.UsageRecordRepository
	profileLinks  *persistence.ProfileLinkRepository
	linkTokens    *persistence.LinkTokenRepository
}

// WithPersonalChannels fournit l'énumération des conversations privées
// d'un membre. Sans elle, seules les conversations liées en ligne sont
// couvertes — les canaux déclarés en configuration seraient oubliés, et
// un export incomplet vaut une réponse fausse.
func (s *Service) WithPersonalChannels(fn func(memberID string) []string) *Service {
	s.personalChannels = fn
	return s
}

// New construit le service. store peut être nil (mémoire non configurée) :
// l'export ne portera alors aucun souvenir.
func New(db *persistence.DB, store memory.Store, creditRate func(ctx context.Context, q persistence.Querier) float64) *Service {
	return &Service{
		db:            db,
		memory:        store,
		creditRate:    creditRate,
		members:       persistence.NewMemberRepository(),
		organizations: persistence.NewOrganizationRepository(),
		messages:      persistence.NewMessageRepository(db.Cipher()),
		usage:         persistence.NewUsageRecordRepository(),
		profileLinks:  persistence.NewProfileLinkRepository(),
		linkTokens:    persistence.NewLinkTokenRepository(),
	}
}

// explanations décrit en français simple ce que l'export contient et ce
// qu'il ne contient pas : un export muet sur ses limites vaut une réponse
// incomplète.
func explanations() map[string]string {
	return map[string]string{
		"messages":     "Vos messages dans vos conversations privées avec Automata. Les échanges de groupe ne figurent pas ici : ils appartiennent à leurs participants.",
		"souvenirs":    "Ce qu'Automata a retenu de vous, dans votre portée personnelle.",
		"consommation": "Le détail de votre usage, mois par mois. Ces chiffres servent de pièces comptables et sont conservés même après une suppression, sans vous être rattachés.",
		"suppression":  "Une demande de suppression efface vos messages privés, vos souvenirs personnels, vos rappels et votre adresse de récupération, et détache votre compte de votre messagerie.",
	}
}

// Export rassemble les données d'un membre.
func (s *Service) Export(ctx context.Context, memberID string) (Export, error) {
	export := Export{GeneratedAt: time.Now().UTC(), Explanations: explanations()}

	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		member, found, err := s.members.FindByID(ctx, tx, memberID)
		if err != nil || !found {
			return err
		}

		export.Member = ExportMember{
			DisplayName: member.DisplayName,
			Role:        member.Role,
			Email:       member.Email,
			CreatedAt:   member.CreatedAt,
		}
		if !member.LinkedAt.IsZero() {
			linkedAt := member.LinkedAt
			export.Member.LinkedAt = &linkedAt
		}

		if org, found, err := s.organizations.FindByID(ctx, tx, member.OrgID); err == nil && found {
			export.Member.Organization = org.DisplayName
		}

		// Messages des conversations privées de la personne. La portée
		// personnelle est la seule dont elle soit l'unique propriétaire.
		for _, conversationID := range s.privateConversations(ctx, tx, member) {
			messages, err := s.messages.ListRecentByConversation(ctx, tx, model.ConversationID(conversationID), 10000)
			if err != nil {
				return err
			}
			for _, message := range messages {
				at, _ := time.Parse(time.RFC3339, message.CreatedAt)
				export.Messages = append(export.Messages, ExportMessage{
					At:      at,
					Channel: "conversation privée",
					Role:    message.Role,
					Content: message.Content,
				})
			}
		}

		// Consommation mensuelle, en crédits.
		rate := 0.001
		if s.creditRate != nil {
			rate = s.creditRate(ctx, tx)
		}
		aggregates, err := s.usage.AggregateUsage(ctx, tx, time.Time{}, time.Now().AddDate(1, 0, 0),
			[]string{"month"}, persistence.UsageFilter{PrincipalID: memberID})
		if err != nil {
			return err
		}
		for _, agg := range aggregates {
			export.Usage = append(export.Usage, ExportUsage{
				Month:   agg.Keys[0],
				Calls:   agg.Calls,
				Tokens:  agg.TotalTokens,
				Credits: int64(agg.CostAmount / rate),
			})
		}

		return nil
	})
	if err != nil {
		return Export{}, fmt.Errorf("privacy: export du membre %q: %w", memberID, err)
	}

	// Souvenirs de portée personnelle : ils vivent dans la mémoire
	// (amoxtli), hors de la base applicative.
	if s.memory != nil {
		memories, err := s.memory.List(ctx)
		if err == nil {
			for _, m := range memories {
				if !isPersonalMemoryOf(m, memberID) {
					continue
				}
				export.Memories = append(export.Memories, ExportMemory{
					At:      m.CreatedAt,
					Content: m.Content,
					Origin:  m.Metadata["origin"],
				})
			}
		}
	}

	return export, nil
}

// DeletionReport dit ce qui a été effacé.
type DeletionReport struct {
	Messages int
	Memories int
	Tokens   int
	Links    int
}

// Delete efface les données personnelles d'un membre : messages privés,
// souvenirs de sa portée personnelle, jetons et liens, adresse de
// récupération, et rattachement à sa messagerie. Le compte lui-même est
// conservé sous une identité neutre — le supprimer romprait les
// conversations de groupe auxquelles il a participé — et les traces de
// consommation sont dissociées plutôt qu'effacées.
func (s *Service) Delete(ctx context.Context, memberID string) (DeletionReport, error) {
	var report DeletionReport

	// La mémoire d'abord : elle vit hors transaction SQL, et un échec ne
	// doit pas laisser croire que tout a été effacé.
	if s.memory != nil {
		memories, err := s.memory.List(ctx)
		if err != nil {
			return report, fmt.Errorf("privacy: lecture des souvenirs: %w", err)
		}
		for _, m := range memories {
			if !isPersonalMemoryOf(m, memberID) {
				continue
			}
			if err := s.memory.Forget(ctx, m.ID); err != nil {
				return report, fmt.Errorf("privacy: oubli du souvenir %q: %w", m.ID, err)
			}
			report.Memories++
		}
	}

	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		member, found, err := s.members.FindByID(ctx, tx, memberID)
		if err != nil || !found {
			return err
		}

		// Messages des conversations privées, et les historiques compactés
		// qui en dérivent.
		for _, conversationID := range s.privateConversations(ctx, tx, member) {
			result, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE conversation_id = ?`, conversationID)
			if err != nil {
				return fmt.Errorf("suppression des messages: %w", err)
			}
			if n, err := result.RowsAffected(); err == nil {
				report.Messages += int(n)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM conversation_summaries WHERE conversation_id = ?`, conversationID); err != nil {
				return fmt.Errorf("suppression des résumés: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM reminders WHERE principal_id = ?`, memberID); err != nil {
			return fmt.Errorf("suppression des rappels: %w", err)
		}

		// Jetons et liens : ils permettraient de retrouver ou rouvrir le
		// compte.
		result, err := tx.ExecContext(ctx, `DELETE FROM link_tokens WHERE member_id = ?`, memberID)
		if err != nil {
			return fmt.Errorf("suppression des jetons: %w", err)
		}
		if n, err := result.RowsAffected(); err == nil {
			report.Tokens = int(n)
		}
		result, err = tx.ExecContext(ctx, `DELETE FROM profile_links WHERE member_id = ?`, memberID)
		if err != nil {
			return fmt.Errorf("suppression des liens de profil: %w", err)
		}
		if n, err := result.RowsAffected(); err == nil {
			report.Links = int(n)
		}

		// La liaison de canal privé : sans elle, la personne redevient une
		// inconnue pour l'ingress.
		if _, err := tx.ExecContext(ctx, `DELETE FROM channel_bindings WHERE member_id = ?`, memberID); err != nil {
			return fmt.Errorf("suppression des liaisons de canal: %w", err)
		}

		// Le magasin d'objets des plugins et ses publications : des pages
		// PUBLIQUES ne doivent pas survivre à l'effacement de leur
		// propriétaire.
		if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_objects WHERE member_id = ?`, memberID); err != nil {
			return fmt.Errorf("suppression des objets de plugins: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_public_sites WHERE member_id = ?`, memberID); err != nil {
			return fmt.Errorf("suppression des publications de plugins: %w", err)
		}

		// Traces de consommation : dissociées, jamais supprimées — ce sont
		// des pièces comptables.
		if _, err := tx.ExecContext(ctx, `UPDATE usage_records SET principal_id = '', conversation_id = ''
			WHERE principal_id = ?`, memberID); err != nil {
			return fmt.Errorf("dissociation des traces d'usage: %w", err)
		}

		// Le compte devient anonyme, et se détache de la messagerie.
		member.DisplayName = "Compte supprimé"
		member.Email = ""
		member.EmailVerifiedAt = time.Time{}
		member.Provider = ""
		member.ExternalUserID = ""
		member.LinkedAt = time.Time{}
		member.UpdatedAt = time.Now()

		return s.members.Update(ctx, tx, member)
	})
	if err != nil {
		return report, fmt.Errorf("privacy: suppression des données du membre %q: %w", memberID, err)
	}

	return report, nil
}

// isPersonalMemoryOf reconnaît un souvenir de la portée personnelle d'un
// membre. La portée voyage dans les métadonnées du store (voir
// internal/memory.AmoxtliStore) : c'est elle qui garantit qu'on n'exporte
// ni n'efface le souvenir d'un groupe ou de quelqu'un d'autre.
func isPersonalMemoryOf(m memory.Memory, memberID string) bool {
	return m.Metadata["scope"] == string(model.ScopePersonal) && m.Metadata["scope_id"] == memberID
}

// privateConversations énumère les conversations privées d'un membre :
// celles liées en ligne (channel_bindings), celles déclarées en
// configuration (via personalChannels), et celle que son identité de
// messagerie désigne directement.
func (s *Service) privateConversations(ctx context.Context, tx *sql.Tx, member persistence.Member) []string {
	seen := map[string]struct{}{}
	var conversations []string

	add := func(id string) {
		if id == "" || id == ":" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		conversations = append(conversations, id)
	}

	if member.Provider != "" && member.ExternalUserID != "" {
		add(member.Provider + ":" + member.ExternalUserID)
	}

	rows, err := tx.QueryContext(ctx, `SELECT provider, channel_id FROM channel_bindings
		WHERE member_id = ? AND kind = 'private'`, member.ID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var provider, channelID string
			if err := rows.Scan(&provider, &channelID); err == nil {
				add(provider + ":" + channelID)
			}
		}
	}

	if s.personalChannels != nil {
		for _, id := range s.personalChannels(member.ID) {
			add(id)
		}
	}

	return conversations
}
