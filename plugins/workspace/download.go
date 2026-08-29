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
	parsed, err := url.Parse(strings.TrimSpace(raw))
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
