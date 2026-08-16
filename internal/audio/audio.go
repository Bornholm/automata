// Package audio transcrit les notes vocales sans jamais conserver le média
// brut ni, par défaut, la transcription elle-même (PLAN.md §3.4, Phase 9).
//
// Flux : contrôle de taille -> lecture en flux borné -> transcription ->
// suppression des octets -> traitement comme message textuel. Aucune étape
// de ce package ne journalise ni ne persiste le contenu audio ou transcrit :
// c'est à l'appelant (internal/conversation) de décider, selon la
// configuration, si le texte transcrit peut être conservé.
package audio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/bornholm/go-courier"
)

// Config décrit les paramètres de traitement audio consommés par ce package,
// dérivés de config.Audio (internal/config).
type Config struct {
	Enabled bool
	MaxSize int64
	Timeout time.Duration
}

// ErrTooLarge est retournée lorsque le flux audio dépasse Config.MaxSize.
var ErrTooLarge = errors.New("fichier audio trop volumineux")

// ErrUnsupportedFormat est retournée lorsque le format audio n'est pas
// reconnu par le détecteur GenAI.
var ErrUnsupportedFormat = errors.New("format audio non pris en charge")

// ErrEmptyTranscription est retournée lorsque la transcription obtenue est
// vide une fois débarrassée des espaces superflus.
var ErrEmptyTranscription = errors.New("transcription vide")

// Transcriber transcrit un flux audio complet en texte.
type Transcriber interface {
	Transcribe(ctx context.Context, audio []byte) (string, error)
}

// ExtractText lit attachment en flux borné (Config.MaxSize octets maximum,
// sous un timeout de Config.Timeout), puis appelle transcriber.Transcribe
// sur les octets lus. Aucune référence au buffer audio n'est conservée après
// le retour de cette fonction : l'appelant ne doit ni le journaliser ni le
// persister (voir le commentaire de package).
func ExtractText(ctx context.Context, cfg Config, transcriber Transcriber, attachment courier.Attachment) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	reader, err := attachment.Reader(ctx)
	if err != nil {
		return "", fmt.Errorf("audio: ouverture du flux de la pièce jointe: %w", err)
	}
	defer reader.Close()

	limited := io.LimitReader(reader, cfg.MaxSize+1)

	buf, err := io.ReadAll(limited)
	if err != nil {
		return "", fmt.Errorf("audio: lecture du flux de la pièce jointe: %w", err)
	}

	if int64(len(buf)) > cfg.MaxSize {
		return "", ErrTooLarge
	}

	text, err := transcriber.Transcribe(ctx, buf)
	if err != nil {
		return "", err
	}

	return text, nil
}

// FindAudio recherche la première pièce jointe audio du message : une note
// vocale enregistrée au micro, ou tout fichier de type MIME "audio/*"
// (PLAN.md Phase 9, travail 1 : « détecter VoiceNote OU les types MIME
// audio »).
//
// Les deux comptent : du point de vue de l'utilisateur, joindre un
// enregistrement plutôt que d'appuyer sur le bouton micro ne change pas
// l'intention, et n'a aucune raison de rendre le message inaudible pour
// l'assistant.
func FindAudio(msg courier.Message) (courier.Attachment, bool) {
	for _, part := range msg.Parts() {
		attachment, ok := part.(courier.Attachment)
		if !ok {
			continue
		}

		if courier.IsVoiceNote(part) || isAudioMIME(attachment.ContentType()) {
			return attachment, true
		}
	}

	return nil, false
}

// isAudioMIME reconnaît un type MIME audio, paramètres éventuels compris
// ("audio/ogg; codecs=opus").
func isAudioMIME(contentType string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "audio/")
}
