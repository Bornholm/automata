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

	// Plafond d'exploitation atteint : ni une panne ni un problème d'URL.
	// L'agent doit le dire tel quel, sans chercher d'autre lien.
	case strings.Contains(output, "larger than max-filesize"),
		strings.Contains(output, "does not pass filter"):
		return "The video was refused by this server's limits, not by the platform: it is too large or too long. " +
			"Do not retry with another link to the same video. " +
			"Tell the user plainly that this video exceeds what the workspace accepts."

	// Aucun sous-titre : ce n'est pas une panne, c'est une propriété de la
	// vidéo. Sans consigne, l'agent enchaîne les formes d'URL comme il le
	// faisait pour les vidéos indisponibles.
	case strings.Contains(output, "no subtitles are available"):
		return "This video carries no subtitles at all, not even automatic ones. This is a property of the video, not a fault: " +
			"do NOT retry with another form of the same link. " +
			"You may try ONCE more with a different 'lang' if the video is likely spoken in another language. " +
			"Otherwise tell the user plainly that this video has no subtitles, so there is no text to summarise."

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

// fetchCapability décrit un wrapper réseau exposé comme outil du plugin.
//
// Ce qui suit est vrai de TOUT téléchargement : tolérer les synonymes de
// paramètres qu'un modèle invente, valider l'URL contre la liste blanche,
// borner le nom de sortie, journaliser sans l'URL, traduire l'échec de
// yt-dlp en consigne. Seuls le script appelé et les phrases rendues au
// modèle changent d'une capacité à l'autre. Ajouter une capacité, c'est
// donc une entrée dans ce tableau et un script dans misc/toolbox — jamais
// une branche de plus dans CallTool.
//
// Le script, lui, reste étroit et propre à sa capacité, et c'est
// délibéré : `allowed_binaries` de policies/fetch.yaml est l'inventaire
// lisible de ce que l'agent peut atteindre sur le réseau. Une capacité
// réseau qui s'ajouterait sans que cette policy change s'ajouterait sans
// revue.
type fetchCapability struct {
	// Tool est le nom exposé au modèle.
	Tool string
	// Script est le binaire autorisé par la policy « fetch ». Il reçoit
	// l'URL, puis le nom de sortie, puis les Params dans l'ordre.
	Script string
	// Purpose ouvre la description de l'outil ; la liste des domaines
	// autorisés y est ajoutée à la volée, elle vient de la configuration
	// de l'exploitant.
	Purpose string
	// Params sont les arguments propres à la capacité, au-delà de l'URL et
	// du nom de sortie que toutes partagent.
	Params []fetchParam
	// Success est ajouté à la sortie du script quand il a réussi : il dit
	// au modèle quoi faire du fichier obtenu.
	Success string
}

// fetchParam est un argument propre à une capacité. Le motif borne ce qui
// atteint la ligne de commande du script ; le défaut évite d'exiger du
// modèle une valeur qu'il devinerait mal.
type fetchParam struct {
	Name        string
	Description string
	Default     string
	Pattern     *regexp.Regexp
}

// langPattern borne une liste de langues (« fr », « fr,en », « pt-BR »).
var langPattern = regexp.MustCompile(`^[A-Za-z]{2,8}(-[A-Za-z0-9]{2,8})*(,[A-Za-z]{2,8}(-[A-Za-z0-9]{2,8})*)*$`)

// fetchCapabilities énumère les téléchargements offerts quand l'exploitant
// a configuré la clé de la policy réseau.
var fetchCapabilities = []fetchCapability{
	{
		Tool:   "download_video",
		Script: "fetch-video",
		Purpose: "Download a public video into your workspace so you can edit it. " +
			"Playlists are not downloaded, only a single video, capped in size and duration.",
		Success: "\nThe file is now in your workspace: call list_files to see its exact name before working on it.",
	},
	{
		Tool:   "download_subtitles",
		Script: "fetch-subtitles",
		Purpose: "Download the subtitles of a public video as a text file, WITHOUT downloading the video itself. " +
			"This is the way to answer a question about what a video SAYS — summarise it, search it, quote it: " +
			"it takes seconds where downloading the video would take minutes, and there is nothing to watch afterwards. " +
			"Automatic subtitles carry no punctuation and never name the speakers, so never attribute a sentence to anyone based on them.",
		Params: []fetchParam{{
			Name:        "lang",
			Description: "Preferred subtitle languages, comma separated. The first one the video offers is used.",
			Default:     "fr,en",
			Pattern:     langPattern,
		}},
		Success: "\nThe subtitle file is now in your workspace. It is raw VTT: load the summarize-video-from-subtitles skill and follow it. " +
			"Do NOT cat the file as it is — automatic subtitles repeat every line two or three times and would flood your context for nothing.",
	},
}

// lookupFetchCapability retrouve une capacité par le nom de son outil.
func lookupFetchCapability(tool string) (fetchCapability, bool) {
	for _, capability := range fetchCapabilities {
		if capability.Tool == tool {
			return capability, true
		}
	}
	return fetchCapability{}, false
}

// validateParam applique défaut et motif à un argument propre à une
// capacité. Le texte d'erreur part au modèle : anglais, actionnable.
func (p fetchParam) validate(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return p.Default, nil
	}
	if p.Pattern != nil && !p.Pattern.MatchString(value) {
		return "", fmt.Errorf("the '%s' parameter is malformed; leave it out to use %q", p.Name, p.Default)
	}
	return value, nil
}
