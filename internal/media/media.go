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
// internal/audio, sans jamais être conservées (PLAN.md §3.4).
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
	// MaxHistory borne le nombre de pièces jointes rejouées depuis
	// l'historique à chaque tour. <= 0 : aucun rejeu.
	MaxHistory int
	// MaxReply borne le nombre de pièces jointes jointes à une réponse.
	// <= 0 : pas de borne.
	MaxReply int
}

// accepts indique si mimeType figure parmi les types acceptés.
func (c Config) accepts(mimeType string) bool {
	for _, accepted := range c.AcceptedTypes {
		if strings.EqualFold(strings.TrimSpace(accepted), mimeType) {
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

		if !cfg.Enabled {
			rejected = append(rejected, fmt.Sprintf("%s (%s) : pièces jointes désactivées", name, mimeType))
			continue
		}

		if len(kept) >= cfg.MaxCount {
			rejected = append(rejected, fmt.Sprintf("%s (%s) : au-delà de %d pièce(s) jointe(s) par message", name, mimeType, cfg.MaxCount))
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
		attachment, err := ToLLM(m)
		if err != nil {
			rejected = append(rejected, fmt.Sprintf("%s (%s) : conversion impossible", m.Filename, m.MimeType))
			continue
		}

		attachments = append(attachments, attachment)
	}

	return attachments, rejected
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

// defaultFilename fabrique un nom de fichier lorsqu'aucun n'est fourni : les
// plateformes de messagerie en exigent un pour afficher correctement une
// pièce jointe.
func defaultFilename(mimeType string) string {
	extensions, err := mime.ExtensionsByType(mimeType)
	if err == nil && len(extensions) > 0 {
		return "piece-jointe" + extensions[0]
	}

	return "piece-jointe"
}
