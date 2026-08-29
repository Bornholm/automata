package plugin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"time"
	"unicode/utf8"

	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// Pont entre le gestionnaire de plugins et le registre d'agents. Les types
// restent locaux au paquet : c'est internal/registry qui les convertit
// vers ceux du paquet agent, aucun des deux n'importe l'autre.

// pluginToolTimeout borne un appel d'outil : un plugin qui ne répond pas
// ne doit pas suspendre le tour entier.
const pluginToolTimeout = 60 * time.Second

// maxPluginToolTimeout plafonne ce qu'un plugin peut demander comme
// timeout d'outil. Un plugin déclare ce dont il a besoin (une commande
// ffmpeg dure plus qu'un appel d'API), l'hôte garde le dernier mot : sans
// plafond, un plugin fautif suspendrait le tour indéfiniment.
const maxPluginToolTimeout = 10 * time.Minute

// maxToolResultBytes borne la taille d'un résultat relayé au modèle.
const maxToolResultBytes = 48 * 1024

// fileChunkBytes est la taille des tranches de fichier échangées avec un
// plugin. Bien sous la limite de message gRPC (4 Mio par défaut) : la
// tranche entière doit tenir dans un message, cadre compris.
const fileChunkBytes = 1 << 20

// SubAgentSpec décrit le sous-agent d'un plugin actif, prêt à être monté.
type SubAgentSpec struct {
	PluginName       string
	SystemPrompt     string
	Description      string
	PermissionDomain string
	MaxToolCalls     int
	// SupportsFiles autorise l'hôte à monter ses outils fichiers sur ce
	// sous-agent, et à appeler PutFile/GetFile sur ce plugin.
	SupportsFiles bool
	Tools         []ToolSpec
}

// ToolSpec décrit un outil du plugin.
type ToolSpec struct {
	Name        string
	Description string
	SchemaJSON  string
	ReadOnly    bool
	// TimeoutSeconds est le timeout demandé par le plugin pour cet outil.
	// 0 : défaut de l'hôte. Toujours plafonné par maxPluginToolTimeout.
	TimeoutSeconds int
}

// CallContext est l'identité d'un appel d'outil, construite par l'hôte.
type CallContext struct {
	OrgID          string
	MemberID       string
	Scope          string
	ScopeID        string
	IdempotencyKey string
}

// ActiveSubAgents retourne les sous-agents des plugins actifs pour
// l'organisation. L'activation est relue en base à CHAQUE appel — donc à
// chaque tour — pour qu'une désactivation s'applique immédiatement, et
// ListTools est interrogé avec l'identité du tour : le plugin peut
// tailler ses outils selon les réglages du membre (une boîte mail en
// lecture seule n'expose aucun outil d'envoi).
func (m *Manager) ActiveSubAgents(ctx context.Context, db dbTx, callCtx CallContext) []SubAgentSpec {
	var enabled []string
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		enabled, err = m.hostService.activations.EnabledPlugins(ctx, tx, callCtx.OrgID)
		return err
	})
	if err != nil {
		slog.ErrorContext(ctx, "plugin: lecture des activations échouée", "org_id", callCtx.OrgID, "error", err)
		return nil
	}

	var specs []SubAgentSpec
	for _, name := range enabled {
		client, desc, ok := m.GetOrRestart(ctx, name)
		if !ok || desc.SubAgent == nil {
			continue
		}

		listCtx, cancel := context.WithTimeout(ctx, pluginToolTimeout)
		tools, err := client.ListTools(listCtx, &proto.ListToolsInput{Ctx: toProtoContext(callCtx)})
		cancel()
		if err != nil {
			slog.WarnContext(ctx, "plugin: ListTools en échec, sous-agent ignoré ce tour",
				"plugin", name, "error", err)
			continue
		}

		spec := SubAgentSpec{
			PluginName:       name,
			SystemPrompt:     desc.SubAgent.SystemPrompt,
			Description:      desc.SubAgent.Description,
			PermissionDomain: desc.PermissionDomain,
			MaxToolCalls:     int(desc.SubAgent.MaxSequentialToolCalls),
			SupportsFiles:    desc.SupportsFiles,
		}
		for _, t := range tools.Tools {
			spec.Tools = append(spec.Tools, ToolSpec{
				Name:           t.Name,
				Description:    t.Description,
				SchemaJSON:     t.InputSchemaJson,
				ReadOnly:       t.ReadOnly,
				TimeoutSeconds: int(t.TimeoutSeconds),
			})
		}
		specs = append(specs, spec)
	}

	return specs
}

