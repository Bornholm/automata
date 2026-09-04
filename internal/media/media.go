// Package media transporte les pièces jointes d'une conversation, de leur
// réception (go-courier) jusqu'au modèle (genai) et retour.
//
// Il ne décide jamais seul de ce qui est transmis : la configuration
// (config.Attachments) fixe les types acceptés, la taille maximale et le
// nombre maximal de pièces par message. Ce qui est écarté n'est jamais
// silencieux — Extract retourne la description des rejets, que
// internal/conversation ajoute au texte remis à l'agent, afin qu'il puisse
// l'expliquer à l'utilisateur plutôt que de répondre à côté.
//
// Les notes vocales ne passent PAS par ici : elles restent transcrites par
// internal/audio, sans jamais être conservées (plan de conception, §3.4).
package media

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"strings"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/go-courier"
)

// Kind classe une pièce jointe selon ce que le modèle peut en faire. Il
// reprend les catégories de genai (llm.AttachmentType), auxquelles il est
// converti sans perte.
type Kind string

const (
	KindImage    Kind = "image"
	KindDocument Kind = "document"
	KindAudio    Kind = "audio"
	KindVideo    Kind = "video"
)

// Media est une pièce jointe applicative, indépendante du transport comme du
// fournisseur de modèle.
//
// Data porte les octets en clair : ils sont bornés par Config.MaxSize à
// l'extraction, et ne doivent jamais être journalisés (AGENTS.md).
type Media struct {
	Kind     Kind
	MimeType string
	Filename string
	Caption  string
	Data     []byte
	// ToolOnly marque une pièce jointe retenue POUR LES OUTILS et jamais
	// transmise au modèle. Un fournisseur texte-seul refuse la requête
	// entière quand elle porte une vidéo ; un fournisseur multimodal la
	// facturerait pour rien. La pièce reste disponible à l'agent par sa
	// description textuelle (ToolOnlyNotice), et un outil hôte va en
	// chercher les octets quand il en a besoin.
	ToolOnly bool
}

// Config décrit les limites appliquées aux pièces jointes, dérivée de
// config.Attachments (internal/config).
type Config struct {
	// Enabled à false ignore toute pièce jointe (hors notes vocales, gérées
	// par internal/audio) : Extract ne retourne alors aucun média, et
	// signale les pièces écartées.
	Enabled bool
	// MaxSize borne la taille d'UNE pièce jointe, en octets.
	MaxSize int64
	// MaxCount borne le nombre de pièces jointes retenues par message.
	MaxCount int
	// AcceptedTypes énumère les types MIME transmissibles au modèle. Vide :
	// aucun type accepté (défaut sûr, jamais "tout accepter").
	//
	// Ce filtre est indispensable et ne peut pas être délégué au
	// fournisseur : celui-ci REFUSE la requête entière lorsqu'une pièce
	// jointe ne lui convient pas (voir la validation par provider dans
	// genai), si bien qu'un simple PDF ferait échouer tout le tour et
	// laisserait l'utilisateur sans réponse. Mieux vaut écarter la pièce et
	// le lui dire.
	AcceptedTypes []string
	// ToolTypes énumère les types MIME retenus à l'extraction pour les
	// OUTILS seulement : ils ne partent jamais au modèle. C'est ce qui
	// permet de recevoir une vidéo par messagerie et de la faire traiter
	// par un sous-agent, sans qu'elle fasse échouer le tour chez un
	// fournisseur qui ne sait pas la lire.
	ToolTypes []string
	// MaxToolSize borne la taille d'une pièce jointe ToolTypes, en octets.
	// Distincte de MaxSize : ces pièces ne coûtent pas de jetons, elles
	// peuvent donc être bien plus grosses qu'une image envoyée au modèle.
	MaxToolSize int64
	// MaxHistory borne le nombre de pièces jointes rejouées depuis
	// l'historique à chaque tour. <= 0 : aucun rejeu.
	MaxHistory int
	// MaxReply borne le nombre de pièces jointes jointes à une réponse.
	// <= 0 : pas de borne.
	MaxReply int
	// SkipAudio écarte silencieusement toute pièce jointe audio, sans la
	// signaler comme rejetée : elle est prise en charge ailleurs, par la
	// transcription (internal/audio). Sans cela, un fichier audio accepté
	// par AcceptedTypes serait à la fois transcrit ET transmis au modèle.
	SkipAudio bool
}

