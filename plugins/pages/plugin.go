package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/bornholm/automata/pkg/pluginsdk"
	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// Plugin est l'atelier de pages web : des espaces (un dossier logique avec
// index.html et ses ressources) rangés dans le magasin d'objets de l'hôte,
// modifiés librement en brouillon et publiés — derrière la confirmation
// humaine — sous un lien public court servi par l'hôte.
type Plugin struct {
	proto.UnimplementedAutomataPluginServer

	mu   sync.Mutex
	host pluginsdk.HostClient
	// writtenKeys retient les clés d'idempotence déjà honorées : une
	// confirmation rejouée ne doit pas republier ou re-supprimer.
	writtenKeys map[string]struct{}
	writtenList []string
}

const writtenKeysCap = 512

func newPlugin() *Plugin {
	return &Plugin{writtenKeys: map[string]struct{}{}}
}

// SetHostClient implémente pluginsdk.HostClientSetter.
func (p *Plugin) SetHostClient(client pluginsdk.HostClient) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.host = client
}

func (p *Plugin) hostClient() pluginsdk.HostClient {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.host
}

// Conventions du magasin : le brouillon et la version publiée d'un espace
// sont deux collections, les fichiers importés depuis la conversation une
// troisième. Les écritures vont TOUJOURS au brouillon ; publish_space
// copie brouillon → live, donc modifier après publication ne change rien
// en ligne avant la prochaine publication confirmée.
const importsCollection = "imports"

func draftCollection(space string) string { return "spaces/" + space + "/draft" }
func liveCollection(space string) string  { return "spaces/" + space + "/live" }

// spaceOfLive retrouve le nom d'espace d'une collection live.
func spaceOfLive(collection string) (string, bool) {
	name, ok := strings.CutPrefix(collection, "spaces/")
	if !ok {
		return "", false
	}
	name, ok = strings.CutSuffix(name, "/live")
	return name, ok
}

// spaceNamePattern encadre les noms d'espaces choisis par le modèle.
var spaceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)

// filePathPattern encadre les chemins de fichiers d'un espace : minuscules
// et séparateurs sûrs, cohérent avec les clés du magasin hôte et les URLs
// publiques.
var filePathPattern = regexp.MustCompile(`^[a-z0-9._/-]{1,180}$`)

