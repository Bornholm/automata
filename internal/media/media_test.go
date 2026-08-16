package media_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/go-courier"

	"github.com/bornholm/automata/internal/media"
)

// defaultTestConfig autorise les types d'image usuels, avec des limites
// larges : chaque test resserre ce dont il a besoin.
func defaultTestConfig() media.Config {
	return media.Config{
		Enabled:       true,
		MaxSize:       1024,
		MaxCount:      3,
		AcceptedTypes: []string{"image/png", "image/jpeg", "text/plain"},
	}
}

// attachmentPart construit une pièce jointe de test portant data.
func attachmentPart(filename, contentType string, data []byte, opts ...courier.AttachmentOptionFunc) courier.Attachment {
	open := func(ctx context.Context) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	}

	opts = append([]courier.AttachmentOptionFunc{courier.WithAttachmentSize(int64(len(data)))}, opts...)

	return courier.NewAttachment(filename, contentType, open, opts...)
}

// messageWith construit un message porteur des parties fournies.
func messageWith(parts ...courier.MessagePart) courier.Message {
	opts := make([]courier.BaseMessageOptionFunc, 0, len(parts))
	for _, p := range parts {
		opts = append(opts, courier.WithMessagePart(p))
	}

	return courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannelRef("chan-1"),
		courier.NewUser("alice", "alice"),
		opts...,
	)
}

func TestExtract_AcceptedImage(t *testing.T) {
	data := []byte("octets png")
	msg := messageWith(attachmentPart("photo.png", "image/png", data))

	kept, rejected := media.Extract(context.Background(), msg, defaultTestConfig())

	if len(rejected) != 0 {
		t.Fatalf("aucun rejet attendu, obtenu %v", rejected)
	}
	if len(kept) != 1 {
		t.Fatalf("pièces jointes retenues: got %d, expected 1", len(kept))
	}

	got := kept[0]
	if got.Kind != media.KindImage {
		t.Errorf("kind = %q, attendu image", got.Kind)
	}
	if got.MimeType != "image/png" {
		t.Errorf("mime_type = %q, attendu image/png", got.MimeType)
	}
	if got.Filename != "photo.png" {
		t.Errorf("filename = %q, attendu photo.png", got.Filename)
	}
	if !bytes.Equal(got.Data, data) {
		t.Errorf("données altérées: %q", got.Data)
	}
}

// TestExtract_MimeTypeWithParameters vérifie qu'un type MIME paramétré, tel
// que l'envoient certaines plateformes, est reconnu.
func TestExtract_MimeTypeWithParameters(t *testing.T) {
	msg := messageWith(attachmentPart("note.txt", "text/plain; charset=utf-8", []byte("bonjour")))

	kept, rejected := media.Extract(context.Background(), msg, defaultTestConfig())

	if len(kept) != 1 {
		t.Fatalf("pièces jointes retenues: got %d, expected 1 (rejets: %v)", len(kept), rejected)
	}
	if kept[0].MimeType != "text/plain" {
		t.Errorf("mime_type = %q, attendu text/plain (paramètres retirés)", kept[0].MimeType)
	}
}

// TestExtract_UnsupportedTypeRejected vérifie qu'un type non accepté est
// écarté ET signalé : le silence ferait répondre l'agent à côté d'un document
// qu'il n'a jamais vu.
func TestExtract_UnsupportedTypeRejected(t *testing.T) {
	msg := messageWith(attachmentPart("contrat.pdf", "application/pdf", []byte("%PDF-1.4")))

	kept, rejected := media.Extract(context.Background(), msg, defaultTestConfig())

	if len(kept) != 0 {
		t.Fatalf("aucune pièce jointe ne devait être retenue, obtenu %d", len(kept))
	}
	if len(rejected) != 1 {
		t.Fatalf("rejets: got %d, expected 1", len(rejected))
	}
	if !strings.Contains(rejected[0], "contrat.pdf") || !strings.Contains(rejected[0], "non pris en charge") {
		t.Errorf("le rejet devrait nommer le fichier et son motif, obtenu: %q", rejected[0])
	}
}

func TestExtract_TooLargeRejected(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.MaxSize = 8

	msg := messageWith(attachmentPart("grande.png", "image/png", bytes.Repeat([]byte("a"), 64)))

	kept, rejected := media.Extract(context.Background(), msg, cfg)

	if len(kept) != 0 {
		t.Fatalf("aucune pièce jointe ne devait être retenue, obtenu %d", len(kept))
	}
	if len(rejected) != 1 || !strings.Contains(rejected[0], "dépasse") {
		t.Fatalf("un rejet pour dépassement de taille était attendu, obtenu %v", rejected)
	}
}

