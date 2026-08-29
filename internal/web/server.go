package web

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/web/core"
	"github.com/bornholm/automata/internal/web/view"
)

//go:embed assets
var assetsFS embed.FS

// shutdownTimeout suit internal/observability/http.go : même modèle de
// serveur, même politique d'arrêt.
const shutdownTimeout = 5 * time.Second

// Server assemble le routeur et le serveur HTTP. Tout ce qu'il partage
// avec les écrans vit dans core.Deps, qu'il embarque : les handlers y
// accèdent sous les mêmes noms, quel que soit leur paquet.
type Server struct {
	*core.Deps

	httpServer *http.Server
}

// NewServer construit le serveur décrit par cfg.Web. mail peut être nil
// (aucun provider d'envoi configuré).
func NewServer(cfg *config.Config, db *persistence.DB, mail core.MailSender, logger *slog.Logger) *Server {
	s := &Server{Deps: core.NewDeps(cfg, db, mail, logger)}

	mux := http.NewServeMux()

	assets, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		// Impossible par construction (le répertoire est embarqué).
		panic(err)
	}
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(assets)))

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/dashboard", http.StatusFound)
	})

	mux.HandleFunc("GET /admin/login", s.handleLoginForm)
	mux.HandleFunc("POST /admin/login", s.handleLogin)
	mux.HandleFunc("POST /admin/logout", s.handleLogout)
	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/dashboard", http.StatusFound)
	})

	admin := func(h http.HandlerFunc) http.HandlerFunc { return s.requireAdmin(h) }
	mux.HandleFunc("GET /admin/dashboard", admin(s.handleDashboard))
	mux.HandleFunc("GET /admin/orgs", admin(s.handleOrgs))
	mux.HandleFunc("GET /admin/orgs/new", admin(s.handleOrgNewForm))
	mux.HandleFunc("POST /admin/orgs", admin(s.handleOrgCreate))
	mux.HandleFunc("GET /admin/orgs/{id}", admin(s.handleOrg))
	mux.HandleFunc("GET /admin/orgs/{id}/wallet.csv", admin(s.handleOrgWalletCSV))
	mux.HandleFunc("POST /admin/orgs/{id}/grant", admin(s.handleOrgGrant))
	mux.HandleFunc("POST /admin/orgs/{id}/offered", admin(s.handleOrgOffered))
	mux.HandleFunc("POST /admin/orgs/{id}/unlimited", admin(s.handleOrgUnlimited))
	mux.HandleFunc("POST /admin/orgs/{id}/models", admin(s.handleOrgModels))
	mux.HandleFunc("POST /admin/orgs/{id}/group-token", admin(s.handleOrgGroupToken))
	mux.HandleFunc("POST /admin/orgs/{id}/customization", admin(s.handleOrgCustomization))
	mux.HandleFunc("POST /admin/orgs/{id}/delete", admin(s.handleOrgDelete))
	mux.HandleFunc("GET /admin/orgs/{id}/members/new", admin(s.handleMemberNewForm))
	mux.HandleFunc("POST /admin/orgs/{id}/members", admin(s.handleMemberCreate))
	mux.HandleFunc("GET /admin/members/{id}", admin(s.handleMember))
	mux.HandleFunc("POST /admin/members/{id}", admin(s.handleMemberUpdate))
	mux.HandleFunc("POST /admin/members/{id}/token", admin(s.handleMemberToken))
	mux.HandleFunc("POST /admin/members/{id}/token/revoke", admin(s.handleMemberTokenRevoke))
	mux.HandleFunc("POST /admin/members/{id}/profile-link", admin(s.handleMemberProfileLink))
	mux.HandleFunc("GET /admin/platforms", admin(s.handlePlatforms))
	mux.HandleFunc("POST /admin/platforms/group-token", admin(s.handlePlatformsGroupToken))
	mux.HandleFunc("POST /admin/platforms", admin(s.handlePlatformCreate))
	mux.HandleFunc("POST /admin/platforms/{id}/toggle", admin(s.handlePlatformToggle))
	mux.HandleFunc("POST /admin/platforms/{id}/delete", admin(s.handlePlatformDelete))
	mux.HandleFunc("GET /admin/pricing", admin(s.handlePricing))
	mux.HandleFunc("POST /admin/pricing/packs", admin(s.handlePricingPackCreate))
	mux.HandleFunc("POST /admin/pricing/packs/delete", admin(s.handlePricingPackDelete))
	mux.HandleFunc("POST /admin/pricing/packs/feature", admin(s.handlePricingPackFeature))
	mux.HandleFunc("POST /admin/pricing/settings", admin(s.handlePricingSettings))
	mux.HandleFunc("POST /admin/pricing/models", admin(s.handleModelPriceUpsert))
	mux.HandleFunc("POST /admin/pricing/models/delete", admin(s.handleModelPriceDelete))
	// Bibliothèque de compétences (ADM — voir docs/skills.md). Le nom
	// d'une compétence est sa clé : les routes d'édition le portent.
	mux.HandleFunc("GET /admin/skills", admin(s.handleSkills))
	mux.HandleFunc("GET /admin/skills/new", admin(s.handleSkillNewForm))
	mux.HandleFunc("POST /admin/skills", admin(s.handleSkillCreate))
	mux.HandleFunc("GET /admin/skills/{name}", admin(s.handleSkillForm))
	mux.HandleFunc("POST /admin/skills/{name}", admin(s.handleSkillUpdate))
	mux.HandleFunc("POST /admin/skills/{name}/delete", admin(s.handleSkillDelete))
	mux.HandleFunc("POST /admin/skills/{name}/restore", admin(s.handleSkillRestore))

	// Catalogue de modèles : la base fait autorité, une modification
	// s'applique au message suivant (voir internal/llmclients).
	mux.HandleFunc("GET /admin/llm-clients", admin(s.handleLLMClients))
	mux.HandleFunc("POST /admin/llm-clients/roles", admin(s.handleInstanceModels))
	mux.HandleFunc("GET /admin/llm-clients/new", admin(s.handleLLMClientNewForm))
	mux.HandleFunc("POST /admin/llm-clients", admin(s.handleLLMClientCreate))
	mux.HandleFunc("GET /admin/llm-clients/{name}", admin(s.handleLLMClientForm))
	mux.HandleFunc("POST /admin/llm-clients/{name}", admin(s.handleLLMClientUpdate))
	mux.HandleFunc("POST /admin/llm-clients/{name}/delete", admin(s.handleLLMClientDelete))

	mux.HandleFunc("GET /admin/plugins", admin(s.handlePlugins))
	mux.HandleFunc("POST /admin/plugins/{name}/restart", admin(s.handlePluginRestart))
	mux.HandleFunc("POST /admin/orgs/{id}/plugins", admin(s.handleOrgPlugins))
	mux.HandleFunc("GET /admin/instance", admin(s.handleInstance))
	mux.HandleFunc("GET /admin/usage", admin(s.handleUsage))
	mux.HandleFunc("GET /admin/usage.csv", admin(s.handleUsageCSV))

	mux.HandleFunc("GET /plugins/{name}/oauth/callback", s.handlePluginOAuthCallback)
	// Interfaces des plugins : une seule porte pour l'opérateur et pour
	// les membres, authentifiée par le jeton du chemin — une iframe
	// sandbouclée ne transporte aucun cookie.
	mux.HandleFunc("GET /plugin-ui/{token}/{path...}", s.handlePluginUI)
	mux.HandleFunc("POST /plugin-ui/{token}/{path...}", s.handlePluginUI)

	// Lien temporaire de téléchargement d'un fichier de plugin : la
	// seconde voie de sortie, pour ce qui ne tient pas en pièce jointe.
	mux.HandleFunc("GET /f/{token}", s.handleFileLink)

	mux.HandleFunc("GET /p/{link}/plugins/{name}", s.handleProfilePluginPage)
	mux.HandleFunc("GET /p/{link}", s.handleProfile)
	mux.HandleFunc("GET /p/{link}/credits", s.handleProfileCredits)
	mux.HandleFunc("POST /p/{link}/email", s.handleProfileEmail)
	mux.HandleFunc("POST /p/{link}/email/verify", s.handleProfileEmailVerify)
	mux.HandleFunc("POST /p/{link}/checkout", s.handleCheckout)
	mux.HandleFunc("GET /p/{link}/usage", s.handleProfileUsage)
	mux.HandleFunc("POST /p/{link}/open", s.handleProfileOpen)
	mux.HandleFunc("GET /p/{link}/privacy", s.handleProfilePrivacy)
	mux.HandleFunc("GET /p/{link}/privacy/export", s.handleProfileExport)
	mux.HandleFunc("POST /p/{link}/privacy/delete", s.handleProfileDelete)
	mux.HandleFunc("POST /stripe/webhook", s.handleStripeWebhook)

	// Pages publiques publiées par les plugins (magasin d'objets). Servies
	// sans session, sous CSP sandbox : voir handlers_public_site.go.
	mux.HandleFunc("GET /s/{slug}", s.handlePublicSiteRoot)
	mux.HandleFunc("GET /s/{slug}/{path...}", s.handlePublicSite)
	// Prévisualisation d'un brouillon derrière un jeton signé éphémère.
	mux.HandleFunc("GET /d/{token}", s.handleDraftPreviewRoot)
	mux.HandleFunc("GET /d/{token}/{path...}", s.handleDraftPreview)

	s.httpServer = &http.Server{Addr: cfg.Web.Addr, Handler: mux}

	return s
}