// dbTx est la part de persistence.DB dont le pont a besoin.
type dbTx interface {
	WithTx(ctx context.Context, fn func(tx *sql.Tx) error) error
}

// CallTool exécute un outil du plugin, avec timeout et troncature — même
// discipline que les outils MCP. isError signale un échec métier, relayé
// au modèle sans avorter le tour.
func (m *Manager) CallTool(ctx context.Context, pluginName, toolName string, callCtx CallContext, argsJSON string, timeoutSeconds int) (string, bool, error) {
	client, _, ok := m.GetOrRestart(ctx, pluginName)
	if !ok {
		return "", false, fmt.Errorf("plugin %q indisponible", pluginName)
	}

	callTimeout, cancel := context.WithTimeout(ctx, toolTimeout(timeoutSeconds))
	defer cancel()

	started := time.Now()
	out, err := client.CallTool(callTimeout, &proto.CallToolInput{
		Ctx:           toProtoContext(callCtx),
		Name:          toolName,
		ArgumentsJson: argsJSON,
	})
	duration := time.Since(started)

	// Identifiants et compteurs seulement : jamais les valeurs des
	// arguments ni le résultat, qui peuvent porter des contenus privés.
	//
	// is_error distingue l'échec MÉTIER de l'échec de transport : sans
	// lui, un outil qui refuse poliment est indiscernable d'un outil qui
	// réussit, et l'exploitant ne voit rien. Les NOMS des arguments
	// accompagnent un échec : ils viennent du schéma, pas de
	// l'utilisateur, et c'est ce qui révèle un modèle qui se trompe de
	// paramètre — cause déjà observée d'un outil « en panne ».
	logCtx := []any{"plugin", pluginName, "tool", toolName,
		"duration", duration.String(), "error", err != nil}
	if err == nil {
		logCtx = append(logCtx, "is_error", out.IsError)
		if out.IsError {
			logCtx = append(logCtx, "argument_keys", argumentKeys(argsJSON))
		}
	}
	slog.InfoContext(ctx, "plugin: appel d'outil", logCtx...)

	if err != nil {
		return "", false, fmt.Errorf("appel de %s.%s: %w", pluginName, toolName, err)
	}

	text := out.ResultText
	if len(text) > maxToolResultBytes {
		text = truncateUTF8(text, maxToolResultBytes) + fmt.Sprintf("\n[résultat tronqué à %d octets]", maxToolResultBytes)
	}

	return text, out.IsError, nil
}

// argumentKeys extrait les NOMS des arguments d'un appel, triés. Jamais
// les valeurs : les noms viennent du schéma de l'outil, les valeurs de
// l'utilisateur.
func argumentKeys(argsJSON string) []string {
	var args map[string]json.RawMessage
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return []string{"<illisible>"}
	}

	keys := make([]string, 0, len(args))
	for key := range args {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// toolTimeout applique le timeout demandé par le plugin, borné par
// l'hôte des deux côtés : jamais moins que le défaut n'est imposé, jamais
// plus que maxPluginToolTimeout n'est accordé.
func toolTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		return pluginToolTimeout
	}
	d := time.Duration(seconds) * time.Second
	if d > maxPluginToolTimeout {
		return maxPluginToolTimeout
	}
	return d
}

// PutFile pousse un fichier vers le plugin, par tranches. Retourne le
// chemin sous lequel le plugin l'a rangé.
//
// Les octets ne passent JAMAIS par un résultat d'outil : ce canal existe
// précisément pour qu'une vidéo n'ait pas à traverser la conversation.
// PutFile et GetFile : voir agent.PluginFileTransfer pour le contrat côté
// appelant.
func (m *Manager) PutFile(ctx context.Context, pluginName string, callCtx CallContext, filename, mimeType string, data []byte) (string, bool, string, error) {
	client, desc, ok := m.GetOrRestart(ctx, pluginName)
	if !ok {
		return "", false, "", fmt.Errorf("plugin %q indisponible", pluginName)
	}
	if desc == nil || !desc.SupportsFiles {
		return "", false, "", fmt.Errorf("le plugin %q ne prend pas en charge les fichiers", pluginName)
	}

	callTimeout, cancel := context.WithTimeout(ctx, maxPluginToolTimeout)
	defer cancel()

	stream, err := client.PutFile(callTimeout)
	if err != nil {
		return "", false, "", fmt.Errorf("ouverture du flux PutFile vers %q: %w", pluginName, err)
	}

	meta := &proto.PutFileChunk{Payload: &proto.PutFileChunk_Metadata{Metadata: &proto.PutFileMetadata{
		Ctx:      toProtoContext(callCtx),
		Filename: filename,
		MimeType: mimeType,
		Size:     uint64(len(data)),
	}}}
	if err := stream.Send(meta); err != nil {
		return "", false, "", fmt.Errorf("envoi des métadonnées à %q: %w", pluginName, err)
	}

	for offset := 0; offset < len(data); offset += fileChunkBytes {
		end := min(offset+fileChunkBytes, len(data))
		if err := stream.Send(&proto.PutFileChunk{Payload: &proto.PutFileChunk_Data{Data: data[offset:end]}}); err != nil {
			return "", false, "", fmt.Errorf("envoi d'une tranche à %q: %w", pluginName, err)
		}
	}

	result, err := stream.CloseAndRecv()
	if err != nil {
		return "", false, "", fmt.Errorf("fin du flux PutFile vers %q: %w", pluginName, err)
	}

	// Identifiants et compteurs seulement : jamais le nom du fichier, qui
	// vient de l'utilisateur.
	slog.InfoContext(ctx, "plugin: fichier transmis au plugin",
		"plugin", pluginName, "bytes", len(data), "is_error", result.IsError)

	return result.Path, result.IsError, result.ErrorText, nil
}

