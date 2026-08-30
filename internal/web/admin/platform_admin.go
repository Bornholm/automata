package admin

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/bornholm/automata/internal/persistence"
)

// qrPNGDataURI rend un code d'appairage en image PNG embarquée. Le code
// n'a que quelques secondes de validité et vaut accès au compte : il est
// rendu à la volée, jamais écrit sur disque ni journalisé.
func qrPNGDataURI(code string) (string, error) {
	png, err := qrcode.Encode(code, qrcode.Medium, 512)
	if err != nil {
		return "", err
	}

	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), nil
}

// platformConfigFields décrit les champs de configuration attendus par
// type de compte : ce sont ceux du provider Courier correspondant.
// Secret marque les champs à masquer à la saisie et à ne jamais réafficher.
type platformField struct {
	Name        string
	Label       string
	Placeholder string
	Secret      bool
	Required    bool
}

// platformFields décrit les champs attendus par type de compte. Les noms
// sont EXACTEMENT ceux des structs de configuration
// (internal/config/courier_providers.go) : ce sont eux que le
// constructeur de fournisseur décodera, un nom approchant ne produit rien
// d'autre qu'un compte qui refuse de démarrer.
func platformFields(providerType string) []platformField {
	switch providerType {
	case "whatsapp":
		return []platformField{
			{Name: "session_path", Label: "Fichier de session", Placeholder: "data/whatsapp/session.db", Required: true},
		}
	case "signal":
		return []platformField{
			{Name: "account", Label: "Numéro du compte", Placeholder: "+33612345678", Required: true},
			{Name: "address", Label: "Adresse du démon signal-cli", Placeholder: "127.0.0.1:8080"},
		}
	case "rocket":
		return []platformField{
			{Name: "server_url", Label: "Adresse du serveur", Placeholder: "https://chat.exemple.fr", Required: true},
			{Name: "username", Label: "Identifiant du bot", Placeholder: "automata", Required: true},
			{Name: "password", Label: "Mot de passe du bot", Secret: true, Required: true},
		}
	case "discord":
		return []platformField{
			{Name: "token", Label: "Jeton du bot", Secret: true, Required: true},
		}
	case "mail":
		return []platformField{
			{Name: "imap.address", Label: "Serveur IMAP", Placeholder: "imap.exemple.fr:993", Required: true},
			{Name: "imap.username", Label: "Utilisateur IMAP", Required: true},
			{Name: "imap.password", Label: "Mot de passe IMAP", Secret: true, Required: true},
			{Name: "smtp.address", Label: "Serveur SMTP", Placeholder: "smtp.exemple.fr:587", Required: true},
			{Name: "smtp.issuer", Label: "Adresse d'expédition", Placeholder: "automata@exemple.fr", Required: true},
			{Name: "smtp.username", Label: "Utilisateur SMTP"},
			{Name: "smtp.password", Label: "Mot de passe SMTP", Secret: true},
		}
	default:
		return nil
	}
}

// formToConfig assemble la configuration d'un compte depuis le formulaire,
// en respectant l'imbrication des champs pointés (« imap.address »).
func formToConfig(r *http.Request, providerType string) map[string]any {
	config := map[string]any{}

	for _, field := range platformFields(providerType) {
		value := strings.TrimSpace(r.PostFormValue(field.Name))
		if value == "" {
			continue
		}

		parent, child, nested := strings.Cut(field.Name, ".")
		if !nested {
			config[field.Name] = value
			continue
		}

		section, _ := config[parent].(map[string]any)
		if section == nil {
			section = map[string]any{}
			config[parent] = section
		}
		section[child] = value
	}

	return config
}