// WithPluginManager branche le gestionnaire de plugins : sans lui,
// l'écran des plugins affiche une liste vide et l'activation par
// organisation est masquée.
func (s *Server) WithPluginManager(manager core.PluginManager) *Server {
	s.PluginMgr = manager
	return s
}

// WithPlatformManager branche le gestionnaire de comptes de messagerie :
// sans lui, l'écran des plateformes affiche la configuration enregistrée
// mais aucun état de connexion.
func (s *Server) WithPlatformManager(manager core.PlatformManager) *Server {
	s.PlatformMgr = manager
	return s
}

// WithPlatformValidator branche la vérification des configurations de
// comptes de messagerie.
func (s *Server) WithPlatformValidator(validate func(providerType string, config map[string]any) error) *Server {
	s.ValidatePlatform = validate
	return s
}

// WithPurchaseNotifier branche la confirmation conversationnelle des
// achats : sans lui, un paiement crédite le portefeuille en silence.
func (s *Server) WithPurchaseNotifier(notifier core.PurchaseNotifier) *Server {
	s.Purchases = notifier
	return s
}

// WithPrivacy branche le service de confidentialité (PRO-04).
func (s *Server) WithPrivacy(service core.PrivacyService) *Server {
	s.Privacy = service
	return s
}

// Handler expose le routeur, pour les tests httptest.
func (s *Server) Handler() http.Handler {
	return s.httpServer.Handler
}

