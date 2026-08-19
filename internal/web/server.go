package web

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/a-h/templ"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/platform"
	"github.com/bornholm/automata/internal/secretbox"
	"github.com/bornholm/automata/internal/web/view"
	"github.com/bornholm/automata/internal/weblink"
)

//go:embed assets
var assetsFS embed.FS

// shutdownTimeout suit internal/observability/http.go : même modèle de
// serveur, même politique d'arrêt.
const shutdownTimeout = 5 * time.Second

// MailSender envoie un code de vérification de courriel (PRO-01). Nil =
// envoi non configuré : la vérification est proposée mais désactivée.
type MailSender interface {
	SendVerificationCode(ctx context.Context, email, code string) error
}

// Server est le serveur web d'administration et de profil.
type Server struct {
	httpServer *http.Server

	db     *persistence.DB
	cfg    *config.Config
	logger *slog.Logger
	now    func() time.Time
	signer signer
	mail   MailSender

	stripe  *stripeClient
	secrets *secretbox.Box
	// platformManager, s'il est renseigné, porte l'état réel des comptes
	// de messagerie et applique leurs changements à chaud (pilier 2).
	platformManager PlatformManager
	limiter         *loginLimiter
	reveals         *revealStash
	codes           *codeStore

	orgs         *persistence.OrganizationRepository
	members      *persistence.MemberRepository
	linkTokens   *persistence.LinkTokenRepository
	wallet       *persistence.WalletRepository
	profileLinks *persistence.ProfileLinkRepository
	usage        *persistence.UsageRecordRepository
	platforms    *persistence.PlatformRepository
	bindings     *persistence.ChannelBindingRepository
}

// PlatformManager est la vue qu'a le serveur web du gestionnaire de
// comptes de messagerie (internal/platform) : lire l'état, et demander
// l'application immédiate d'un changement.
type PlatformManager interface {
	Statuses() map[string]platform.Status
	Wake()
}

// NewServer construit le serveur décrit par cfg.Web. mail peut être nil
// (aucun provider d'envoi configuré).
func NewServer(cfg *config.Config, db *persistence.DB, mail MailSender, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}

	s := &Server{
		db:      db,
		cfg:     cfg,
		logger:  logger,
		now:     time.Now,
		signer:  signer{secret: []byte(cfg.Web.SessionSecret)},
		mail:    mail,
		limiter: &loginLimiter{},
		reveals: newRevealStash(),
		codes:   newCodeStore(),

		orgs:         persistence.NewOrganizationRepository(),
		members:      persistence.NewMemberRepository(),
		linkTokens:   persistence.NewLinkTokenRepository(),
		wallet:       persistence.NewWalletRepository(),
		profileLinks: persistence.NewProfileLinkRepository(),
		usage:        persistence.NewUsageRecordRepository(),
		platforms:    persistence.NewPlatformRepository(),
		bindings:     persistence.NewChannelBindingRepository(),
	}

	// La clé de chiffrement des secrets dérive du secret de session : la
	// configuration des comptes de messagerie porte des mots de passe.
	if box, err := secretbox.New(cfg.Web.SessionSecret); err == nil {
		s.secrets = box
	}

	if cfg.Web.Stripe.Enabled() {
		s.stripe = newStripeClient(cfg.Web.Stripe.SecretKey)
	}

	mux := http.NewServeMux()

	assets, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		// Impossible par construction (le répertoire est embarqué).
		panic(err)
	}
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(assets)))

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/orgs", http.StatusFound)
	})

	mux.HandleFunc("GET /admin/login", s.handleLoginForm)
	mux.HandleFunc("POST /admin/login", s.handleLogin)
	mux.HandleFunc("POST /admin/logout", s.handleLogout)
	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/orgs", http.StatusFound)
	})

	admin := func(h http.HandlerFunc) http.HandlerFunc { return s.requireAdmin(h) }
	mux.HandleFunc("GET /admin/orgs", admin(s.handleOrgs))
	mux.HandleFunc("GET /admin/orgs/new", admin(s.handleOrgNewForm))
	mux.HandleFunc("POST /admin/orgs", admin(s.handleOrgCreate))
	mux.HandleFunc("GET /admin/orgs/{id}", admin(s.handleOrg))
	mux.HandleFunc("GET /admin/orgs/{id}/wallet.csv", admin(s.handleOrgWalletCSV))
	mux.HandleFunc("POST /admin/orgs/{id}/grant", admin(s.handleOrgGrant))
	mux.HandleFunc("POST /admin/orgs/{id}/offered", admin(s.handleOrgOffered))
	mux.HandleFunc("POST /admin/orgs/{id}/group-token", admin(s.handleOrgGroupToken))
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

	mux.HandleFunc("GET /p/{link}", s.handleProfile)
	mux.HandleFunc("GET /p/{link}/credits", s.handleProfileCredits)
	mux.HandleFunc("POST /p/{link}/email", s.handleProfileEmail)
	mux.HandleFunc("POST /p/{link}/email/verify", s.handleProfileEmailVerify)
	mux.HandleFunc("POST /p/{link}/checkout", s.handleCheckout)
	mux.HandleFunc("POST /stripe/webhook", s.handleStripeWebhook)

	s.httpServer = &http.Server{Addr: cfg.Web.Addr, Handler: mux}

	return s
}