// accepts indique si mimeType figure parmi les types acceptés.
func (c Config) accepts(mimeType string) bool {
	return containsMIME(c.AcceptedTypes, mimeType)
}

// acceptsForTools indique si mimeType est retenu pour les outils seulement.
func (c Config) acceptsForTools(mimeType string) bool {
	return containsMIME(c.ToolTypes, mimeType)
}

func containsMIME(list []string, mimeType string) bool {
	for _, candidate := range list {
		if strings.EqualFold(strings.TrimSpace(candidate), mimeType) {
			return true
		}
	}

	return false
}

// KindFromMIME classe un type MIME. Le booléen est faux pour un type que
// genai ne sait pas représenter.
func KindFromMIME(mimeType string) (Kind, bool) {
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return KindImage, true
	case strings.HasPrefix(mimeType, "audio/"):
		return KindAudio, true
	case strings.HasPrefix(mimeType, "video/"):
		return KindVideo, true
	case strings.HasPrefix(mimeType, "text/"), mimeType == "application/pdf":
		return KindDocument, true
	default:
		return "", false
	}
}

// normalizeMIME retire les paramètres d'un type MIME ("image/jpeg;
// charset=..." -> "image/jpeg") et le passe en minuscules. Un type illisible
// est retourné tel quel, débarrassé de ses espaces : c'est au filtre
// AcceptedTypes de l'écarter, pas à cette fonction d'en inventer un.
func normalizeMIME(contentType string) string {
	parsed, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(contentType))
	}

	return strings.ToLower(parsed)
}

// Extract lit les pièces jointes de msg exploitables par le modèle et
// retourne, séparément, celles qui ont été retenues et la description en
// clair de celles qui ont été écartées (type refusé, taille dépassée, quota
// atteint).
//
// Les notes vocales sont ignorées : internal/audio les transcrit.
//
// Une pièce jointe illisible n'interrompt jamais le traitement du message :
// elle rejoint les rejets décrits. Perdre une image ne justifie pas de perdre
// la conversation.
func Extract(ctx context.Context, msg courier.Message, cfg Config) ([]Media, []string) {
	attachments := courier.Attachments(msg)
	if len(attachments) == 0 {
		return nil, nil
	}

	var (
		kept     []Media
		rejected []string
	)

	for _, attachment := range attachments {
		if courier.IsVoiceNote(attachment) {
			continue
		}

		name := courier.FilenameFor(attachment)
		mimeType := normalizeMIME(attachment.ContentType())

		// Pris en charge par la transcription, pas ici : ce n'est pas un
		// rejet, il n'y a donc rien à signaler à l'agent.
		if cfg.SkipAudio && strings.HasPrefix(mimeType, "audio/") {
			continue
		}

		if !cfg.Enabled {
			rejected = append(rejected, fmt.Sprintf("%s (%s) : pièces jointes désactivées", name, mimeType))
			continue
		}

		if len(kept) >= cfg.MaxCount {
			rejected = append(rejected, fmt.Sprintf("%s (%s) : au-delà de %d pièce(s) jointe(s) par message", name, mimeType, cfg.MaxCount))
			continue
		}

		// Les types « outillage seulement » sont testés en premier : ils ont
		// leur propre borne de taille, et ne doivent surtout pas retomber
		// dans le chemin qui convertit la pièce pour le modèle.
		if cfg.acceptsForTools(mimeType) {
			m, err := extractToolOnly(ctx, attachment, name, mimeType, cfg.MaxToolSize)
			if err != nil {
				rejected = append(rejected, fmt.Sprintf("%s (%s) : %v", name, mimeType, err))
				continue
			}
			kept = append(kept, m)
			continue
		}

		if !cfg.accepts(mimeType) {
			rejected = append(rejected, fmt.Sprintf("%s (%s) : type non pris en charge", name, mimeType))
			continue
		}

		kind, ok := KindFromMIME(mimeType)
		if !ok {
			rejected = append(rejected, fmt.Sprintf("%s (%s) : type non pris en charge", name, mimeType))
			continue
		}

		// Taille annoncée par la plateforme : évite de lire un flux dont on
		// sait déjà qu'il sera écarté. Size() vaut -1 quand elle est
		// inconnue, auquel cas seule la lecture bornée ci-dessous tranche.
		if size := attachment.Size(); size > 0 && size > cfg.MaxSize {
			rejected = append(rejected, fmt.Sprintf("%s (%s) : dépasse %d octets", name, mimeType, cfg.MaxSize))
			continue
		}

		data, err := readBounded(ctx, attachment, cfg.MaxSize)
		if err != nil {
			rejected = append(rejected, fmt.Sprintf("%s (%s) : %v", name, mimeType, err))
			continue
		}

		kept = append(kept, Media{
			Kind:     kind,
			MimeType: mimeType,
			Filename: name,
			Caption:  attachment.Caption(),
			Data:     data,
		})
	}

	return kept, rejected
}

