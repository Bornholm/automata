package main

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// Téléchargement de vidéos publiques : validation de l'URL côté plugin,
// avant même d'atteindre le bac à sable réseau de LeaSH.
//
// La frontière de sécurité reste la séparation des policies (l'atelier
// n'a pas le réseau, le sandbox « fetch » n'a que fetch-video). Ce qui
// suit est la couche de politique d'exploitation : QUELS domaines ce
// déploiement accepte de solliciter. Elle est ici et non dans le script
// parce qu'elle change avec l'exploitant, pas avec l'image.

// envDownloadDomains porte la liste blanche, séparée par des virgules
// (« youtube.com,vimeo.com »). Les sous-domaines d'un domaine listé sont
// acceptés. Vide : la liste par défaut ci-dessous.
const envDownloadDomains = "WORKSPACE_DOWNLOAD_DOMAINS"

// defaultDownloadDomains : les plateformes que l'on peut nommer sans
// deviner l'usage. Toute autre destination demande un choix explicite de
// l'exploitant — c'est ce qui empêche un agent, guidé par un contenu
// tiers, d'aller solliciter n'importe quelle URL au nom du membre.
var defaultDownloadDomains = []string{
	"youtube.com",
	"youtu.be",
	"vimeo.com",
	"dailymotion.com",
}

// downloadDomains lit la liste blanche effective.
func downloadDomains() []string {
	raw := strings.TrimSpace(os.Getenv(envDownloadDomains))
	if raw == "" {
		return defaultDownloadDomains
	}

	var domains []string
	for _, part := range strings.Split(raw, ",") {
		if domain := strings.ToLower(strings.TrimSpace(part)); domain != "" {
			domains = append(domains, domain)
		}
	}
	if len(domains) == 0 {
		return defaultDownloadDomains
	}
	return domains
}

// outputNamePattern borne le nom de fichier demandé : ce qui atterrit dans
// le workspace est ensuite manipulé par des scripts.
var outputNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// validateDownloadURL vérifie schéma, hôte et appartenance à la liste
// blanche. Le texte d'erreur part au modèle : anglais, actionnable.
func validateDownloadURL(raw string, domains []string) (string, error) {
	raw = strings.TrimSpace(raw)

	// Un modèle recopie volontiers le lien tel qu'il s'affiche, sans
	// schéma (« facebook.com/share/… ») : le refuser ferait échouer la
	// demande pour une broutille de forme. On complète en https, jamais
	// en http — compléter vers le moins sûr des deux serait un choix
	// silencieux à la place de l'utilisateur.
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("the URL could not be parsed")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("only http and https URLs can be downloaded")
	}

	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", fmt.Errorf("the URL has no host")
	}
	// Une adresse littérale ne peut pas appartenir à la liste blanche, et
	// c'est la forme qu'aurait une tentative d'atteindre un service
	// interne. Refus explicite plutôt que « domaine non autorisé », qui
	// inviterait le modèle à réessayer.
	if net.ParseIP(host) != nil {
		return "", fmt.Errorf("numeric host addresses cannot be downloaded")
	}

	for _, domain := range domains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return parsed.String(), nil
		}
	}

	return "", fmt.Errorf("downloads are limited to these sites: %s. Ask the user to send the file directly if the video is elsewhere",
		strings.Join(domains, ", "))
}

// validateOutputName contrôle le nom de sortie, « video » par défaut.
func validateOutputName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "video", nil
	}
	if !outputNamePattern.MatchString(name) {
		return "", fmt.Errorf("the output name may only contain letters, digits, dashes and underscores")
	}
	return name, nil
}

// downloadFailureAdvice traduit une sortie d'échec de yt-dlp en consigne
// pour le modèle.
//
// Sans elle, l'agent lit « This video is not available », en conclut que
// l'URL est mauvaise, et rejoue la même vidéo sous une douzaine de formes
// (youtu.be, /watch, /shorts, /embed, m.youtube…). Vu en production :
// quatorze appels pour rien, puis une explication FAUSSE donnée à
// l'utilisateur — « la vidéo est privée ou géo-restreinte » — alors que la
// vidéo était publique et la panne du côté de l'outil.
//
// Retourne "" quand rien de connu n'est reconnu : mieux vaut laisser
// passer la sortie brute que d'inventer une explication à notre tour.
func downloadFailureAdvice(output string) string {
	switch {
	// L'image manque du runtime JavaScript dont yt-dlp a besoin pour
	// résoudre le défi de signature de YouTube. Le message de yt-dlp est
	// trompeur : il parle de disponibilité, c'est une panne d'outil.
	case strings.Contains(output, "No supported JavaScript runtime"):
		return "This is a fault in the download tool itself, not a problem with the URL: " +
			"the video platform requires a JavaScript runtime that this server is missing. " +
			"Do NOT retry, and do NOT try other forms of the same link — every one of them will fail the same way. " +
			"Tell the user the download is broken on our side and that the operator has been given the details, " +
			"and offer to work on the video if they send it as an attachment instead."

	// Le format demandé n'existe pas, ou rien de vidéo n'est sorti :
	// souvent le même défaut de fond, parfois une vidéo réellement sans
	// piste téléchargeable.
	case strings.Contains(output, "Requested format is not available"),
		strings.Contains(output, "no video file was produced"):
		return "The platform did not offer any downloadable video track for this link. " +
			"Do NOT try other forms of the same link: they lead to the same page and will fail identically. " +
			"Ask the user to send the video as an attachment instead."

	// Vraie indisponibilité : là, une autre URL a du sens.
	case strings.Contains(output, "Private video"),
		strings.Contains(output, "Sign in to confirm"),
		strings.Contains(output, "members-only"),
		strings.Contains(output, "age-restricted"):
		return "This video is genuinely not publicly accessible (private, age-restricted, or members-only). " +
			"Do not retry: ask the user for a public link, or for the file as an attachment."
	}

	return ""
}
