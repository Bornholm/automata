package core

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Durées des sessions : longue pour l'opérateur (poste de travail),
// courte pour un profil ouvert par lien temporaire (visite de 1 à 3
// minutes, compteur affiché dans l'en-tête).
const (
	AdminSessionTTL   = 12 * time.Hour
	ProfileSessionTTL = 15 * time.Minute

	AdminCookieName   = "automata_admin"
	ProfileCookieName = "automata_profile"
	CSRFCookieName    = "automata_csrf"

	// PluginUITokenTTL borne la vie d'un jeton d'interface de plugin. Une
	// heure suffit à consulter un écran ; passé ce délai, recharger la
	// page en produit un neuf. Le jeton voyage dans un CHEMIN d'URL, et
	// une URL se retrouve dans les journaux du reverse proxy : sa durée
	// se compte en minutes, jamais en heures de session opérateur.
	PluginUITokenTTL = time.Hour
)

// Signer signe et vérifie les valeurs de cookies : payload |
// base64url(HMAC-SHA256(secret, payload)). Aucun état serveur : la session
// tient dans le cookie, l'expiration dans le payload signé.
type Signer struct {
	secret []byte
}

// NewSigner construit un signeur à partir du secret de session : le secret
// reste encapsulé, personne ne le relit depuis un autre paquet.
func NewSigner(secret string) Signer {
	return Signer{secret: []byte(secret)}
}

func (s Signer) Sign(payload string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	return payload + "|" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verify retourne le payload si la signature est valide.
func (s Signer) Verify(value string) (string, bool) {
	i := strings.LastIndexByte(value, '|')
	if i < 0 {
		return "", false
	}
	payload, sig := value[:i], value[i+1:]

	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return "", false
	}
	return payload, true
}

// SessionPayload compose « <kind>:<subject>:<expiration unix> » (le
// point-virgule est interdit dans une valeur de cookie). kind distingue
// les sessions admin des sessions de profil : un cookie signé de l'une ne
// vaut jamais pour l'autre.
func SessionPayload(kind, subject string, expires time.Time) string {
	return kind + ":" + base64.RawURLEncoding.EncodeToString([]byte(subject)) + ":" + strconv.FormatInt(expires.Unix(), 10)
}

// parseSession vérifie kind et l'expiration, et retourne le sujet.
func (s Signer) ParseSession(value, kind string, now time.Time) (subject string, expires time.Time, ok bool) {
	payload, valid := s.Verify(value)
	if !valid {
		return "", time.Time{}, false
	}

	parts := strings.Split(payload, ":")
	if len(parts) != 3 || parts[0] != kind {
		return "", time.Time{}, false
	}

	rawSubject, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", time.Time{}, false
	}
	unix, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", time.Time{}, false
	}

	expires = time.Unix(unix, 0)
	if !now.Before(expires) {
		return "", time.Time{}, false
	}

	return string(rawSubject), expires, true
}

// LoginLimiter borne les tentatives de connexion opérateur en mémoire :
// 5 échecs par fenêtre de 15 minutes, toutes origines confondues (un seul
// compte opérateur existe — inutile de distinguer par IP derrière un
// reverse proxy).
type LoginLimiter struct {
	mu       sync.Mutex
	failures []time.Time
}

const (
	loginMaxFailures = 5
	loginWindow      = 15 * time.Minute
)

// remaining retourne le nombre de tentatives restantes à now.
func (l *LoginLimiter) Remaining(now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	kept := l.failures[:0]
	for _, at := range l.failures {
		if now.Sub(at) < loginWindow {
			kept = append(kept, at)
		}
	}
	l.failures = kept

	return loginMaxFailures - len(l.failures)
}

// recordFailure enregistre un échec et retourne les tentatives restantes.
func (l *LoginLimiter) RecordFailure(now time.Time) int {
	l.mu.Lock()
	l.failures = append(l.failures, now)
	l.mu.Unlock()

	return l.Remaining(now)
}

// reset efface les échecs (connexion réussie).
func (l *LoginLimiter) Reset() {
	l.mu.Lock()
	l.failures = nil
	l.mu.Unlock()
}