// extractToolOnly lit une pièce jointe destinée aux seuls outils. Son type
// n'a pas besoin d'être représentable par genai (KindFromMIME) : elle ne
// sera jamais convertie en llm.Attachment. Faute de classement, on retient
// KindDocument, qui n'est utilisé que si la pièce ressort vers la
// messagerie.
func extractToolOnly(ctx context.Context, attachment courier.Attachment, name, mimeType string, maxToolSize int64) (Media, error) {
	if size := attachment.Size(); size > 0 && size > maxToolSize {
		return Media{}, fmt.Errorf("dépasse %d octets", maxToolSize)
	}

	data, err := readBounded(ctx, attachment, maxToolSize)
	if err != nil {
		return Media{}, err
	}

	kind, ok := KindFromMIME(mimeType)
	if !ok {
		kind = KindDocument
	}

	return Media{
		Kind:     kind,
		MimeType: mimeType,
		Filename: name,
		Caption:  attachment.Caption(),
		Data:     data,
		ToolOnly: true,
	}, nil
}

// readBounded lit au plus maxSize octets du flux de attachment, et refuse
// tout contenu qui dépasse cette limite plutôt que de le tronquer : une image
// tronquée serait illisible pour le modèle, et un document tronqué le
// tromperait.
func readBounded(ctx context.Context, attachment courier.Attachment, maxSize int64) ([]byte, error) {
	reader, err := attachment.Reader(ctx)
	if err != nil {
		return nil, fmt.Errorf("ouverture impossible")
	}
	defer func() {
		_ = reader.Close()
	}()

	data, err := io.ReadAll(io.LimitReader(reader, maxSize+1))
	if err != nil {
		return nil, fmt.Errorf("lecture impossible")
	}

	if int64(len(data)) > maxSize {
		return nil, fmt.Errorf("dépasse %d octets", maxSize)
	}

	return data, nil
}

// ToLLM convertit m en pièce jointe genai, encodée en base64.
//
// Le fournisseur applique ensuite sa propre validation (types et tailles
// admis par le modèle visé) : ToLLM ne la duplique pas, c'est le filtre
// AcceptedTypes qui doit avoir écarté en amont ce que le fournisseur
// refuserait.
func ToLLM(m Media) (llm.Attachment, error) {
	data := base64.StdEncoding.EncodeToString(m.Data)

	var (
		attachment llm.Attachment
		err        error
	)

	switch m.Kind {
	case KindImage:
		attachment, err = llm.NewImageAttachment(m.MimeType, data, false)
	case KindDocument:
		attachment, err = llm.NewDocumentAttachment(m.MimeType, data, false)
	case KindAudio:
		attachment, err = llm.NewAudioAttachment(m.MimeType, data, false)
	case KindVideo:
		attachment, err = llm.NewVideoAttachment(m.MimeType, data, false)
	default:
		return nil, fmt.Errorf("media: type de pièce jointe inconnu %q", m.Kind)
	}

	if err != nil {
		return nil, fmt.Errorf("media: conversion de la pièce jointe %q (%s): %w", m.Filename, m.MimeType, err)
	}

	return attachment, nil
}

