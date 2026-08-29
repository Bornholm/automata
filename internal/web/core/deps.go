// Package core porte ce que partagent tous les écrans du serveur web :
// les dépendances (base, configuration, dépôts, gestionnaires), les
// helpers transverses (transaction, rendu, CSRF, jetons signés, calculs de
// portefeuille) et les contrats vers le reste de l'application.
//
// Il existe pour que les écrans se répartissent en paquets par surface —
// internal/web/admin, .../profile, .../public — sans qu'aucun n'ait à
// connaître les autres. Chacun embarque *core.Deps et retrouve, sous le
// même nom, ce que les handlers utilisaient quand ils vivaient tous dans
// un seul paquet.
//
// Les champs y sont exportés parce qu'ils traversent une frontière de
// paquet ; Deps ne se construit que par NewDeps, jamais littéralement
// hors d'ici.
package core

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/platform"
	"github.com/bornholm/automata/internal/privacy"
	"github.com/bornholm/automata/internal/secretbox"
	"github.com/bornholm/automata/internal/web/view"
)

// MailSender envoie un code de vérification de courriel (PRO-01). Nil =
// envoi non configuré : la vérification est proposée mais désactivée.
type MailSender interface {
	SendVerificationCode(ctx context.Context, email, code string) error
}

// PrivacyService est la vue qu'a le serveur du service de confidentialité
// (internal/privacy).
type PrivacyService interface {
	Export(ctx context.Context, memberID string) (privacy.Export, error)
	Delete(ctx context.Context, memberID string) (privacy.DeletionReport, error)
	DeleteOrganization(ctx context.Context, orgID string) (privacy.OrgDeletionReport, error)
}

// PlatformManager est la vue qu'a le serveur web du gestionnaire de
// comptes de messagerie (internal/platform) : lire l'état, et demander
// l'application immédiate d'un changement.
type PlatformManager interface {
	Statuses() map[string]platform.Status
	Wake()
}

// PurchaseNotifier porte la confirmation d'un achat jusqu'à la
// conversation privée de l'acheteur. Le serveur web ne connaît pas les
// plateformes de messagerie : l'envoi est implémenté par le registre.
type PurchaseNotifier interface {
	NotifyPurchase(ctx context.Context, memberID string, credits, balance int64) error
}

// Deps rassemble tout ce dont un écran a besoin.
type Deps struct {
	DB     *persistence.DB
	Cfg    *config.Config
	Logger *slog.Logger
	Now    func() time.Time
	Signer Signer
	Mail   MailSender

	Stripe  *StripeClient
	Secrets *secretbox.Box
	// PlatformMgr, s'il est renseigné, porte l'état réel des comptes de
	// messagerie et applique leurs changements à chaud (pilier 2).
	PlatformMgr PlatformManager
	// ValidatePlatform vérifie qu'une configuration de compte produit bien
	// un fournisseur, avant de l'enregistrer : sans ce contrôle, un champ
	// mal nommé donne un compte qui ne démarrera jamais, et l'erreur
	// n'apparaît que dans les journaux.
	ValidatePlatform func(providerType string, config map[string]any) error
	// Privacy sert l'export et la suppression des données personnelles ;
	// nil désactive l'écran de confidentialité.
	Privacy PrivacyService
	// PluginMgr, s'il est renseigné, porte l'état des plugins et le
	// redémarrage manuel.
	PluginMgr PluginManager
	// Purchases, s'il est renseigné, confirme les achats dans la
	// conversation privée de l'acheteur.
	Purchases PurchaseNotifier
	Limiter   *LoginLimiter
	Reveals   *RevealStash
	Codes     *CodeStore

	Orgs              *persistence.OrganizationRepository
	Members           *persistence.MemberRepository
	LinkTokens        *persistence.LinkTokenRepository
	Wallet            *persistence.WalletRepository
	ProfileLinks      *persistence.ProfileLinkRepository
	Usage             *persistence.UsageRecordRepository
	Platforms         *persistence.PlatformRepository
	OrgSettings       *persistence.OrgSettingsRepository
	PluginActivations *persistence.PluginActivationRepository
	Skills            *persistence.SkillRepository
	PricingRepo       *persistence.PricingRepository
	ModelPrices       *persistence.ModelPriceRepository
	Bindings          *persistence.ChannelBindingRepository
	PluginObjects     *persistence.PluginObjectRepository
	PluginSites       *persistence.PluginPublicSiteRepository
	LLMClients        *persistence.LLMClientRepository
	OrgClients        *persistence.OrgAgentClientRepository
	// LLMBox scelle les clés d'API du catalogue de modèles. Nil si le
	// secret de session est inexploitable : les écrans du catalogue
	// refusent alors d'écrire plutôt que d'enregistrer une clé en clair.
	LLMBox *secretbox.Box
}