// TestExtract_TooLargeWithUnknownSizeRejected couvre le cas d'une plateforme
// qui n'annonce pas la taille : seule la lecture bornée peut alors trancher.
func TestExtract_TooLargeWithUnknownSizeRejected(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.MaxSize = 8

	data := bytes.Repeat([]byte("a"), 64)
	open := func(ctx context.Context) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	// Size non renseignée : vaut -1 côté go-courier.
	part := courier.NewAttachment("inconnue.png", "image/png", open)

	kept, rejected := media.Extract(context.Background(), messageWith(part), cfg)

	if len(kept) != 0 {
		t.Fatalf("aucune pièce jointe ne devait être retenue, obtenu %d", len(kept))
	}
	if len(rejected) != 1 || !strings.Contains(rejected[0], "dépasse") {
		t.Fatalf("un rejet pour dépassement de taille était attendu, obtenu %v", rejected)
	}
}

func TestExtract_MaxCountRespected(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.MaxCount = 2

	msg := messageWith(
		attachmentPart("a.png", "image/png", []byte("a")),
		attachmentPart("b.png", "image/png", []byte("b")),
		attachmentPart("c.png", "image/png", []byte("c")),
	)

	kept, rejected := media.Extract(context.Background(), msg, cfg)

	if len(kept) != 2 {
		t.Fatalf("pièces jointes retenues: got %d, expected 2", len(kept))
	}
	if len(rejected) != 1 || !strings.Contains(rejected[0], "c.png") {
		t.Fatalf("la troisième pièce jointe devait être écartée et signalée, obtenu %v", rejected)
	}
}

// TestExtract_VoiceNoteIgnored vérifie que les notes vocales ne passent pas
// par ce chemin : internal/audio les transcrit sans jamais les conserver
// (PLAN.md §3.4).
func TestExtract_VoiceNoteIgnored(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.AcceptedTypes = append(cfg.AcceptedTypes, "audio/ogg")

	msg := messageWith(attachmentPart("note.ogg", "audio/ogg", []byte("audio"), courier.WithAttachmentVoiceNote(2*time.Second)))

	kept, rejected := media.Extract(context.Background(), msg, cfg)

	if len(kept) != 0 {
		t.Fatalf("une note vocale ne doit jamais être retenue comme pièce jointe, obtenu %d", len(kept))
	}
	if len(rejected) != 0 {
		t.Fatalf("une note vocale ne doit pas non plus être signalée comme rejetée, obtenu %v", rejected)
	}
}

// TestExtract_SkipAudioIsSilent vérifie qu'un fichier audio pris en charge
// par la transcription n'est ni transmis comme pièce jointe, ni signalé comme
// rejeté : ce n'est pas un rejet, il est simplement traité ailleurs.
func TestExtract_SkipAudioIsSilent(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.AcceptedTypes = append(cfg.AcceptedTypes, "audio/ogg")
	cfg.SkipAudio = true

	msg := messageWith(attachmentPart("enregistrement.ogg", "audio/ogg", []byte("audio")))

	kept, rejected := media.Extract(context.Background(), msg, cfg)

	if len(kept) != 0 {
		t.Fatalf("l'audio est pris en charge par la transcription, il ne doit pas être retenu ici (obtenu %d)", len(kept))
	}
	if len(rejected) != 0 {
		t.Fatalf("aucun rejet ne doit être signalé pour un audio transcrit, obtenu %v", rejected)
	}
}

// TestExtract_DisabledSignalsRejection vérifie que la désactivation n'est pas
// silencieuse : l'agent doit pouvoir expliquer pourquoi il n'a rien vu.
func TestExtract_DisabledSignalsRejection(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.Enabled = false

	msg := messageWith(attachmentPart("photo.png", "image/png", []byte("png")))

	kept, rejected := media.Extract(context.Background(), msg, cfg)

	if len(kept) != 0 {
		t.Fatalf("aucune pièce jointe ne devait être retenue, obtenu %d", len(kept))
	}
	if len(rejected) != 1 || !strings.Contains(rejected[0], "désactivées") {
		t.Fatalf("la désactivation devait être signalée, obtenu %v", rejected)
	}
}

