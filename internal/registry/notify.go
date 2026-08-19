package registry

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/bornholm/go-courier"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
)

// orgNotifier porte les alertes de facturation jusqu'à la conversation
// d'une organisation. Le message est rédigé par l'application, jamais par
// le modèle : une alerte de solde doit être exacte, et elle part sans
// qu'aucun humain n'ait écrit — ce n'est pas un tour de conversation.
type orgNotifier struct {
	db       *persistence.DB
	cfg      *config.Config
	senders  senderLookup
	links    profileLinkGenerator
	logger   *slog.Logger
	bindings *persistence.ChannelBindingRepository
	members  *persistence.MemberRepository
}

// senderLookup donne accès aux fournisseurs par nom de compte.
type senderLookup interface {
	Get(name string) (courier.Provider, bool)
}

// profileLinkGenerator produit le lien de recharge à insérer dans
// l'alerte.
type profileLinkGenerator interface {
	GenerateProfileLink(ctx context.Context, orgID, principalID string) (string, bool, error)
}

// newOrgNotifier construit le notifieur.
func newOrgNotifier(db *persistence.DB, cfg *config.Config, senders senderLookup, links profileLinkGenerator, logger *slog.Logger) *orgNotifier {
	return &orgNotifier{
		db:       db,
		cfg:      cfg,
		senders:  senders,
		links:    links,
		logger:   logger,
		bindings: persistence.NewChannelBindingRepository(),
		members:  persistence.NewMemberRepository(),
	}
}

// NotifyLowBalance implémente billing.Notifier : il prévient le
// responsable de l'organisation, ou à défaut son groupe.
func (n *orgNotifier) NotifyLowBalance(ctx context.Context, orgID string, balance int64) error {
	target, member, err := n.destination(ctx, orgID)
	if err != nil {
		return err
	}
	if target.provider == "" {
		return fmt.Errorf("aucun canal connu pour l'organisation %q", orgID)
	}

	provider, ok := n.senders.Get(target.provider)
	if !ok {
		return fmt.Errorf("compte de messagerie %q indisponible", target.provider)
	}

	message := fmt.Sprintf(
		"Vos crédits Automata s'épuisent : il en reste %d. Je continue de répondre, mais je m'arrêterai quand ils seront à zéro.",
		balance)

	// Le lien de recharge n'est proposé qu'à une personne identifiée : un
	// lien de profil ouvre un compte, il n'a rien à faire dans un groupe.
	if member != "" && n.links != nil {
		if url, ok, err := n.links.GenerateProfileLink(ctx, orgID, member); err == nil && ok {
			message += "\n\nPour recharger : " + url + "\n(lien valable un quart d'heure, à usage unique)"
		}
	} else {
		message += "\n\nDemandez-moi « mes crédits » et je vous ouvre la page de recharge."
	}

	outgoing := courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannelRef(courier.ChannelID(target.channelID)),
		courier.NewUser("automata", "Automata"),
		courier.WithMessageMainPart(message),
	)

	if err := provider.Send(ctx, outgoing); err != nil {
		return fmt.Errorf("envoi de l'alerte de solde: %w", err)
	}

	return nil
}

// destination désigne où envoyer l'alerte : la conversation privée du
// responsable si l'organisation en a un, sinon son premier canal connu.
type destination struct {
	provider  string
	channelID string
}

func (n *orgNotifier) destination(ctx context.Context, orgID string) (destination, string, error) {
	var (
		target   destination
		memberID string
	)

	err := n.db.WithTx(ctx, func(tx *sql.Tx) error {
		members, err := n.members.ListByOrg(ctx, tx, orgID)
		if err != nil {
			return err
		}

		// Un responsable d'abord : c'est lui qui peut payer.
		var fallback persistence.Member
		for _, member := range members {
			if !member.Linked() {
				continue
			}
			if member.Role == persistence.MemberRoleOwner {
				target = destination{member.Provider, member.ExternalUserID}
				memberID = member.ID
				return nil
			}
			if fallback.ID == "" {
				fallback = member
			}
		}
		if fallback.ID != "" {
			target = destination{fallback.Provider, fallback.ExternalUserID}
			memberID = fallback.ID
			return nil
		}

		// Aucun membre rattaché : on se rabat sur un canal de
		// l'organisation, liaison en ligne d'abord.
		bindings, err := n.bindings.ListByOrg(ctx, tx, orgID)
		if err != nil {
			return err
		}
		if len(bindings) > 0 {
			target = destination{bindings[0].Provider, bindings[0].ChannelID}
			return nil
		}

		for _, ch := range n.cfg.Channels {
			if ch.OrgID == orgID {
				target = destination{ch.Provider, ch.ChannelID}
				return nil
			}
		}

		return nil
	})
	if err != nil {
		return destination{}, "", fmt.Errorf("recherche du destinataire de %q: %w", orgID, err)
	}

	// Le nom du compte de messagerie et le type de provider ne coïncident
	// pas forcément : les canaux de la configuration nomment le compte.
	if target.provider != "" {
		if _, ok := n.cfg.Courier.Providers[target.provider]; !ok {
			for name, provider := range n.cfg.Courier.Providers {
				if provider.Type == target.provider {
					target.provider = name
					break
				}
			}
		}
	}

	return target, memberID, nil
}
