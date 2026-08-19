package ingress

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/bornholm/go-courier"

	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/weblink"
)

// linkTokenPattern reconnaît un jeton de liaison dans un message entrant :
// le préfixe « atm_ » suivi de 16 caractères de l'alphabet de Crockford.
// Les séparateurs d'affichage (espaces, points médians, tirets) sont
// retirés avant la recherche : la personne recopie souvent le code tel
// qu'il lui a été transmis, mise en forme comprise.
var linkTokenPattern = regexp.MustCompile(`(?i)atm_[0-9A-HJKMNP-TV-Z]{16}`)

// linkSeparators nettoie la mise en forme d'un jeton copié à la main.
var linkSeparators = strings.NewReplacer(" ", "", " ", "", "·", "", "-", "", "—", "", " ", "", "\t", "")

// findLinkToken extrait un jeton de liaison du texte, s'il y en a un.
func findLinkToken(text string) (string, bool) {
	match := linkTokenPattern.FindString(linkSeparators.Replace(text))
	if match == "" {
		return "", false
	}

	// Le préfixe est écrit en minuscules, le corps en majuscules : c'est la
	// forme produite par weblink.NewLinkToken, et donc celle qui a été
	// hachée.
	return weblink.LinkTokenPrefix + strings.ToUpper(strings.TrimPrefix(strings.ToLower(match), weblink.LinkTokenPrefix)), true
}

// WithLinking active la liaison par jeton sur le pipeline : un expéditeur
// inconnu dont le message porte un jeton valide est rattaché à son compte
// pré-créé (jeton personnel) ou lie son groupe à une organisation (jeton
// de groupe). Sans cet appel, le comportement historique est inchangé : un
// inconnu est simplement ignoré.
func (p *Pipeline) WithLinking(enabled bool) *Pipeline {
	p.linking = enabled
	return p
}

// linkResult décrit l'issue d'une tentative de liaison.
type linkResult struct {
	// Linked est vrai si un rattachement a bien eu lieu.
	Linked bool
	// Reply est le message de bienvenue à renvoyer sur le canal. Rédigé
	// ici, jamais par le modèle : le tour n'a pas encore d'identité
	// résolue, et une confirmation de rattachement doit être exacte.
	Reply string
}

// tryLink tente de consommer un jeton de liaison présent dans le message.
// Retourne (linkResult{}, false, nil) si le message n'en porte pas : le
// pipeline poursuit alors son traitement habituel (message ignoré).
//
// Aucun jeton n'est jamais journalisé, même partiellement : seuls
// l'identifiant de la ligne consommée, le membre et l'organisation le
// sont.
func (p *Pipeline) tryLink(ctx context.Context, msg courier.Message, externalUserID, channelID string, logCtx []any) (linkResult, bool, error) {
	if !p.linking || p.db == nil {
		return linkResult{}, false, nil
	}

	text, err := courier.GetMessageMainContent(ctx, msg)
	if err != nil {
		// Message sans part textuelle (média seul) : rien à chercher.
		return linkResult{}, false, nil
	}

	clear, found := findLinkToken(text)
	if !found {
		return linkResult{}, false, nil
	}

	now := time.Now()
	hash := weblink.HashToken(clear)

	var result linkResult
	err = p.db.WithTx(ctx, func(tx *sql.Tx) error {
		tokens := persistence.NewLinkTokenRepository()

		token, valid, err := tokens.FindPendingByHash(ctx, tx, hash, now)
		if err != nil {
			return err
		}
		if !valid {
			// Jeton inconnu, périmé, révoqué ou déjà consommé : on ne dit
			// pas lequel — un message d'échec précis renseignerait un
			// inconnu sur la validité d'un code.
			result = linkResult{Reply: "Ce code n'est plus valide. Demandez-en un nouveau à la personne qui administre votre accès."}
			return nil
		}

		switch token.Kind {
		case persistence.LinkTokenKindPersonal:
			result, err = p.linkPersonal(ctx, tx, token, externalUserID, channelID, msg, now)
		case persistence.LinkTokenKindGroup:
			result, err = p.linkGroup(ctx, tx, token, channelID, msg, now)
		default:
			return fmt.Errorf("ingress: nature de jeton inconnue %q", token.Kind)
		}
		if err != nil {
			return err
		}

		if !result.Linked {
			return nil
		}

		consumed, err := tokens.MarkUsed(ctx, tx, token.ID, now)
		if err != nil {
			return err
		}
		if !consumed {
			// Course entre deux messages portant le même jeton : la
			// transaction perdante n'écrit rien.
			return fmt.Errorf("ingress: jeton %q déjà consommé", token.ID)
		}

		p.logger.InfoContext(ctx, "ingress: liaison par jeton effectuée",
			append(logCtx, "token_id", token.ID, "token_kind", token.Kind, "org_id", token.OrgID, "member_id", token.MemberID)...)

		return nil
	})
	if err != nil {
		return linkResult{}, true, err
	}

	return result, true, nil
}

