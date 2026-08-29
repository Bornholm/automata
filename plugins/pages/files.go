package main

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"path"
	"regexp"
	"strings"

	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// Pont de fichiers avec l'hôte : import_attachment pousse les pièces
// jointes de la conversation dans la collection « imports » (PutFile), et
// attach_file récupère un fichier d'un brouillon ou l'archive zip d'un
// espace (GetFile).

// maxImportBytes borne un fichier importé ; l'hôte applique déjà ses
// quotas, cette borne est la ceinture du plugin.
const maxImportBytes = 16 << 20

// fileChunkBytes est la taille des tranches renvoyées à l'hôte, bien sous
// la limite de message gRPC.
const fileChunkBytes = 1 << 20

// PutFile implémente proto.AutomataPluginServer : l'hôte pousse une pièce
// jointe de la conversation, qui atterrit dans « imports » en attendant
// use_file.
func (p *Plugin) PutFile(stream proto.AutomataPlugin_PutFileServer) error {
	host := p.hostClient()
	if host == nil {
		return errors.New("pages: plugin non initialisé")
	}

	first, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("pages: réception des métadonnées: %w", err)
	}
	meta, ok := first.Payload.(*proto.PutFileChunk_Metadata)
	if !ok || meta.Metadata == nil {
		return errors.New("pages: le premier fragment doit porter les métadonnées")
	}
	callCtx := meta.Metadata.Ctx
	if callCtx == nil || callCtx.OrgId == "" || callCtx.MemberId == "" {
		return errors.New("pages: contexte d'appel incomplet")
	}

	var data []byte
	for {
		chunk, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return fmt.Errorf("pages: réception d'une tranche: %w", recvErr)
		}
		payload, isData := chunk.Payload.(*proto.PutFileChunk_Data)
		if !isData {
			return errors.New("pages: fragment inattendu après les métadonnées")
		}
		data = append(data, payload.Data...)
		if len(data) > maxImportBytes {
			return stream.SendAndClose(&proto.PutFileResult{
				IsError:   true,
				ErrorText: fmt.Sprintf("the file exceeds %d bytes", maxImportBytes),
			})
		}
	}

	key := importKey(meta.Metadata.Filename, meta.Metadata.MimeType)
	if err := host.PutObject(stream.Context(), callCtx.OrgId, callCtx.MemberId,
		importsCollection, key, meta.Metadata.MimeType, data); err != nil {
		return stream.SendAndClose(&proto.PutFileResult{
			IsError:   true,
			ErrorText: "the file could not be stored: " + err.Error(),
		})
	}

	return stream.SendAndClose(&proto.PutFileResult{Path: importsCollection + "/" + key})
}

// GetFile implémente proto.AutomataPluginServer : « <espace>.zip » rend
// l'archive du brouillon, « <espace>/<chemin> » un fichier du brouillon.
func (p *Plugin) GetFile(req *proto.GetFileRequest, stream proto.AutomataPlugin_GetFileServer) error {
	host := p.hostClient()
	if host == nil {
		return errors.New("pages: plugin non initialisé")
	}
	if req.Ctx == nil || req.Ctx.OrgId == "" || req.Ctx.MemberId == "" {
		return errors.New("pages: contexte d'appel incomplet")
	}

	if space, ok := strings.CutSuffix(req.Path, ".zip"); ok && spaceNamePattern.MatchString(space) {
		data, err := p.zipSpace(stream.Context(), callScope{host: host, orgID: req.Ctx.OrgId, memberID: req.Ctx.MemberId}, space)
		if err != nil {
			return err
		}
		return sendFile(stream, space+".zip", "application/zip", data)
	}

	space, filePath, ok := strings.Cut(req.Path, "/")
	if !ok || !spaceNamePattern.MatchString(space) || !validFilePath(filePath) {
		return fmt.Errorf("pages: chemin invalide %q", req.Path)
	}

	data, contentType, found, err := host.GetObject(stream.Context(), req.Ctx.OrgId, req.Ctx.MemberId, draftCollection(space), filePath)
	if err != nil {
		return fmt.Errorf("pages: lecture du fichier: %w", err)
	}
	if !found {
		return fmt.Errorf("pages: fichier introuvable %q", req.Path)
	}
	if contentType == "" {
		contentType = mimeFromExtension(filePath)
	}
	return sendFile(stream, path.Base(filePath), contentType, data)
}