// NewDeps construit les dépendances partagées. mail peut être nil (aucun
// provider d'envoi configuré).
func NewDeps(cfg *config.Config, db *persistence.DB, mail MailSender, logger *slog.Logger) *Deps {
	if logger == nil {
		logger = slog.Default()
	}

	d := &Deps{
		DB:      db,
		Cfg:     cfg,
		Logger:  logger,
		Now:     time.Now,
		Signer:  NewSigner(cfg.Web.SessionSecret),
		Mail:    mail,
		Limiter: &LoginLimiter{},
		Reveals: NewRevealStash(),
		Codes:   NewCodeStore(),

		Orgs:              persistence.NewOrganizationRepository(),
		Members:           persistence.NewMemberRepository(),
		LinkTokens:        persistence.NewLinkTokenRepository(),
		Wallet:            persistence.NewWalletRepository(),
		ProfileLinks:      persistence.NewProfileLinkRepository(),
		Usage:             persistence.NewUsageRecordRepository(),
		Platforms:         persistence.NewPlatformRepository(),
		OrgSettings:       persistence.NewOrgSettingsRepository(),
		PricingRepo:       persistence.NewPricingRepository(),
		ModelPrices:       persistence.NewModelPriceRepository(),
		Bindings:          persistence.NewChannelBindingRepository(),
		PluginActivations: persistence.NewPluginActivationRepository(),
		Skills:            persistence.NewSkillRepository(),
		PluginObjects:     persistence.NewPluginObjectRepository(),
		PluginSites:       persistence.NewPluginPublicSiteRepository(),
		LLMClients:        persistence.NewLLMClientRepository(),
		OrgClients:        persistence.NewOrgAgentClientRepository(),
	}

	// Même dérivation que celle du catalogue côté registry : les deux
	// ouvrent les mêmes clés scellées.
	if box, err := secretbox.NewLLMClients(cfg.Web.SessionSecret); err == nil {
		d.LLMBox = box
	} else {
		logger.Warn("web: catalogue de modèles en lecture seule", "error", err)
	}

	// La clé de chiffrement des secrets dérive du secret de session : la
	// configuration des comptes de messagerie porte des mots de passe.
	if box, err := secretbox.New(cfg.Web.SessionSecret); err == nil {
		d.Secrets = box
	}

	if cfg.Web.Stripe.Enabled() {
		d.Stripe = NewStripeClient(cfg.Web.Stripe.SecretKey, cfg.Web.Stripe.EffectiveTaxCode())
	}

	return d
}

// Render écrit une vue templ avec le bon type de contenu.
func (s *Deps) Render(w http.ResponseWriter, r *http.Request, status int, component templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := component.Render(r.Context(), w); err != nil {
		s.Logger.ErrorContext(r.Context(), "web: échec du rendu d'une vue", "path", r.URL.Path, "error", err)
	}
}

// WithTx exécute fn dans une transaction et journalise l'erreur ; retourne
// false si le handler doit s'interrompre (une réponse 500 a été envoyée).
func (s *Deps) WithTx(w http.ResponseWriter, r *http.Request, fn func(tx *sql.Tx) error) bool {
	err := s.DB.WithTx(r.Context(), fn)
	if err != nil {
		s.Logger.ErrorContext(r.Context(), "web: échec d'un accès à la base", "path", r.URL.Path, "error", err)
		http.Error(w, "erreur interne", http.StatusInternalServerError)
		return false
	}
	return true
}

// SidebarPlatforms liste les comptes de messagerie enregistrés, pour la
// sidebar : l'état vivant du gestionnaire, jamais le fichier — les comptes
// se gèrent en ligne.
func (s *Deps) SidebarPlatforms() []view.SidebarPlatform {
	if s.PlatformMgr == nil {
		return nil
	}

	var platforms []view.SidebarPlatform
	for name, status := range s.PlatformMgr.Statuses() {
		platforms = append(platforms, view.SidebarPlatform{
			Type: status.Type,
			Name: PlatformDisplayName(status.Type, name),
		})
	}
	slices.SortFunc(platforms, func(a, b view.SidebarPlatform) int {
		return strings.Compare(a.Name, b.Name)
	})

	return platforms
}