// HandlePlatformCreate enregistre un nouveau compte de messagerie et
// demande au gestionnaire de le démarrer.
func (h *Handlers) HandlePlatformCreate(w http.ResponseWriter, r *http.Request) {
	providerType := r.PostFormValue("type")
	if platformFields(providerType) == nil {
		http.Redirect(w, r, "/admin/platforms?error=type", http.StatusFound)
		return
	}

	id := strings.TrimSpace(r.PostFormValue("id"))
	if id == "" {
		id = providerType
	}
	id = slugify(id)

	config := formToConfig(r, providerType)
	for _, field := range platformFields(providerType) {
		if !field.Required {
			continue
		}
		if _, ok := lookupConfig(config, field.Name); !ok {
			http.Redirect(w, r, "/admin/platforms?error=champs&type="+providerType, http.StatusFound)
			return
		}
	}

	// La configuration doit produire un fournisseur : la vérifier ici évite
	// d'enregistrer un compte qui échouerait indéfiniment au démarrage.
	if h.ValidatePlatform != nil {
		if err := h.ValidatePlatform(providerType, config); err != nil {
			h.Logger.WarnContext(r.Context(), "web: configuration de compte refusée",
				"platform_id", id, "type", providerType, "error", err)
			http.Redirect(w, r, "/admin/platforms?pairing="+pairingModeFor(providerType)+
				"&platform="+providerType+"&error=invalide", http.StatusFound)
			return
		}
	}

	raw, err := json.Marshal(config)
	if err != nil {
		http.Error(w, "erreur interne", http.StatusInternalServerError)
		return
	}

	sealed, err := h.Secrets.Seal(string(raw))
	if err != nil {
		h.Logger.ErrorContext(r.Context(), "web: chiffrement de la configuration d'un compte", "platform_id", id, "error", err)
		http.Error(w, "erreur interne", http.StatusInternalServerError)
		return
	}

	now := h.Now()
	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		return h.Platforms.Insert(r.Context(), tx, persistence.Platform{
			ID:          id,
			Type:        providerType,
			DisplayName: strings.TrimSpace(r.PostFormValue("display_name")),
			Config:      sealed,
			Enabled:     true,
			CreatedAt:   now,
			UpdatedAt:   now,
		}, false)
	})
	if !ok {
		return
	}

	// Le secret n'apparaît jamais dans les journaux : seuls l'identifiant
	// et le type du compte.
	h.Logger.InfoContext(r.Context(), "web: compte de messagerie ajouté", "platform_id", id, "type", providerType)
	h.wakePlatforms()

	http.Redirect(w, r, "/admin/platforms?added="+id, http.StatusFound)
}

// lookupConfig retrouve une valeur dans la configuration assemblée, en
// suivant l'imbrication des champs pointés.
func lookupConfig(config map[string]any, name string) (string, bool) {
	parent, child, nested := strings.Cut(name, ".")
	if !nested {
		value, ok := config[name].(string)
		return value, ok && value != ""
	}

	section, _ := config[parent].(map[string]any)
	if section == nil {
		return "", false
	}
	value, ok := section[child].(string)

	return value, ok && value != ""
}

// HandlePlatformToggle active ou désactive un compte : le gestionnaire
// démarre ou arrête son pipeline sans redémarrage du processus.
func (h *Handlers) HandlePlatformToggle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	enabled := r.PostFormValue("enabled") == "true"

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		p, found, err := h.Platforms.FindByID(r.Context(), tx, id)
		if err != nil || !found {
			return err
		}
		p.Enabled = enabled
		p.UpdatedAt = h.Now()
		return h.Platforms.Update(r.Context(), tx, p)
	})
	if !ok {
		return
	}

	h.Logger.InfoContext(r.Context(), "web: compte de messagerie basculé", "platform_id", id, "enabled", enabled)
	h.wakePlatforms()

	http.Redirect(w, r, "/admin/platforms", http.StatusFound)
}

// HandlePlatformDelete retire un compte. La session sur disque n'est pas
// supprimée : la détruire relève d'une décision explicite de l'exploitant.
func (h *Handlers) HandlePlatformDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		return h.Platforms.Delete(r.Context(), tx, id)
	})
	if !ok {
		return
	}

	h.Logger.InfoContext(r.Context(), "web: compte de messagerie supprimé", "platform_id", id)
	h.wakePlatforms()

	http.Redirect(w, r, "/admin/platforms", http.StatusFound)
}

// wakePlatforms demande au gestionnaire d'appliquer immédiatement les
// changements ; sans lui, la prochaine synchronisation périodique s'en
// chargerait, mais l'écran mentirait entre-temps.
func (h *Handlers) wakePlatforms() {
	if h.PlatformMgr != nil {
		h.PlatformMgr.Wake()
	}
}

// platformStatusChip traduit l'état d'un compte en pastille.
func platformStatusChip(state string) (label, tone string) {
	switch state {
	case "running":
		return "Connectée", "ok"
	case "pairing":
		return "Appairage requis", "warn"
	case "starting":
		return "Connexion…", "neutral"
	case "failed":
		return "Déconnectée", "crit"
	case "stopped":
		return "Arrêtée", "neutral"
	default:
		return "Inconnue", "neutral"
	}
}

// sinceLabel décrit depuis quand un état dure.
func sinceLabel(since time.Time) string {
	if since.IsZero() {
		return ""
	}

	switch elapsed := time.Since(since); {
	case elapsed < time.Minute:
		return "à l'instant"
	case elapsed < time.Hour:
		return "depuis " + itoa(int(elapsed.Minutes())) + " min"
	case elapsed < 24*time.Hour:
		return "depuis " + itoa(int(elapsed.Hours())) + " h"
	default:
		return "depuis le " + since.Format("02/01")
	}
}

// itoa évite strconv dans les handlers.
func itoa(n int) string { return strconv.Itoa(n) }

// pairingModeFor retrouve la variante du gabarit d'appairage d'un type de
// compte, pour rouvrir le formulaire là où l'utilisateur l'a laissé.
func pairingModeFor(providerType string) string {
	if providerType == "whatsapp" || providerType == "signal" {
		return "qr"
	}
	return "credentials"
}
