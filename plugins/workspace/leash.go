package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Client LeaSH : deux canaux vers le même serveur, avec la même
// authentification Bearer et le même discriminant de workspace.
//
//   - MCP Streamable HTTP pour l'exécution de commandes (execute_shell)
//   - endpoints /files/ pour les octets, qui n'ont rien à faire dans un
//     résultat d'outil textuel et borné.
//
// Le discriminant est « <org_id>/<member_id> » : LeaSH le passe en
// HMAC-SHA256 avant d'en faire un nom de répertoire, il n'est jamais
// utilisé tel quel comme chemin.

const (
	envServerURL = "LEASH_SERVER_URL"
	envAPIKey    = "LEASH_API_KEY"
	// envFetchAPIKey désigne la clé de la policy « fetch » côté LeaSH : la
	// SEULE à ouvrir le réseau, et restreinte au script fetch-video. Elle
	// est distincte de LEASH_API_KEY à dessein — c'est la séparation des
	// clés qui garde l'atelier (ffmpeg, imagemagick, LibreOffice) étanche,
	// donc exécutable sans confirmation. Vide : pas de téléchargement.
	envFetchAPIKey = "LEASH_FETCH_API_KEY"

	// httpTimeout borne les requêtes fichiers. Généreux : une vidéo de
	// 16 Mio traverse un réseau interne, pas Internet.
	httpTimeout = 2 * time.Minute

	// commandTimeout borne un execute_shell côté plugin. Il doit rester
	// au-dessus de execution.max_duration de la policy LeaSH (300 s), sans
	// quoi le plugin abandonnerait avant que LeaSH ait rendu son verdict.
	commandTimeout = 320 * time.Second

	// fetchTimeout borne un téléchargement. Au-dessus de la
	// execution.max_duration de la policy « fetch » (600 s), même
	// raisonnement que commandTimeout.
	fetchTimeout = 620 * time.Second

	// maxErrorBodyBytes borne ce qu'on recopie du corps d'une réponse
	// d'erreur. Un code HTTP nu ne dit rien de la cause (leçon du bug
	// rooms.upload) : on remonte le corps, tronqué.
	maxErrorBodyBytes = 512
)

// LeashClient parle à un serveur LeaSH pour le compte d'un membre.
type LeashClient struct {
	baseURL string
	apiKey  string
	// fetchAPIKey ouvre la policy réseau ; vide quand l'exploitant n'a pas
	// activé le téléchargement.
	fetchAPIKey string
	http        *http.Client
}

// newLeashClientFromEnv construit le client depuis l'environnement hérité
// de l'hôte (manager.go passe os.Environ() au sous-processus). Aucune
// configuration par membre, aucune UI : c'est un réglage d'exploitation.
func newLeashClientFromEnv() *LeashClient {
	return &LeashClient{
		baseURL:     strings.TrimSuffix(os.Getenv(envServerURL), "/"),
		apiKey:      os.Getenv(envAPIKey),
		fetchAPIKey: os.Getenv(envFetchAPIKey),
		http:        &http.Client{Timeout: httpTimeout},
	}
}

// fetchConfigured indique si le téléchargement est ouvert par
// l'exploitant.
func (c *LeashClient) fetchConfigured() bool {
	return c.configured() && c.fetchAPIKey != ""
}

// configured indique si le client a de quoi joindre un serveur.
func (c *LeashClient) configured() bool {
	return c.baseURL != "" && c.apiKey != ""
}

// workspaceID compose le discriminant du membre.
func workspaceID(orgID, memberID string) string {
	return orgID + "/" + memberID
}