// Run démarre le serveur et bloque jusqu'à l'annulation de ctx, puis
// s'arrête proprement (même modèle que observability.Server.Run).
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)

	go func() {
		s.Logger.InfoContext(ctx, "web: serveur démarré", "addr", s.httpServer.Addr)

		// Une écoute en boucle locale dans un conteneur n'est joignable
		// par personne : ni le proxy de l'hôte, ni la sonde de démarrage,
		// qui répondront « connection refused » sans jamais dire pourquoi.
		// Le signaler ici est la seule occasion de relier la cause à
		// l'effet.
		if inContainer() && isLoopbackAddr(s.httpServer.Addr) {
			s.Logger.WarnContext(ctx, "web: écoute en boucle locale dans un conteneur, injoignable depuis l'extérieur",
				"addr", s.httpServer.Addr, "attendu", "0.0.0.0:<port>")
		}
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("web: arrêt du serveur: %w", err)
	}

	return <-errCh
}

// render écrit un composant templ, en journalisant l'échec éventuel (une
// connexion coupée en cours d'écriture n'est pas une panne).
func (s *Server) Render(w http.ResponseWriter, r *http.Request, status int, component templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := component.Render(r.Context(), w); err != nil {
		s.Logger.ErrorContext(r.Context(), "web: échec du rendu d'une vue", "path", r.URL.Path, "error", err)
	}
}

// withTx exécute fn dans une transaction et journalise l'erreur ; retourne
// false si le handler doit s'interrompre (une réponse 500 a été envoyée).
func (s *Server) WithTx(w http.ResponseWriter, r *http.Request, fn func(tx *sql.Tx) error) bool {
	err := s.DB.WithTx(r.Context(), fn)
	if err != nil {
		s.Logger.ErrorContext(r.Context(), "web: échec d'un accès à la base", "path", r.URL.Path, "error", err)
		http.Error(w, "erreur interne", http.StatusInternalServerError)
		return false
	}
	return true
}

// sidebarPlatforms liste les comptes de messagerie enregistrés, pour la
// sidebar : l'état vivant du gestionnaire, jamais le fichier — les comptes
// se gèrent en ligne.
func (s *Server) SidebarPlatforms() []view.SidebarPlatform {
	if s.PlatformMgr == nil {
		return nil
	}

	var platforms []view.SidebarPlatform
	for name, status := range s.PlatformMgr.Statuses() {
		platforms = append(platforms, view.SidebarPlatform{
			Type: status.Type,
			Name: core.PlatformDisplayName(status.Type, name),
		})
	}
	slices.SortFunc(platforms, func(a, b view.SidebarPlatform) int {
		return strings.Compare(a.Name, b.Name)
	})

	return platforms
}

// isLoopbackAddr indique si l'adresse d'écoute est restreinte à la boucle
// locale.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// inContainer reconnaît une exécution conteneurisée. Le fichier
// /.dockerenv est posé par Docker ; son absence n'affirme rien, d'où un
// simple avertissement plutôt qu'un refus de démarrer.
func inContainer() bool {
	_, err := os.Stat("/.dockerenv")
	return err == nil
}

// DraftPreviewMinter et FileLinkMinter sont réexportées depuis core : le
// registre ne connaît que le paquet web, et le secret de signature reste
// capturé dans la closure sans traverser internal/plugin.
var (
	DraftPreviewMinter = core.DraftPreviewMinter
	FileLinkMinter     = core.FileLinkMinter
)

// HashPassword produit l'empreinte bcrypt attendue par web.admin
// (commande « automata web hash-password »).
var HashPassword = core.HashPassword