// CheckPassword compare le mot de passe au hachage bcrypt configuré.
func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// HashPassword produit le hachage bcrypt d'un mot de passe opérateur
// (sous-commande « automata web hash-password »).
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("web: hachage du mot de passe: %w", err)
	}
	return string(hash), nil
}

// Jeton d'interface de plugin : ce qui authentifie les requêtes d'une
// iframe de plugin, à la place du cookie.
//
// L'iframe est sandboxée SANS allow-same-origin — le document du plugin
// obtient une origine opaque, et n'accède donc ni au DOM ni aux cookies de
// l'application. Le revers : le navigateur traite ce contexte comme tiers
// et n'envoie AUCUN cookie SameSite=Lax avec ses requêtes. Le proxy ne
// pouvait donc pas reconnaître la personne, et rendait l'écran « ce lien a
// déjà servi » à l'intérieur du cadre (constaté en production le
// 2026-08-23).
//
// Le jeton porte l'identité que le cookie ne peut plus porter : la vue
// (opérateur ou membre), l'organisation, le membre et le PLUGIN visé — un
// jeton ne vaut que pour l'interface pour laquelle il a été émis. Il vit
// dans le chemin et non en paramètre de requête, pour la même raison que
// l'organisation avant lui : une navigation relative du document du
// plugin — un lien, un formulaire — reste sous le préfixe et conserve son
// contexte, là où un « ?token= » se perdrait au premier POST.
//
// Émettre ce jeton ne décide de rien : l'activation du plugin et
// l'existence de l'organisation restent vérifiées à CHAQUE requête
// proxifiée. Un plugin désactivé entre-temps devient injoignable, jeton
// valide ou non.
func (s *Deps) PluginUIToken(view, orgID, memberID, name string, now time.Time) string {
	subject := view + "/" + orgID + "/" + memberID + "/" + name
	payload := SessionPayload("plugin-ui", subject, now.Add(PluginUITokenTTL))
	return base64.RawURLEncoding.EncodeToString([]byte(s.Signer.Sign(payload)))
}

// DraftPreviewTTL borne la vie d'un lien de prévisualisation de brouillon
// (route /d/). Même raisonnement que PluginUITokenTTL : le jeton voyage
// dans un chemin d'URL, donc dans les journaux du reverse proxy — une
// heure pour regarder son brouillon, puis l'agent en refait un.
const DraftPreviewTTL = time.Hour

// DraftPreviewMinter retourne la fabrique de liens de prévisualisation,
// construite sur les seuls secret de session et URL de base : le service
// hôte des plugins peut la recevoir en closure (via le registre) sans
// dépendre de ce paquet ni détenir le secret. Le sujet garde l'ordre
// plugin/org/membre/collection ; seuls les trois premiers segments sont
// sans « / », la collection récupère le reste au découpage.
func DraftPreviewMinter(sessionSecret, baseURL string) func(pluginName, orgID, memberID, collection string) (url string, expiresAt time.Time, err error) {
	sig := Signer{secret: []byte(sessionSecret)}
	base := strings.TrimRight(baseURL, "/")

	return func(pluginName, orgID, memberID, collection string) (string, time.Time, error) {
		for _, segment := range []string{pluginName, orgID, memberID} {
			if segment == "" || strings.Contains(segment, "/") {
				return "", time.Time{}, fmt.Errorf("web: sujet de prévisualisation invalide")
			}
		}
		if collection == "" {
			return "", time.Time{}, fmt.Errorf("web: collection de prévisualisation vide")
		}

		expires := time.Now().Add(DraftPreviewTTL)
		subject := pluginName + "/" + orgID + "/" + memberID + "/" + collection
		payload := SessionPayload("draft-preview", subject, expires)
		token := base64.RawURLEncoding.EncodeToString([]byte(sig.Sign(payload)))
		return base + "/d/" + token + "/", expires, nil
	}
}

// parseDraftPreviewToken vérifie un jeton de prévisualisation et rend ce
// qu'il désigne.
func (s *Deps) ParseDraftPreviewToken(token string, now time.Time) (pluginName, orgID, memberID, collection string, ok bool) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", "", "", "", false
	}

	subject, _, valid := s.Signer.ParseSession(string(raw), "draft-preview", now)
	if !valid {
		return "", "", "", "", false
	}

	parts := strings.SplitN(subject, "/", 4)
	if len(parts) != 4 || parts[0] == "" || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		return "", "", "", "", false
	}

	return parts[0], parts[1], parts[2], parts[3], true
}