// setHeaders applique authentification et discriminant. apiKey vide
// signifie « la clé de l'atelier » ; la policy servie par LeaSH découle de
// la clé présentée, c'est ce qui sépare l'atelier étanche du sandbox
// réseau.
func (c *LeashClient) setHeaders(req *http.Request, orgID, memberID, apiKey string) {
	if apiKey == "" {
		apiKey = c.apiKey
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("X-Workspace", workspaceID(orgID, memberID))
}

// Execute lance un script dans le bac à sable du membre et retourne le
// texte de résultat formaté par LeaSH (STDOUT/STDERR/EXIT CODE/BLOCKED).
//
// Une session MCP est ouverte puis refermée à chaque appel. C'est un
// aller-retour de plus, et c'est délibéré : garder une session par membre
// obligerait à la rattacher à un contexte qui survive au tour, et une
// session orpheline retiendrait un workspace côté serveur. Le coût d'un
// initialize est négligeable devant celui d'un ffmpeg.
func (c *LeashClient) Execute(ctx context.Context, orgID, memberID, script string) (string, bool, error) {
	if !c.configured() {
		return "", false, fmt.Errorf("workspace: %s et %s doivent être renseignés", envServerURL, envAPIKey)
	}

	return c.executeWithKey(ctx, orgID, memberID, script, "", commandTimeout)
}

// Fetch télécharge une vidéo publique dans le workspace du membre, par la
// policy réseau de LeaSH. L'URL est déjà validée par l'appelant (schéma,
// liste blanche de domaines) ; le script fetch-video la revalide de son
// côté et encadre yt-dlp.
func (c *LeashClient) Fetch(ctx context.Context, orgID, memberID, url, name string) (string, bool, error) {
	if !c.fetchConfigured() {
		return "", false, fmt.Errorf("workspace: %s doit être renseigné pour télécharger", envFetchAPIKey)
	}

	// Un seul argument par valeur, entre apostrophes simples : la policy
	// n'autorise qu'une commande et aucun subshell, mais rien ne justifie
	// de laisser une URL non échappée atteindre l'analyseur de LeaSH.
	script := fmt.Sprintf("fetch-video '%s' '%s'", shellSingleQuoted(url), shellSingleQuoted(name))

	return c.executeWithKey(ctx, orgID, memberID, script, c.fetchAPIKey, fetchTimeout)
}

// shellSingleQuoted neutralise l'apostrophe simple dans une valeur entre
// apostrophes simples.
func shellSingleQuoted(value string) string {
	return strings.ReplaceAll(value, "'", `'\''`)
}

// executeWithKey ouvre une session MCP avec la clé donnée (vide = celle de
// l'atelier) et lance un script. La clé détermine la policy appliquée
// côté LeaSH, donc l'ouverture réseau.
func (c *LeashClient) executeWithKey(ctx context.Context, orgID, memberID, script, apiKey string, timeout time.Duration) (string, bool, error) {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	transport := &mcp.StreamableClientTransport{
		Endpoint: c.baseURL,
		HTTPClient: &http.Client{
			Timeout:   timeout,
			Transport: &headerTransport{client: c, orgID: orgID, memberID: memberID, apiKey: apiKey},
		},
		// Aucune notification serveur n'est attendue : ne pas ouvrir le
		// flux SSE permanent évite une connexion qui survivrait à l'appel.
		DisableStandaloneSSE: true,
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "automata-workspace", Version: "0.1.0"}, nil)

	session, err := client.Connect(callCtx, transport, nil)
	if err != nil {
		return "", false, fmt.Errorf("connexion au serveur d'exécution: %w", err)
	}
	defer func() { _ = session.Close() }()

	result, err := session.CallTool(callCtx, &mcp.CallToolParams{
		Name:      "execute_shell",
		Arguments: map[string]any{"script": script},
	})
	if err != nil {
		return "", false, fmt.Errorf("exécution de la commande: %w", err)
	}

	var sb strings.Builder
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			sb.WriteString(text.Text)
		}
	}

	return sb.String(), result.IsError, nil
}

// headerTransport injecte l'authentification et le discriminant sur chaque
// requête HTTP de la session MCP.
type headerTransport struct {
	client   *LeashClient
	orgID    string
	memberID string
	// apiKey vide : la clé de l'atelier.
	apiKey string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Le RoundTripper ne doit pas modifier la requête qu'on lui confie.
	clone := req.Clone(req.Context())
	t.client.setHeaders(clone, t.orgID, t.memberID, t.apiKey)
	return http.DefaultTransport.RoundTrip(clone)
}