func TestExtract_UnreadableAttachmentDoesNotFailTheMessage(t *testing.T) {
	open := func(ctx context.Context) (io.ReadCloser, error) {
		return nil, io.ErrUnexpectedEOF
	}
	broken := courier.NewAttachment("cassee.png", "image/png", open)

	kept, rejected := media.Extract(context.Background(), messageWith(broken), defaultTestConfig())

	if len(kept) != 0 {
		t.Fatalf("aucune pièce jointe ne devait être retenue, obtenu %d", len(kept))
	}
	if len(rejected) != 1 {
		t.Fatalf("la pièce jointe illisible devait être signalée, obtenu %v", rejected)
	}
}

func TestToLLM_Image(t *testing.T) {
	data := []byte("octets png")

	attachment, err := media.ToLLM(media.Media{
		Kind:     media.KindImage,
		MimeType: "image/png",
		Filename: "photo.png",
		Data:     data,
	})
	if err != nil {
		t.Fatalf("ToLLM: %v", err)
	}

	if attachment.Type() != llm.AttachmentTypeImage {
		t.Errorf("type = %q, attendu image", attachment.Type())
	}
	if attachment.Source() != llm.AttachmentSourceBase64 {
		t.Errorf("source = %q, attendu base64", attachment.Source())
	}

	decoded, err := base64.StdEncoding.DecodeString(attachment.Data())
	if err != nil {
		t.Fatalf("les données doivent être encodées en base64: %v", err)
	}
	if !bytes.Equal(decoded, data) {
		t.Errorf("données altérées après conversion: %q", decoded)
	}
}

// TestFromLLM_RoundTrip couvre le retour : un média produit par un outil MCP
// doit pouvoir redevenir un média applicatif, prêt à être renvoyé.
func TestFromLLM_RoundTrip(t *testing.T) {
	data := []byte("octets jpeg")

	attachment, err := llm.NewImageAttachment("image/jpeg", base64.StdEncoding.EncodeToString(data), false)
	if err != nil {
		t.Fatalf("NewImageAttachment: %v", err)
	}

	got, ok := media.FromLLM(attachment, "")
	if !ok {
		t.Fatal("la conversion depuis une pièce jointe llm aurait dû réussir")
	}
	if got.Kind != media.KindImage {
		t.Errorf("kind = %q, attendu image", got.Kind)
	}
	if !bytes.Equal(got.Data, data) {
		t.Errorf("données altérées: %q", got.Data)
	}
	if got.Filename == "" {
		t.Error("un nom de fichier doit être fabriqué : les messageries en exigent un")
	}
}

// TestFromLLM_URLSourceRefused documente le choix de ne pas télécharger une
// pièce jointe désignée par URL : ce serait une requête sortante vers un hôte
// choisi par un tiers.
func TestFromLLM_URLSourceRefused(t *testing.T) {
	attachment, err := llm.NewImageAttachment("image/png", "https://example.test/image.png", true)
	if err != nil {
		t.Fatalf("NewImageAttachment: %v", err)
	}

	if _, ok := media.FromLLM(attachment, ""); ok {
		t.Fatal("une pièce jointe désignée par URL ne doit pas être convertie")
	}
}

func TestToCourier(t *testing.T) {
	data := []byte("octets png")

	attachment := media.ToCourier(media.Media{
		Kind:     media.KindImage,
		MimeType: "image/png",
		Filename: "graphique.png",
		Caption:  "Évolution mensuelle",
		Data:     data,
	})

	if attachment.Filename() != "graphique.png" {
		t.Errorf("filename = %q", attachment.Filename())
	}
	if attachment.ContentType() != "image/png" {
		t.Errorf("content_type = %q", attachment.ContentType())
	}
	if attachment.Caption() != "Évolution mensuelle" {
		t.Errorf("caption = %q", attachment.Caption())
	}

	reader, err := attachment.Reader(context.Background())
	if err != nil {
		t.Fatalf("Reader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("lecture: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("données altérées: %q", got)
	}
}

func TestKindFromMIME(t *testing.T) {
	cases := map[string]struct {
		want Kind
		ok   bool
	}{
		"image/png":       {media.KindImage, true},
		"audio/ogg":       {media.KindAudio, true},
		"video/mp4":       {media.KindVideo, true},
		"text/plain":      {media.KindDocument, true},
		"application/pdf": {media.KindDocument, true},
		"application/zip": {"", false},
	}

	for mimeType, expected := range cases {
		got, ok := media.KindFromMIME(mimeType)
		if ok != expected.ok {
			t.Errorf("KindFromMIME(%q) ok = %v, attendu %v", mimeType, ok, expected.ok)
			continue
		}
		if got != expected.want {
			t.Errorf("KindFromMIME(%q) = %q, attendu %q", mimeType, got, expected.want)
		}
	}
}

// Kind est un alias local pour alléger la table de TestKindFromMIME.
type Kind = media.Kind
