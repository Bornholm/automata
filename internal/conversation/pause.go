package conversation

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
)

// pauseNoticeInterval borne la fréquence du message de pause sur une même
// conversation : quelqu'un qui insiste ne doit pas recevoir la même
// explication à chaque phrase.
const pauseNoticeInterval = time.Hour

// ProfileLinkGenerator produit un lien de profil pour proposer la recharge
// dans le message de pause. Même interface que celle des outils d'agent ;
// nil = message sans lien (l'utilisateur devra demander à l'assistant… qui
// est justement en pause, d'où l'intérêt de la câbler).
type ProfileLinkGenerator interface {
	GenerateProfileLink(ctx context.Context, orgID, principalID string) (string, bool, error)
}

// WithBilling active la pause du service à solde épuisé. Sans cet appel,
// aucune conversation n'est jamais interrompue pour raison de crédits — le
// comportement des instances non facturées.
func (h *Handler) WithBilling(links ProfileLinkGenerator) *Handler {
	h.wallet = persistence.NewWalletRepository()
	h.orgs = persistence.NewOrganizationRepository()
	h.profileLinks = links
	h.pauseNotices = &pauseNoticeTracker{last: map[model.ConversationID]time.Time{}}
	return h
}

// pauseNoticeTracker retient la dernière notice envoyée par conversation.
type pauseNoticeTracker struct {
	mu   sync.Mutex
	last map[model.ConversationID]time.Time
}

// shouldNotify indique s'il faut (re)dire que le service est en pause.
func (t *pauseNoticeTracker) shouldNotify(conversationID model.ConversationID, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if last, ok := t.last[conversationID]; ok && now.Sub(last) < pauseNoticeInterval {
		return false
	}
	t.last[conversationID] = now

	return true
}

// pausedReply retourne le message à envoyer si l'organisation n'a plus de
// crédits, et true dans ce cas. Une organisation sans portefeuille (aucun
// mouvement : instance non facturée, tenant encore purement configuré)
// n'est jamais mise en pause — la facturation ne doit pas s'inviter là où
// elle n'a pas été mise en place.
func (h *Handler) pausedReply(ctx context.Context, identity model.ExecutionIdentity) (string, bool) {
	if h.wallet == nil {
		return "", false
	}

	var (
		balance   int64
		hasWallet bool
		unlimited bool
	)
	err := h.db.WithTx(ctx, func(tx *sql.Tx) error {
		// Le mode gratuit sans limite se lit dans la même transaction que le
		// solde : une organisation offerte sans limite n'est JAMAIS mise en
		// pause, quel que soit l'état de son portefeuille.
		org, exists, err := h.orgs.FindByID(ctx, tx, string(identity.OrgID))
		if err != nil {
			return err
		}
		if exists && org.Unlimited {
			unlimited = true
			return nil
		}

		entries, err := h.wallet.List(ctx, tx, string(identity.OrgID), 1)
		if err != nil {
			return err
		}
		hasWallet = len(entries) > 0
		if !hasWallet {
			return nil
		}

		balance, err = h.wallet.Balance(ctx, tx, string(identity.OrgID))
		return err
	})
	if err != nil || unlimited || !hasWallet || balance > 0 {
		// Une erreur de lecture ne met jamais le service en pause : en cas de
		// doute, on sert la conversation (le débit, lui, est déjà enregistré).
		return "", false
	}

	h.metrics.IncServicePause()

	if !h.pauseNotices.shouldNotify(identity.ConversationID, h.now()) {
		// Déjà prévenu récemment : on reste silencieux plutôt que de répéter.
		return "", true
	}

	reply := "Je suis en pause : les crédits de votre organisation sont épuisés. " +
		"Vos souvenirs, vos rappels et vos conversations sont intacts — une recharge et je reprends là où nous en étions."

	if h.profileLinks != nil {
		url, ok, err := h.profileLinks.GenerateProfileLink(ctx, string(identity.OrgID), string(identity.PrincipalID))
		if err == nil && ok {
			reply += "\n\nPour recharger : " + url + "\n(lien valable un quart d'heure, à usage unique)"
		}
	}

	return reply, true
}