// GetFile récupère un fichier depuis le plugin, par tranches, en bornant
// la taille reçue : un plugin ne doit pas pouvoir saturer la mémoire de
// l'hôte en annonçant un fichier énorme.
func (m *Manager) GetFile(ctx context.Context, pluginName string, callCtx CallContext, path string, maxBytes int64) (filename, mimeType string, data []byte, err error) {
	client, desc, ok := m.GetOrRestart(ctx, pluginName)
	if !ok {
		return "", "", nil, fmt.Errorf("plugin %q indisponible", pluginName)
	}
	if desc == nil || !desc.SupportsFiles {
		return "", "", nil, fmt.Errorf("le plugin %q ne prend pas en charge les fichiers", pluginName)
	}

	callTimeout, cancel := context.WithTimeout(ctx, maxPluginToolTimeout)
	defer cancel()

	stream, err := client.GetFile(callTimeout, &proto.GetFileRequest{
		Ctx:  toProtoContext(callCtx),
		Path: path,
	})
	if err != nil {
		return "", "", nil, fmt.Errorf("ouverture du flux GetFile depuis %q: %w", pluginName, err)
	}

	var buf []byte
	for {
		chunk, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return "", "", nil, fmt.Errorf("lecture du flux GetFile depuis %q: %w", pluginName, recvErr)
		}

		switch payload := chunk.Payload.(type) {
		case *proto.FileChunk_Metadata:
			filename = payload.Metadata.Filename
			mimeType = payload.Metadata.MimeType
			if maxBytes > 0 && payload.Metadata.Size > uint64(maxBytes) {
				return "", "", nil, fmt.Errorf("le fichier dépasse %d octets", maxBytes)
			}
		case *proto.FileChunk_Data:
			buf = append(buf, payload.Data...)
			if maxBytes > 0 && int64(len(buf)) > maxBytes {
				return "", "", nil, fmt.Errorf("le fichier dépasse %d octets", maxBytes)
			}
		}
	}

	slog.InfoContext(ctx, "plugin: fichier récupéré depuis le plugin",
		"plugin", pluginName, "bytes", len(buf))

	return filename, mimeType, buf, nil
}

func toProtoContext(cc CallContext) *proto.CallContext {
	return &proto.CallContext{
		OrgId:          cc.OrgID,
		MemberId:       cc.MemberID,
		Scope:          cc.Scope,
		ScopeId:        cc.ScopeID,
		IdempotencyKey: cc.IdempotencyKey,
	}
}

// truncateUTF8 coupe sans casser un point de code.
func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// ActiveDomains retourne les domaines de permission des plugins actifs
// pour l'organisation. Lu depuis les descripteurs déjà en mémoire : aucun
// appel gRPC, la fonction est sur le chemin de chaque autorisation.
func (m *Manager) ActiveDomains(ctx context.Context, db dbTx, orgID string) []string {
	var enabled []string
	if err := db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		enabled, err = m.hostService.activations.EnabledPlugins(ctx, tx, orgID)
		return err
	}); err != nil {
		slog.ErrorContext(ctx, "plugin: lecture des activations échouée", "org_id", orgID, "error", err)
		return nil
	}
	if len(enabled) == 0 {
		return nil
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	var domains []string
	for _, name := range enabled {
		if _, entry := m.findEntry(name); entry != nil && entry.Descriptor.PermissionDomain != "" {
			domains = append(domains, entry.Descriptor.PermissionDomain)
		}
	}
	return domains
}