func validFilePath(p string) bool {
	if !filePathPattern.MatchString(p) || strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
		return false
	}
	for _, segment := range strings.Split(p, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

const (
	// maxSpaces borne le nombre d'espaces d'un membre ; les quotas de
	// volume vivent côté hôte.
	maxSpaces = 10
	// maxReadableBytes borne read_file : un résultat d'outil est plafonné
	// à 48 Kio par l'hôte, au-delà le contenu serait tronqué en silence.
	maxReadableBytes = 40 << 10
)

// Describe implémente proto.AutomataPluginServer.
func (p *Plugin) Describe(context.Context, *proto.DescribeRequest) (*proto.PluginDescriptor, error) {
	return &proto.PluginDescriptor{
		Name:             "pages",
		Version:          "0.1.0",
		Description:      "Pages web créées par l'assistant : espaces brouillon par membre, publication confirmée sous un lien public court, images et vidéos de la conversation réutilisables.",
		PermissionDomain: "pages",
		SupportsFiles:    true,
		SubAgent: &proto.SubAgentDescriptor{
			Description: "builds and edits small web pages (an invitation, a photo page, an event page, a mini-site) in the user's draft spaces, " +
				"can reuse pictures and videos the user sent in the conversation, and publishes a page under a short public link " +
				"(publishing always requires the user's confirmation)",
			SystemPrompt: pagesSystemPrompt,
			// Construire une page demande plusieurs écritures (HTML par
			// morceaux, CSS, imports de médias) plus les load_skill.
			MaxSequentialToolCalls: 24,
		},
	}, nil
}

// pagesSystemPrompt part vers le modèle : anglais uniquement.
const pagesSystemPrompt = "You are a web page builder. You create and edit small static web sites (\"spaces\") for the user: " +
	"an invitation, a photo page, an event page, a mini-site. Each space is a folder with an index.html and its assets, " +
	"stored server-side; there is no network access and no build step — plain HTML, CSS and vanilla JS only.\n\n" +
	"BEFORE writing any HTML or CSS, load the skills design-web-page and responsive-mobile-first from your catalog and follow them: " +
	"they define the visual quality bar and the mobile rules. Do not improvise a design without them.\n\n" +
	"Workflow:\n" +
	"1. Call list_spaces to see what already exists. Reuse and edit an existing space when the user refers to it; never recreate it.\n" +
	"2. For a new page, call create_space with a short lowercase name (e.g. 'invitation-anniversaire').\n" +
	"3. Write files with write_file: index.html plus a single style.css. For long HTML, write it in several calls using append: true — " +
	"never truncate a file to fit one call.\n" +
	"4. To use a picture or video the user sent in the conversation, call import_attachment with its exact filename, " +
	"then use_file to place it in the space, then reference it with a relative path in the HTML. " +
	"To look at a picture, call view_file with the path import_attachment returned (e.g. \"imports/photo.jpg\") " +
	"or with \"<space>/<file>\" once placed. " +
	"NEVER inline media as base64 and never hot-link external images.\n" +
	"5. Edits always go to the user's private draft. Nothing is visible online until publish_space, " +
	"and publish_space always requires the user's explicit confirmation — never claim a page is published or updated online before that. " +
	"After a page is published, changing it online requires publishing again (also confirmed).\n" +
	"6. After publish_space is confirmed, give the user the public URL exactly as returned.\n\n" +
	"Keep pages self-contained (relative links only), accessible and mobile-first. " +
	"If the user asks for something impossible here (forms with a backend, logins, payments), say so plainly and offer the static alternative."

// ListTools implémente proto.AutomataPluginServer.
//
// DÉCISION DE SÉCURITÉ — les outils qui écrivent dans le BROUILLON
// (create_space, write_file, delete_file, use_file) sont déclarés
// read_only à DESSEIN : ils ne touchent que l'espace de travail privé du
// membre, invisible du public, et exiger « confirmer » à chaque fichier
// rendrait l'atelier inutilisable (précédent : le plugin workspace, où la
// frontière est le sandbox). La frontière ici est l'EXPOSITION PUBLIQUE :
// publish_space, unpublish_space et delete_space restent des écritures et
// passent par la confirmation humaine de l'hôte. Corollaire : aucune de
// ces routes « brouillon » ne doit JAMAIS toucher une collection live.
func (p *Plugin) ListTools(context.Context, *proto.ListToolsInput) (*proto.ListToolsOutput, error) {
	return &proto.ListToolsOutput{
		Tools: []*proto.ToolDescriptor{
			{
				Name: "list_spaces",
				Description: "List the user's web page spaces: name, whether a version is published, the public URL if any, " +
					"and the files of each draft.",
				InputSchemaJson: `{"type":"object","properties":{}}`,
				ReadOnly:        true,
			},
			{
				Name:            "create_space",
				Description:     "Create a new draft space with a minimal index.html. The name is lowercase letters, digits and dashes.",
				InputSchemaJson: `{"type":"object","properties":{"name":{"type":"string","description":"Space name, e.g. 'invitation-anniversaire'."}},"required":["name"]}`,
				ReadOnly:        true,
			},
			{
				Name:            "list_files",
				Description:     "List the files of a space's draft, with sizes in bytes.",
				InputSchemaJson: `{"type":"object","properties":{"space":{"type":"string"}},"required":["space"]}`,
				ReadOnly:        true,
			},
			{
				Name:            "read_file",
				Description:     "Read a text file (html, css, js…) from a space's draft. Fails on binary or very large files.",
				InputSchemaJson: `{"type":"object","properties":{"space":{"type":"string"},"path":{"type":"string"}},"required":["space","path"]}`,
				ReadOnly:        true,
			},
			{
				Name: "write_file",
				Description: "Write a text file into a space's draft. With append: true the content is appended to the existing file — " +
					"use it to write long HTML in several calls. Nothing becomes public before publish_space.",
				InputSchemaJson: `{"type":"object","properties":{"space":{"type":"string"},"path":{"type":"string","description":"Relative path, lowercase, e.g. 'index.html' or 'css/style.css'."},"content":{"type":"string"},"append":{"type":"boolean","description":"Append to the existing file instead of replacing it."}},"required":["space","path","content"]}`,
				ReadOnly:        true,
			},
			{
				Name:            "delete_file",
				Description:     "Delete a file from a space's draft.",
				InputSchemaJson: `{"type":"object","properties":{"space":{"type":"string"},"path":{"type":"string"}},"required":["space","path"]}`,
				ReadOnly:        true,
			},
			{
				Name: "use_file",
				Description: "Copy a file previously imported with import_attachment into a space's draft, so the page can reference it " +
					"with a relative path. target_path defaults to the imported filename.",
				InputSchemaJson: `{"type":"object","properties":{"space":{"type":"string"},"import_path":{"type":"string","description":"The path returned by import_attachment, e.g. 'imports/photo.jpg'."},"target_path":{"type":"string"}},"required":["space","import_path"]}`,
				ReadOnly:        true,
			},
			{
				Name: "publish_space",
				Description: "Publish the current draft of a space under its public link (a copy is taken: later draft edits stay private " +
					"until the next publish). Returns the public URL. Requires the user's confirmation.",
				InputSchemaJson: `{"type":"object","properties":{"space":{"type":"string"}},"required":["space"]}`,
			},
			{
				Name:            "unpublish_space",
				Description:     "Take a space offline: the public link dies immediately. The draft is kept. Requires the user's confirmation.",
				InputSchemaJson: `{"type":"object","properties":{"space":{"type":"string"}},"required":["space"]}`,
			},
			{
				Name:            "delete_space",
				Description:     "Delete a space entirely: public link, published version and draft. Requires the user's confirmation.",
				InputSchemaJson: `{"type":"object","properties":{"space":{"type":"string"}},"required":["space"]}`,
			},
		},
	}, nil
}

// CallTool implémente proto.AutomataPluginServer. Un échec métier ressort
// en IsError, jamais en erreur Go : le modèle doit pouvoir l'expliquer.
func (p *Plugin) CallTool(ctx context.Context, in *proto.CallToolInput) (*proto.CallToolOutput, error) {
	host := p.hostClient()
	if host == nil || in.Ctx == nil || in.Ctx.OrgId == "" || in.Ctx.MemberId == "" {
		return toolError("plugin not initialized"), nil
	}

	var args struct {
		Name       string `json:"name"`
		Space      string `json:"space"`
		Path       string `json:"path"`
		Content    string `json:"content"`
		Append     bool   `json:"append"`
		ImportPath string `json:"import_path"`
		TargetPath string `json:"target_path"`
	}
	if in.ArgumentsJson != "" {
		if err := json.Unmarshal([]byte(in.ArgumentsJson), &args); err != nil {
			return toolError("invalid parameters"), nil
		}
	}

	scope := callScope{host: host, orgID: in.Ctx.OrgId, memberID: in.Ctx.MemberId}

	switch in.Name {
	case "list_spaces":
		return p.listSpaces(ctx, scope)
	case "create_space":
		return p.createSpace(ctx, scope, args.Name)
	case "list_files":
		return p.listFiles(ctx, scope, args.Space)
	case "read_file":
		return p.readFile(ctx, scope, args.Space, args.Path)
	case "write_file":
		return p.writeFile(ctx, scope, args.Space, args.Path, args.Content, args.Append)
	case "delete_file":
		return p.deleteFile(ctx, scope, args.Space, args.Path)
	case "use_file":
		return p.useFile(ctx, scope, args.Space, args.ImportPath, args.TargetPath)
	case "publish_space":
		return p.publishSpace(ctx, scope, args.Space, in.Ctx.IdempotencyKey)
	case "unpublish_space":
		return p.unpublishSpace(ctx, scope, args.Space, in.Ctx.IdempotencyKey)
	case "delete_space":
		return p.deleteSpace(ctx, scope, args.Space, in.Ctx.IdempotencyKey)
	default:
		return toolError(fmt.Sprintf("unknown tool %q", in.Name)), nil
	}
}

// callScope regroupe l'identité du tour pour les appels au magasin.
type callScope struct {
	host     pluginsdk.HostClient
	orgID    string
	memberID string
}

// spaceNames retourne les espaces existants (brouillon ou publié), triés.
func spaceNames(ctx context.Context, s callScope) ([]string, error) {
	collections, err := s.host.ListCollections(ctx, s.orgID, s.memberID, "spaces/")
	if err != nil {
		return nil, err
	}

	seen := map[string]struct{}{}
	for _, collection := range collections {
		rest, ok := strings.CutPrefix(collection, "spaces/")
		if !ok {
			continue
		}
		name, _, ok := strings.Cut(rest, "/")
		if !ok {
			continue
		}
		seen[name] = struct{}{}
	}

	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// checkSpace valide le nom et l'existence d'un espace.
func checkSpace(ctx context.Context, s callScope, space string) (string, *proto.CallToolOutput) {
	space = strings.TrimSpace(space)
	if !spaceNamePattern.MatchString(space) {
		return "", toolError("invalid space name: lowercase letters, digits and dashes only")
	}

	entries, err := s.host.ListObjects(ctx, s.orgID, s.memberID, draftCollection(space))
	if err != nil {
		return "", toolError("the space could not be checked: " + err.Error())
	}
	if len(entries) == 0 {
		return "", toolError(fmt.Sprintf("no space named %q: call list_spaces, or create_space first", space))
	}
	return space, nil
}

func (p *Plugin) listSpaces(ctx context.Context, s callScope) (*proto.CallToolOutput, error) {
	names, err := spaceNames(ctx, s)
	if err != nil {
		return toolError("the spaces could not be listed: " + err.Error()), nil
	}
	if len(names) == 0 {
		return &proto.CallToolOutput{ResultText: "No space yet. Use create_space to start one."}, nil
	}

	published := map[string]string{}
	publications, err := s.host.ListPublications(ctx, s.orgID, s.memberID)
	if err != nil {
		return toolError("the publications could not be listed: " + err.Error()), nil
	}
	for _, publication := range publications {
		if space, ok := spaceOfLive(publication.Collection); ok {
			published[space] = publication.URL
		}
	}

	var b strings.Builder
	for _, name := range names {
		if url, ok := published[name]; ok {
			fmt.Fprintf(&b, "- %s (published: %s)\n", name, url)
		} else {
			fmt.Fprintf(&b, "- %s (draft only, not published)\n", name)
		}
		entries, err := s.host.ListObjects(ctx, s.orgID, s.memberID, draftCollection(name))
		if err != nil {
			return toolError("the space files could not be listed: " + err.Error()), nil
		}
		for _, entry := range entries {
			fmt.Fprintf(&b, "    %s\t%d bytes\n", entry.Key, entry.Size)
		}
	}
	return &proto.CallToolOutput{ResultText: b.String()}, nil
}

// starterHTML est le contenu initial d'un espace : le strict minimum
// valide et mobile, que l'agent remplace aussitôt.
const starterHTML = "<!doctype html>\n<html lang=\"fr\">\n<head>\n<meta charset=\"utf-8\">\n" +
	"<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n" +
	"<title>Nouvelle page</title>\n</head>\n<body>\n<p>Page en construction.</p>\n</body>\n</html>\n"

func (p *Plugin) createSpace(ctx context.Context, s callScope, name string) (*proto.CallToolOutput, error) {
	name = strings.TrimSpace(name)
	if !spaceNamePattern.MatchString(name) {
		return toolError("invalid space name: lowercase letters, digits and dashes only, 48 characters max"), nil
	}
	// « imports » est le préfixe des pièces importées dans les chemins de
	// GetFile : un espace de ce nom rendrait ces chemins ambigus.
	if name == importsCollection {
		return toolError(`the name "imports" is reserved: pick another space name`), nil
	}

	names, err := spaceNames(ctx, s)
	if err != nil {
		return toolError("the spaces could not be listed: " + err.Error()), nil
	}
	for _, existing := range names {
		if existing == name {
			return toolError(fmt.Sprintf("the space %q already exists: edit it, or pick another name", name)), nil
		}
	}
	if len(names) >= maxSpaces {
		return toolError(fmt.Sprintf("space limit reached (%d): delete an unused space first", maxSpaces)), nil
	}

	if err := s.host.PutObject(ctx, s.orgID, s.memberID, draftCollection(name), "index.html", "text/html", []byte(starterHTML)); err != nil {
		return toolError("the space could not be created: " + err.Error()), nil
	}
	return &proto.CallToolOutput{ResultText: fmt.Sprintf("Space %q created with a starter index.html. It is a private draft: nothing is online.", name)}, nil
}

func (p *Plugin) listFiles(ctx context.Context, s callScope, space string) (*proto.CallToolOutput, error) {
	space, fail := checkSpace(ctx, s, space)
	if fail != nil {
		return fail, nil
	}

	entries, err := s.host.ListObjects(ctx, s.orgID, s.memberID, draftCollection(space))
	if err != nil {
		return toolError("the files could not be listed: " + err.Error()), nil
	}

	var b strings.Builder
	for _, entry := range entries {
		fmt.Fprintf(&b, "%s\t%d bytes\n", entry.Key, entry.Size)
	}
	return &proto.CallToolOutput{ResultText: b.String()}, nil
}

// readableExtensions borne read_file aux fichiers texte : renvoyer les
// octets d'un jpeg au modèle ne sert à rien et brûle le contexte.
var readableExtensions = map[string]struct{}{
	".html": {}, ".htm": {}, ".css": {}, ".js": {}, ".mjs": {},
	".json": {}, ".txt": {}, ".md": {}, ".svg": {}, ".xml": {}, ".vtt": {},
}

func (p *Plugin) readFile(ctx context.Context, s callScope, space, filePath string) (*proto.CallToolOutput, error) {
	space, fail := checkSpace(ctx, s, space)
	if fail != nil {
		return fail, nil
	}
	if !validFilePath(filePath) {
		return toolError("invalid file path"), nil
	}
	if _, ok := readableExtensions[extensionOf(filePath)]; !ok {
		return toolError("read_file only reads text files (html, css, js…): binary files cannot be shown"), nil
	}

	data, _, found, err := s.host.GetObject(ctx, s.orgID, s.memberID, draftCollection(space), filePath)
	if err != nil {
		return toolError("the file could not be read: " + err.Error()), nil
	}
	if !found {
		return toolError(fmt.Sprintf("no file %q in space %q", filePath, space)), nil
	}
	if len(data) > maxReadableBytes {
		return toolError(fmt.Sprintf("the file is too large to read back (%d bytes): rewrite it with write_file instead", len(data))), nil
	}
	return &proto.CallToolOutput{ResultText: string(data)}, nil
}

// contentTypes du magasin pour les fichiers écrits en texte. Déclaratif :
// la route publique de l'hôte re-dérive le type servi de l'extension.
var writeContentTypes = map[string]string{
	".html": "text/html", ".htm": "text/html", ".css": "text/css",
	".js": "text/javascript", ".mjs": "text/javascript", ".json": "application/json",
	".txt": "text/plain", ".md": "text/plain", ".svg": "image/svg+xml",
	".xml": "text/xml", ".vtt": "text/vtt",
}

func extensionOf(p string) string {
	if i := strings.LastIndex(p, "."); i >= 0 {
		return strings.ToLower(p[i:])
	}
	return ""
}

// htmlDocumentClosed reconnaît un fichier HTML dont le document est déjà
// fermé : y ajouter quoi que ce soit produirait du contenu après </html>.
func htmlDocumentClosed(filePath string, data []byte) bool {
	ext := extensionOf(filePath)
	if ext != ".html" && ext != ".htm" {
		return false
	}
	return strings.Contains(strings.ToLower(string(data)), "</html>")
}

// contentAfterClosingHTML détecte du contenu non blanc après la balise
// </html> — la signature exacte de la corruption par append.
func contentAfterClosingHTML(data []byte) bool {
	lower := strings.ToLower(string(data))
	i := strings.LastIndex(lower, "</html>")
	if i < 0 {
		return false
	}
	return strings.TrimSpace(string(data[i+len("</html>"):])) != ""
}

func (p *Plugin) writeFile(ctx context.Context, s callScope, space, filePath, content string, appendTo bool) (*proto.CallToolOutput, error) {
	space, fail := checkSpace(ctx, s, space)
	if fail != nil {
		return fail, nil
	}
	if !validFilePath(filePath) {
		return toolError("invalid file path: lowercase letters, digits, dots, dashes and slashes only"), nil
	}
	contentType, ok := writeContentTypes[extensionOf(filePath)]
	if !ok {
		return toolError("write_file only writes text files (html, css, js…); bring media in with import_attachment then use_file"), nil
	}
	if content == "" {
		return toolError("the 'content' argument is required"), nil
	}

	data := []byte(content)
	if appendTo {
		existing, _, found, err := s.host.GetObject(ctx, s.orgID, s.memberID, draftCollection(space), filePath)
		if err != nil {
			return toolError("the file could not be read before appending: " + err.Error()), nil
		}
		// Ajouter après </html> produit un document mal formé que le
		// navigateur « répare » en remontant le fragment dans le body —
		// observé en production : une figure orpheline collée après la
		// fermeture cassait toute la mise en page mobile.
		if found && htmlDocumentClosed(filePath, existing) {
			return toolError("this HTML document is already complete (it ends with </html>): appending would place content AFTER the closing tags and break the page. Rewrite the whole file without append, putting the new content inside <body>."), nil
		}
		if found {
			data = append(existing, data...)
		}
	}

	if err := s.host.PutObject(ctx, s.orgID, s.memberID, draftCollection(space), filePath, contentType, data); err != nil {
		return toolError("the file could not be written: " + err.Error()), nil
	}
	return &proto.CallToolOutput{ResultText: fmt.Sprintf("%s written (%d bytes, draft only).", filePath, len(data))}, nil
}

func (p *Plugin) deleteFile(ctx context.Context, s callScope, space, filePath string) (*proto.CallToolOutput, error) {
	space, fail := checkSpace(ctx, s, space)
	if fail != nil {
		return fail, nil
	}
	if !validFilePath(filePath) {
		return toolError("invalid file path"), nil
	}

	deleted, err := s.host.DeleteObject(ctx, s.orgID, s.memberID, draftCollection(space), filePath)
	if err != nil {
		return toolError("the file could not be deleted: " + err.Error()), nil
	}
	if !deleted {
		return toolError(fmt.Sprintf("no file %q in space %q", filePath, space)), nil
	}
	return &proto.CallToolOutput{ResultText: fmt.Sprintf("%s deleted from the draft.", filePath)}, nil
}

func (p *Plugin) useFile(ctx context.Context, s callScope, space, importPath, targetPath string) (*proto.CallToolOutput, error) {
	space, fail := checkSpace(ctx, s, space)
	if fail != nil {
		return fail, nil
	}

	key := strings.TrimPrefix(strings.TrimSpace(importPath), importsCollection+"/")
	if !validFilePath(key) || strings.Contains(key, "/") {
		return toolError("invalid import path: use the path returned by import_attachment"), nil
	}

	if targetPath == "" {
		targetPath = key
	}
	if !validFilePath(targetPath) {
		return toolError("invalid target path"), nil
	}

	data, contentType, found, err := s.host.GetObject(ctx, s.orgID, s.memberID, importsCollection, key)
	if err != nil {
		return toolError("the imported file could not be read: " + err.Error()), nil
	}
	if !found {
		return toolError(fmt.Sprintf("no imported file %q: call import_attachment first", key)), nil
	}

	if err := s.host.PutObject(ctx, s.orgID, s.memberID, draftCollection(space), targetPath, contentType, data); err != nil {
		return toolError("the file could not be placed in the space: " + err.Error()), nil
	}
	return &proto.CallToolOutput{ResultText: fmt.Sprintf("%s placed in space %q (%d bytes): reference it as \"%s\" in the HTML.", targetPath, space, len(data), targetPath)}, nil
}

func (p *Plugin) publishSpace(ctx context.Context, s callScope, space, idempotencyKey string) (*proto.CallToolOutput, error) {
	space, fail := checkSpace(ctx, s, space)
	if fail != nil {
		return fail, nil
	}

	index, _, found, err := s.host.GetObject(ctx, s.orgID, s.memberID, draftCollection(space), "index.html")
	if err != nil {
		return toolError("the draft could not be checked: " + err.Error()), nil
	}
	if !found {
		return toolError("the space has no index.html: write one before publishing"), nil
	}
	// Filet de sécurité : ne jamais publier un document corrompu par du
	// contenu après </html> — le rendu paraît fonctionner mais la
	// récupération d'erreur du navigateur détruit la mise en page.
	if trailing := contentAfterClosingHTML(index); trailing {
		return toolError("index.html is malformed: there is content AFTER the closing </html> tag. Rewrite index.html so everything sits inside <body>, then publish again."), nil
	}

	if idempotencyKey != "" && !p.firstSubmission(idempotencyKey) {
		return &proto.CallToolOutput{ResultText: "This space was already published (replayed confirmation)."}, nil
	}

	if _, err := s.host.CopyCollection(ctx, s.orgID, s.memberID, draftCollection(space), liveCollection(space)); err != nil {
		return toolError("the draft could not be copied for publication: " + err.Error()), nil
	}
	_, url, err := s.host.PublishCollection(ctx, s.orgID, s.memberID, liveCollection(space))
	if err != nil {
		return toolError("the space could not be published: " + err.Error()), nil
	}

	return &proto.CallToolOutput{ResultText: fmt.Sprintf("Space %q is now published at %s — share this link. Later draft edits stay private until the next publish_space.", space, url)}, nil
}

func (p *Plugin) unpublishSpace(ctx context.Context, s callScope, space, idempotencyKey string) (*proto.CallToolOutput, error) {
	space = strings.TrimSpace(space)
	if !spaceNamePattern.MatchString(space) {
		return toolError("invalid space name"), nil
	}

	if idempotencyKey != "" && !p.firstSubmission(idempotencyKey) {
		return &proto.CallToolOutput{ResultText: "This space was already taken offline (replayed confirmation)."}, nil
	}

	existed, err := s.host.UnpublishCollection(ctx, s.orgID, s.memberID, liveCollection(space))
	if err != nil {
		return toolError("the space could not be taken offline: " + err.Error()), nil
	}
	if _, err := s.host.DeleteCollection(ctx, s.orgID, s.memberID, liveCollection(space)); err != nil {
		return toolError("the published copy could not be removed: " + err.Error()), nil
	}
	if !existed {
		return &proto.CallToolOutput{ResultText: fmt.Sprintf("Space %q was not published.", space)}, nil
	}
	return &proto.CallToolOutput{ResultText: fmt.Sprintf("Space %q is offline: the public link is dead. The draft is kept.", space)}, nil
}

func (p *Plugin) deleteSpace(ctx context.Context, s callScope, space, idempotencyKey string) (*proto.CallToolOutput, error) {
	space = strings.TrimSpace(space)
	if !spaceNamePattern.MatchString(space) {
		return toolError("invalid space name"), nil
	}

	if idempotencyKey != "" && !p.firstSubmission(idempotencyKey) {
		return &proto.CallToolOutput{ResultText: "This space was already deleted (replayed confirmation)."}, nil
	}

	// DeleteCollection emporte la publication côté hôte, dans la même
	// transaction : le lien meurt avec le contenu.
	if _, err := s.host.DeleteCollection(ctx, s.orgID, s.memberID, liveCollection(space)); err != nil {
		return toolError("the published copy could not be removed: " + err.Error()), nil
	}
	if _, err := s.host.DeleteCollection(ctx, s.orgID, s.memberID, draftCollection(space)); err != nil {
		return toolError("the draft could not be removed: " + err.Error()), nil
	}
	return &proto.CallToolOutput{ResultText: fmt.Sprintf("Space %q deleted: public link, published version and draft are gone.", space)}, nil
}

// firstSubmission enregistre la clé d'idempotence ; faux si déjà vue.
func (p *Plugin) firstSubmission(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, seen := p.writtenKeys[key]; seen {
		return false
	}
	if len(p.writtenList) >= writtenKeysCap {
		oldest := p.writtenList[0]
		p.writtenList = p.writtenList[1:]
		delete(p.writtenKeys, oldest)
	}
	p.writtenKeys[key] = struct{}{}
	p.writtenList = append(p.writtenList, key)
	return true
}

func toolError(text string) *proto.CallToolOutput {
	return &proto.CallToolOutput{ResultText: text, IsError: true}
}