// ToLLMAll convertit les médias transmissibles au modèle, en ignorant ceux
// qu'il ne saurait pas représenter.
//
// Une conversion qui échoue n'interrompt pas le tour : le média est écarté et
// sa description rejoint les rejets retournés, comme à l'extraction.
func ToLLMAll(medias []Media) ([]llm.Attachment, []string) {
	if len(medias) == 0 {
		return nil, nil
	}

	var (
		attachments []llm.Attachment
		rejected    []string
	)

	for _, m := range medias {
		// Écartée sans être signalée comme rejet : elle n'est pas refusée,
		// elle est simplement invisible du modèle, par construction.
		if m.ToolOnly {
			continue
		}

		attachment, err := ToLLM(m)
		if err != nil {
			rejected = append(rejected, fmt.Sprintf("%s (%s) : conversion impossible", m.Filename, m.MimeType))
			continue
		}

		attachments = append(attachments, attachment)
	}

	return attachments, rejected
}

// ToolOnlyNotice décrit en texte les pièces jointes retenues pour les
// outils, afin que l'agent sache quels fichiers il peut importer. Nom,
// type et taille seulement — jamais le contenu.
//
// En anglais : ce texte part vers le modèle (AGENTS.md).
func ToolOnlyNotice(medias []Media) string {
	var lines []string
	for _, m := range medias {
		if !m.ToolOnly {
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s (%s, %d bytes)", m.Filename, m.MimeType, len(m.Data)))
	}

	if len(lines) == 0 {
		return ""
	}

	return "\n\n[Files attached to this message. You cannot see them directly; " +
		"use the file tools to work on them, referring to each by its exact filename:\n" +
		strings.Join(lines, "\n") + "]"
}

// AttachedFilesNotice décrit TOUTES les pièces jointes du message courant,
// réservées aux outils ou non, pour un agent qui travaille sur des fichiers
// par leur nom. Nom, type et taille seulement — jamais le contenu.
//
// Distinct de ToolOnlyNotice : celui-ci s'adresse à un agent dont le modèle
// voit les images et n'a besoin d'être renseigné que sur ce qu'il ne voit
// pas. Un agent à outils fichiers, lui, a besoin du nom EXACT de chaque
// pièce, y compris de celles qu'un modèle multimodal aurait pu regarder :
// sans le nom, il ne peut pas les importer.
//
// En anglais : ce texte part vers le modèle (AGENTS.md).
func AttachedFilesNotice(medias []Media) string {
	if len(medias) == 0 {
		return ""
	}

	lines := make([]string, 0, len(medias))
	for _, m := range medias {
		lines = append(lines, fmt.Sprintf("- %s (%s, %d bytes)", m.Filename, m.MimeType, len(m.Data)))
	}

	return "\n\n[Files attached to the message you are answering. You cannot see them directly; " +
		"import them with your file tools, using each exact filename:\n" +
		strings.Join(lines, "\n") + "]"
}

// DelegableFilesNotice décrit à un orchestrateur les fichiers déjà reçus
// dans la conversation, pour qu'il sache qu'ils restent traitables par un
// spécialiste — qu'il édite des fichiers ou qu'il regarde des images. Sans cette note, il ne voit que le message courant : « voici
// une photo » puis « enlève le logo » lui donne un message sans pièce
// jointe, et il répond à l'utilisateur qu'il n'a rien reçu au lieu de
// déléguer.
//
// Nom, type et taille seulement — jamais le contenu, que l'orchestrateur ne
// reçoit de toute façon pas.
//
// En anglais : ce texte part vers le modèle (AGENTS.md).
func DelegableFilesNotice(medias []Media) string {
	if len(medias) == 0 {
		return ""
	}

	lines := make([]string, 0, len(medias))
	for _, m := range medias {
		lines = append(lines, fmt.Sprintf("- %s (%s, %d bytes)", m.Filename, m.MimeType, len(m.Data)))
	}

	return "\n\n[Files already received earlier in this conversation, most recent first:\n" +
		strings.Join(lines, "\n") + "\n" +
		"You cannot see them, but a specialist that sees images or works on files can still examine or edit them. " +
		"If the user refers to one of these, delegate instead of telling them you received nothing.]"
}

// EarlierFilesNotice décrit les fichiers reçus lors des messages
// PRÉCÉDENTS de la conversation, pour qu'un délégué sache qu'il peut les
// importer par leur nom. Nom, type et taille seulement — jamais le
// contenu.
//
// Distinct de ToolOnlyNotice à dessein : dire « joint à ce message » d'un
// fichier vieux de trois messages ferait chercher le délégué au mauvais
// endroit, et lui ferait relancer l'utilisateur pour rien.
//
// En anglais : ce texte part vers le modèle (AGENTS.md).
func EarlierFilesNotice(medias []Media) string {
	if len(medias) == 0 {
		return ""
	}

	lines := make([]string, 0, len(medias))
	for _, m := range medias {
		lines = append(lines, fmt.Sprintf("- %s (%s, %d bytes)", m.Filename, m.MimeType, len(m.Data)))
	}

	return "\n\n[Files the user sent earlier in this conversation, most recent first. " +
		"You cannot see them directly, but your file tools can import them by their exact filename:\n" +
		strings.Join(lines, "\n") + "]"
}

// ToolOnly retourne les pièces jointes réservées aux outils.
func ToolOnly(medias []Media) []Media {
	var out []Media
	for _, m := range medias {
		if m.ToolOnly {
			out = append(out, m)
		}
	}

	return out
}

// ToCourier convertit m en pièce jointe sortante go-courier, prête à être
// jointe à un message envoyé à l'utilisateur.
func ToCourier(m Media) courier.Attachment {
	return courier.NewAttachment(
		m.Filename,
		m.MimeType,
		courier.OpenerFromBytes(m.Data),
		courier.WithAttachmentSize(int64(len(m.Data))),
		courier.WithAttachmentCaption(m.Caption),
	)
}

// FromLLM convertit une pièce jointe produite par un outil (résultat MCP) en
// média applicatif, afin de pouvoir être renvoyée à l'utilisateur.
//
// Seules les pièces jointes en base64 sont converties : une pièce jointe
// désignée par URL n'est pas téléchargée ici, ce serait une requête sortante
// vers un hôte choisi par un tiers, décidée hors de toute politique réseau.
func FromLLM(attachment llm.Attachment, filename string) (Media, bool) {
	if attachment.Source() != llm.AttachmentSourceBase64 {
		return Media{}, false
	}

	mimeType := normalizeMIME(attachment.MimeType())

	kind, ok := KindFromMIME(mimeType)
	if !ok {
		return Media{}, false
	}

	data, err := base64.StdEncoding.DecodeString(stripDataURLPrefix(attachment.Data()))
	if err != nil {
		return Media{}, false
	}

	if filename == "" {
		filename = defaultFilename(mimeType)
	}

	return Media{
		Kind:     kind,
		MimeType: mimeType,
		Filename: filename,
		Data:     data,
	}, true
}

// stripDataURLPrefix retire l'éventuel préfixe "data:<mime>;base64," d'une
// donnée encodée, forme que produisent certains serveurs MCP.
func stripDataURLPrefix(data string) string {
	if !strings.HasPrefix(data, "data:") {
		return data
	}

	if _, encoded, found := strings.Cut(data, ","); found {
		return encoded
	}

	return data
}

// usualExtensions fixe l'extension des types les plus courants.
// mime.ExtensionsByType retourne une liste triée par ordre alphabétique, dont
// la première entrée est souvent marginale : « .jfif » pour une image JPEG,
// « .asc » pour du texte brut, « .markdown » pour du markdown. Un
// destinataire humain lit ce nom, et certaines
// messageries se fient à l'extension pour prévisualiser le fichier.
var usualExtensions = map[string]string{
	"image/jpeg":    ".jpg",
	"text/plain":    ".txt",
	"audio/mpeg":    ".mp3",
	"text/markdown": ".md",
}

// DefaultFilename est la version exportée de defaultFilename, pour les
// appelants qui reçoivent un fichier sans nom (outils de plugin).
//
// defaultFilename fabrique un nom de fichier lorsqu'aucun n'est fourni : les
// plateformes de messagerie en exigent un pour afficher correctement une
// pièce jointe.
func DefaultFilename(mimeType string) string {
	return defaultFilename(mimeType)
}

func defaultFilename(mimeType string) string {
	if extension, found := usualExtensions[normalizeMIME(mimeType)]; found {
		return "piece-jointe" + extension
	}

	extensions, err := mime.ExtensionsByType(mimeType)
	if err == nil && len(extensions) > 0 {
		return "piece-jointe" + extensions[0]
	}

	return "piece-jointe"
}