// linkPersonal rattache l'identité de messagerie au membre pré-créé, et
// lie le canal privé d'où vient le message.
func (p *Pipeline) linkPersonal(ctx context.Context, tx *sql.Tx, token persistence.LinkToken, externalUserID, channelID string, msg courier.Message, now time.Time) (linkResult, error) {
	members := persistence.NewMemberRepository()

	member, found, err := members.FindByID(ctx, tx, token.MemberID)
	if err != nil || !found {
		return linkResult{}, err
	}

	// Une identité de messagerie déjà rattachée à quelqu'un d'autre ne
	// change jamais de propriétaire sur simple présentation d'un jeton.
	existing, taken, err := members.FindByExternalUser(ctx, tx, p.providerName, externalUserID)
	if err != nil {
		return linkResult{}, err
	}
	if taken && existing.ID != member.ID {
		return linkResult{Reply: "Ce compte de messagerie est déjà rattaché à un autre profil. Contactez la personne qui administre votre accès."}, nil
	}

	member.Provider = p.providerName
	member.ExternalUserID = externalUserID
	member.LinkedAt = now
	member.UpdatedAt = now
	if err := members.Update(ctx, tx, member); err != nil {
		return linkResult{}, err
	}

	// Le canal d'où vient le message est lié dès qu'il s'agit d'une
	// conversation directe. Se fier à l'égalité entre l'expéditeur et le
	// canal ne vaut que pour les messageries qui identifient un tête-à-tête
	// par la personne (WhatsApp, REST) : Rocket.Chat, Discord ou le
	// courriel donnent au privé un identifiant de salon distinct, et la
	// conversation restait alors inconnue malgré le rattachement.
	if msg.Channel().Kind() == courier.ChannelKindDirect || channelID == externalUserID {
		bindings := persistence.NewChannelBindingRepository()
		if err := bindings.Upsert(ctx, tx, persistence.ChannelBinding{
			Provider:    p.providerName,
			ChannelID:   channelID,
			OrgID:       member.OrgID,
			Kind:        string(model.ChannelPrivate),
			Scope:       string(model.ScopePersonal),
			ScopeID:     member.ID,
			MemberID:    member.ID,
			DisplayName: member.DisplayName,
			CreatedAt:   now,
		}); err != nil {
			return linkResult{}, err
		}
	}

	return linkResult{
		Linked: true,
		Reply: "Bonjour " + member.DisplayName + " ! Votre compte est rattaché, je suis à votre disposition. " +
			"Écrivez-moi comme à quelqu'un : je peux retenir ce qui compte, vous rappeler des choses et chercher pour vous.",
	}, nil
}

// linkGroup rattache le canal de groupe d'où vient le message à
// l'organisation du jeton.
func (p *Pipeline) linkGroup(ctx context.Context, tx *sql.Tx, token persistence.LinkToken, channelID string, msg courier.Message, now time.Time) (linkResult, error) {
	orgs := persistence.NewOrganizationRepository()

	org, found, err := orgs.FindByID(ctx, tx, token.OrgID)
	if err != nil || !found {
		return linkResult{}, err
	}

	displayName := msg.Channel().Name()
	if displayName == "" {
		displayName = "Groupe"
	}

	bindings := persistence.NewChannelBindingRepository()
	if err := bindings.Upsert(ctx, tx, persistence.ChannelBinding{
		Provider:    p.providerName,
		ChannelID:   channelID,
		OrgID:       org.ID,
		Kind:        string(model.ChannelGroup),
		Scope:       string(model.ScopeGroup),
		ScopeID:     channelID,
		DisplayName: displayName,
		CreatedAt:   now,
	}); err != nil {
		return linkResult{}, err
	}

	return linkResult{
		Linked: true,
		Reply: "Cette conversation est maintenant rattachée à « " + org.DisplayName + " ». " +
			"Mentionnez-moi quand vous avez besoin de moi — je ne lis que ce qui m'est adressé.",
	}, nil
}