// FileLinkTTL borne la vie d'un lien de téléchargement (route /f/). Vingt-
// quatre heures, alignées sur la durée de vie d'un espace de travail : un
// lien plus long désignerait un fichier déjà effacé, un lien plus court
// serait mort au réveil de celui qui l'a reçu la veille au soir.
//
// C'est plus large que DraftPreviewTTL, et le jeton voyage dans les mêmes
// journaux de reverse proxy. Le compromis tient à ce qu'il ouvre : UN
// chemin précis, dans l'espace de travail d'UN membre, et rien d'autre.
const FileLinkTTL = 24 * time.Hour

// FileLinkMinter retourne la fabrique de liens de téléchargement, sur le
// modèle de DraftPreviewMinter : une closure qui capture le secret, remise
// au service hôte des plugins par le registre, de sorte que ni
// internal/plugin ni le plugin lui-même ne le détiennent jamais.
//
// Le sujet garde l'ordre plugin/org/membre/chemin ; seuls les trois
// premiers segments sont sans « / », le chemin récupère le reste au
// découpage — un fichier peut vivre dans un sous-répertoire.
func FileLinkMinter(sessionSecret, baseURL string) func(pluginName, orgID, memberID, path string) (url string, expiresAt time.Time, err error) {
	sig := Signer{secret: []byte(sessionSecret)}
	base := strings.TrimRight(baseURL, "/")

	return func(pluginName, orgID, memberID, path string) (string, time.Time, error) {
		for _, segment := range []string{pluginName, orgID, memberID} {
			if segment == "" || strings.Contains(segment, "/") {
				return "", time.Time{}, fmt.Errorf("web: sujet de lien de fichier invalide")
			}
		}
		if path == "" {
			return "", time.Time{}, fmt.Errorf("web: chemin de fichier vide")
		}

		expires := time.Now().Add(FileLinkTTL)
		subject := pluginName + "/" + orgID + "/" + memberID + "/" + path
		payload := SessionPayload("file-link", subject, expires)
		token := base64.RawURLEncoding.EncodeToString([]byte(sig.Sign(payload)))

		return base + "/f/" + token, expires, nil
	}
}

// parseFileLinkToken vérifie un jeton de téléchargement et rend ce qu'il
// désigne.
func (s *Deps) ParseFileLinkToken(token string, now time.Time) (pluginName, orgID, memberID, path string, ok bool) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", "", "", "", false
	}

	subject, _, valid := s.Signer.ParseSession(string(raw), "file-link", now)
	if !valid {
		return "", "", "", "", false
	}

	parts := strings.SplitN(subject, "/", 4)
	if len(parts) != 4 || parts[0] == "" || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		return "", "", "", "", false
	}

	return parts[0], parts[1], parts[2], parts[3], true
}

// parsePluginUIToken vérifie un jeton et rend ce qu'il porte.
func (s *Deps) ParsePluginUIToken(token string, now time.Time) (view, orgID, memberID, name string, ok bool) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", "", "", "", false
	}

	subject, _, valid := s.Signer.ParseSession(string(raw), "plugin-ui", now)
	if !valid {
		return "", "", "", "", false
	}

	parts := strings.SplitN(subject, "/", 4)
	if len(parts) != 4 {
		return "", "", "", "", false
	}
	if parts[0] != PluginViewAdmin && parts[0] != PluginViewMember {
		return "", "", "", "", false
	}
	if parts[3] == "" {
		return "", "", "", "", false
	}

	return parts[0], parts[1], parts[2], parts[3], true
}

// SetSessionCookie pose un cookie de session signé. Secure est laissé au
// soin du reverse proxy TLS (l'adresse d'écoute est locale) ; SameSite
// Lax couvre les POST de même site tout en laissant l'ouverture des liens
// de profil depuis la messagerie fonctionner.
func SetSessionCookie(w http.ResponseWriter, name, value string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearSessionCookie efface un cookie de session.
func ClearSessionCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}
