package web

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
	adminSessionTTL   = 12 * time.Hour
	profileSessionTTL = 15 * time.Minute

	adminCookieName   = "automata_admin"
	profileCookieName = "automata_profile"
	csrfCookieName    = "automata_csrf"

	// pluginUITokenTTL borne la vie d'un jeton d'interface de plugin. Une
	// heure suffit à consulter un écran ; passé ce délai, recharger la
	// page en produit un neuf. Le jeton voyage dans un CHEMIN d'URL, et
	// une URL se retrouve dans les journaux du reverse proxy : sa durée
	// se compte en minutes, jamais en heures de session opérateur.
	pluginUITokenTTL = time.Hour
)

// signer signe et vérifie les valeurs de cookies : payload |
// base64url(HMAC-SHA256(secret, payload)). Aucun état serveur : la session
// tient dans le cookie, l'expiration dans le payload signé.
type signer struct {
	secret []byte
}

func (s signer) sign(payload string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	return payload + "|" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verify retourne le payload si la signature est valide.
func (s signer) verify(value string) (string, bool) {
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

// sessionPayload compose « <kind>:<subject>:<expiration unix> » (le
// point-virgule est interdit dans une valeur de cookie). kind distingue
// les sessions admin des sessions de profil : un cookie signé de l'une ne
// vaut jamais pour l'autre.
func sessionPayload(kind, subject string, expires time.Time) string {
	return kind + ":" + base64.RawURLEncoding.EncodeToString([]byte(subject)) + ":" + strconv.FormatInt(expires.Unix(), 10)
}

// parseSession vérifie kind et l'expiration, et retourne le sujet.
func (s signer) parseSession(value, kind string, now time.Time) (subject string, expires time.Time, ok bool) {
	payload, valid := s.verify(value)
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

// loginLimiter borne les tentatives de connexion opérateur en mémoire :
// 5 échecs par fenêtre de 15 minutes, toutes origines confondues (un seul
// compte opérateur existe — inutile de distinguer par IP derrière un
// reverse proxy).
type loginLimiter struct {
	mu       sync.Mutex
	failures []time.Time
}

const (
	loginMaxFailures = 5
	loginWindow      = 15 * time.Minute
)

// remaining retourne le nombre de tentatives restantes à now.
func (l *loginLimiter) remaining(now time.Time) int {
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
func (l *loginLimiter) recordFailure(now time.Time) int {
	l.mu.Lock()
	l.failures = append(l.failures, now)
	l.mu.Unlock()

	return l.remaining(now)
}

// reset efface les échecs (connexion réussie).
func (l *loginLimiter) reset() {
	l.mu.Lock()
	l.failures = nil
	l.mu.Unlock()
}

// checkPassword compare le mot de passe au hachage bcrypt configuré.
func checkPassword(hash, password string) bool {
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
func (s *Server) pluginUIToken(view, orgID, memberID, name string, now time.Time) string {
	subject := view + "/" + orgID + "/" + memberID + "/" + name
	payload := sessionPayload("plugin-ui", subject, now.Add(pluginUITokenTTL))
	return base64.RawURLEncoding.EncodeToString([]byte(s.signer.sign(payload)))
}

// parsePluginUIToken vérifie un jeton et rend ce qu'il porte.
func (s *Server) parsePluginUIToken(token string, now time.Time) (view, orgID, memberID, name string, ok bool) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", "", "", "", false
	}

	subject, _, valid := s.signer.parseSession(string(raw), "plugin-ui", now)
	if !valid {
		return "", "", "", "", false
	}

	parts := strings.SplitN(subject, "/", 4)
	if len(parts) != 4 {
		return "", "", "", "", false
	}
	if parts[0] != pluginViewAdmin && parts[0] != pluginViewMember {
		return "", "", "", "", false
	}
	if parts[3] == "" {
		return "", "", "", "", false
	}

	return parts[0], parts[1], parts[2], parts[3], true
}

// setSessionCookie pose un cookie de session signé. Secure est laissé au
// soin du reverse proxy TLS (l'adresse d'écoute est locale) ; SameSite
// Lax couvre les POST de même site tout en laissant l'ouverture des liens
// de profil depuis la messagerie fonctionner.
func setSessionCookie(w http.ResponseWriter, name, value string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearSessionCookie efface un cookie de session.
func clearSessionCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}