// FileEntry décrit un fichier du workspace.
type FileEntry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

type fileListing struct {
	Files []FileEntry `json:"files"`
	Total int64       `json:"total_bytes"`
}

// ListFiles retourne les fichiers du workspace du membre.
func (c *LeashClient) ListFiles(ctx context.Context, orgID, memberID string) ([]FileEntry, error) {
	resp, err := c.do(ctx, http.MethodGet, orgID, memberID, "", nil, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if err := checkStatus(resp, http.StatusOK); err != nil {
		return nil, err
	}

	var listing fileListing
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		return nil, fmt.Errorf("réponse de listage illisible: %w", err)
	}

	return listing.Files, nil
}

// PutFile dépose un fichier dans le workspace du membre et retourne le
// chemin retenu par le serveur.
func (c *LeashClient) PutFile(ctx context.Context, orgID, memberID, path, mimeType string, data []byte) (string, error) {
	resp, err := c.do(ctx, http.MethodPut, orgID, memberID, path, bytes.NewReader(data), mimeType)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if err := checkStatus(resp, http.StatusCreated); err != nil {
		return "", err
	}

	var entry FileEntry
	if err := json.NewDecoder(resp.Body).Decode(&entry); err != nil {
		return "", fmt.Errorf("réponse de dépôt illisible: %w", err)
	}

	return entry.Path, nil
}

// GetFile récupère un fichier du workspace du membre. maxBytes borne la
// lecture : le plugin ne doit pas se laisser saturer par un fichier
// démesuré, même produit par l'utilisateur lui-même.
func (c *LeashClient) GetFile(ctx context.Context, orgID, memberID, path string, maxBytes int64) ([]byte, string, error) {
	resp, err := c.do(ctx, http.MethodGet, orgID, memberID, path, nil, "")
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if err := checkStatus(resp, http.StatusOK); err != nil {
		return nil, "", err
	}

	limit := maxBytes
	if limit <= 0 {
		limit = 32 << 20
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", fmt.Errorf("lecture du fichier: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, "", fmt.Errorf("le fichier dépasse %d octets", limit)
	}

	return data, resp.Header.Get("Content-Type"), nil
}

// do construit et exécute une requête vers les endpoints fichiers.
func (c *LeashClient) do(ctx context.Context, method, orgID, memberID, path string, body io.Reader, contentType string) (*http.Response, error) {
	if !c.configured() {
		return nil, fmt.Errorf("workspace: %s et %s doivent être renseignés", envServerURL, envAPIKey)
	}

	endpoint := c.baseURL + "/files/"
	if path != "" {
		// Chaque segment est échappé séparément : un nom de fichier peut
		// contenir des espaces ou des accents, mais les séparateurs de
		// chemin doivent rester des séparateurs.
		segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
		for i, segment := range segments {
			segments[i] = url.PathEscape(segment)
		}
		endpoint += strings.Join(segments, "/")
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("construction de la requête: %w", err)
	}
	// Les endpoints /files travaillent sur le workspace, pas dans un
	// sandbox : la clé de l'atelier suffit.
	c.setHeaders(req, orgID, memberID, "")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("requête vers le serveur d'exécution: %w", err)
	}

	return resp, nil
}

// checkStatus transforme une réponse inattendue en erreur PORTANT le corps
// renvoyé par LeaSH, tronqué. Un « 413 » nu n'apprend rien à personne ;
// « 413 : file too large » se corrige.
func checkStatus(resp *http.Response, want int) error {
	if resp.StatusCode == want {
		return nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	detail := strings.TrimSpace(string(body))
	if detail == "" {
		return fmt.Errorf("le serveur d'exécution a répondu %s", resp.Status)
	}

	return fmt.Errorf("le serveur d'exécution a répondu %s : %s", resp.Status, detail)
}