// WithPlatformManager branche le gestionnaire de comptes de messagerie :
// sans lui, l'écran des plateformes affiche la configuration enregistrée
// mais aucun état de connexion.
func (s *Server) WithPlatformManager(manager PlatformManager) *Server {
	s.platformManager = manager
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
		s.logger.InfoContext(ctx, "web: serveur démarré", "addr", s.httpServer.Addr)
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
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, component templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := component.Render(r.Context(), w); err != nil {
		s.logger.ErrorContext(r.Context(), "web: échec du rendu d'une vue", "path", r.URL.Path, "error", err)
	}
}

// withTx exécute fn dans une transaction et journalise l'erreur ; retourne
// false si le handler doit s'interrompre (une réponse 500 a été envoyée).
func (s *Server) withTx(w http.ResponseWriter, r *http.Request, fn func(tx *sql.Tx) error) bool {
	err := s.db.WithTx(r.Context(), fn)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "web: échec d'un accès à la base", "path", r.URL.Path, "error", err)
		http.Error(w, "erreur interne", http.StatusInternalServerError)
		return false
	}
	return true
}

// sidebarPlatforms liste les plateformes configurées pour la sidebar.
func (s *Server) sidebarPlatforms() []view.SidebarPlatform {
	return configuredPlatforms(s.cfg)
}

// revealStash conserve quelques secondes les secrets à afficher une seule
// fois (jeton fraîchement généré, lien de profil) entre le POST qui les
// crée et le GET qui les affiche (schéma POST-redirect-GET : sans ce
// détour, rafraîchir la page rejouerait la génération). Un secret est
// consommé à la première lecture ou expire au bout de 2 minutes ; tout
// reste en mémoire, rien n'est jamais écrit en clair.
type revealStash struct {
	mu      sync.Mutex
	entries map[string]revealEntry
}

type revealEntry struct {
	value   revealValue
	expires time.Time
}

// revealValue porte les formes d'affichage d'un secret fraîchement créé.
type revealValue struct {
	Clear   string
	Display string
}

func newRevealStash() *revealStash {
	return &revealStash{entries: map[string]revealEntry{}}
}

const revealTTL = 2 * time.Minute

func (s *revealStash) put(value revealValue, now time.Time) (key string, err error) {
	key, err = weblink.RandomCrockford(16)
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for k, e := range s.entries {
		if now.After(e.expires) {
			delete(s.entries, k)
		}
	}
	s.entries[key] = revealEntry{value: value, expires: now.Add(revealTTL)}

	return key, nil
}

func (s *revealStash) pop(key string, now time.Time) (revealValue, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.entries[key]
	delete(s.entries, key)
	if !ok || now.After(entry.expires) {
		return revealValue{}, false
	}

	return entry.value, true
}

// codeStore conserve en mémoire les codes de vérification de courriel en
// attente (PRO-01) : un par membre, 10 minutes. Un redémarrage du worker
// les efface — la personne redemande simplement un code.
type codeStore struct {
	mu      sync.Mutex
	entries map[string]codeEntry
}

type codeEntry struct {
	email   string
	code    string
	expires time.Time
}

func newCodeStore() *codeStore {
	return &codeStore{entries: map[string]codeEntry{}}
}

const codeTTL = 10 * time.Minute

func (s *codeStore) put(memberID, email, code string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[memberID] = codeEntry{email: email, code: code, expires: now.Add(codeTTL)}
}

// pending retourne l'adresse en cours de vérification pour le membre.
func (s *codeStore) pending(memberID string, now time.Time) (email string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, found := s.entries[memberID]
	if !found || now.After(entry.expires) {
		return "", false
	}
	return entry.email, true
}

// verify consomme le code s'il correspond.
func (s *codeStore) verify(memberID, code string, now time.Time) (email string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, found := s.entries[memberID]
	if !found || now.After(entry.expires) || entry.code != code {
		return "", false
	}
	delete(s.entries, memberID)

	return entry.email, true
}