// zipSpace archive le brouillon d'un espace en mémoire.
func (p *Plugin) zipSpace(ctx context.Context, s callScope, space string) ([]byte, error) {
	entries, err := s.host.ListObjects(ctx, s.orgID, s.memberID, draftCollection(space))
	if err != nil {
		return nil, fmt.Errorf("pages: listage de l'espace: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("pages: aucun espace %q", space)
	}

	var buf bytes.Buffer
	archive := zip.NewWriter(&buf)
	for _, entry := range entries {
		data, _, found, err := s.host.GetObject(ctx, s.orgID, s.memberID, draftCollection(space), entry.Key)
		if err != nil || !found {
			return nil, fmt.Errorf("pages: lecture de %q: %w", entry.Key, err)
		}
		writer, err := archive.Create(space + "/" + entry.Key)
		if err != nil {
			return nil, fmt.Errorf("pages: écriture de l'archive: %w", err)
		}
		if _, err := writer.Write(data); err != nil {
			return nil, fmt.Errorf("pages: écriture de l'archive: %w", err)
		}
	}
	if err := archive.Close(); err != nil {
		return nil, fmt.Errorf("pages: clôture de l'archive: %w", err)
	}

	return buf.Bytes(), nil
}

// sendFile pousse un fichier vers l'hôte en tranches.
func sendFile(stream proto.AutomataPlugin_GetFileServer, filename, mimeType string, data []byte) error {
	if err := stream.Send(&proto.FileChunk{Payload: &proto.FileChunk_Metadata{Metadata: &proto.FileMetadata{
		Filename: filename,
		MimeType: mimeType,
		Size:     uint64(len(data)),
	}}}); err != nil {
		return fmt.Errorf("pages: envoi des métadonnées: %w", err)
	}

	for offset := 0; offset < len(data); offset += fileChunkBytes {
		end := min(offset+fileChunkBytes, len(data))
		if err := stream.Send(&proto.FileChunk{Payload: &proto.FileChunk_Data{Data: data[offset:end]}}); err != nil {
			return fmt.Errorf("pages: envoi d'une tranche: %w", err)
		}
	}
	return nil
}

// importKeyUnsafe retire tout ce qui n'a pas sa place dans une clé du
// magasin (l'hôte n'accepte que [a-z0-9._-] par segment).
var importKeyUnsafe = regexp.MustCompile(`[^a-z0-9._-]+`)

// importKey ramène un nom de fichier utilisateur à une clé sûre de la
// collection imports, en préservant l'extension autant que possible.
func importKey(name, mimeType string) string {
	name = path.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
	name = strings.ToLower(name)
	name = importKeyUnsafe.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-.")
	if name == "" || name == "." || name == ".." {
		name = "media" + extensionFor(mimeType)
	}
	return name
}

// usualExtensions fixe l'extension des types courants ; la première entrée
// de mime.ExtensionsByType est souvent marginale (« .jfif » pour
// image/jpeg).
var usualExtensions = map[string]string{
	"video/mp4":  ".mp4",
	"video/webm": ".webm",
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/gif":  ".gif",
	"image/webp": ".webp",
	"audio/mpeg": ".mp3",
	"audio/ogg":  ".ogg",
}

func extensionFor(mimeType string) string {
	if extension, found := usualExtensions[strings.ToLower(strings.TrimSpace(mimeType))]; found {
		return extension
	}
	if extensions, err := mime.ExtensionsByType(mimeType); err == nil && len(extensions) > 0 {
		return extensions[0]
	}
	return ""
}

// mimeFromExtension devine le type d'un fichier du magasin quand aucun
// n'a été enregistré.
func mimeFromExtension(p string) string {
	if guessed := mime.TypeByExtension(path.Ext(p)); guessed != "" {
		if parsed, _, err := mime.ParseMediaType(guessed); err == nil {
			return parsed
		}
	}
	return "application/octet-stream"
}
