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

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bornholm/automata/pkg/pluginsdk"
	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// Pont entre le gestionnaire de plugins et le registre d'agents. Les types
// restent locaux au paquet : c'est internal/registry qui les convertit
// vers ceux du paquet agent, aucun des deux n'importe l'autre.

// pluginToolTimeout borne un appel d'outil : un plugin qui ne répond pas
// ne doit pas suspendre le tour entier.
const pluginToolTimeout = 60 * time.Second

// maxLoggedErrorBytes borne le message d'échec journalisé.
const maxLoggedErrorBytes = 300

// maxPluginToolTimeout plafonne ce qu'un plugin peut demander comme
// timeout d'outil. Un plugin déclare ce dont il a besoin (une commande
// ffmpeg dure plus qu'un appel d'API), l'hôte garde le dernier mot : sans
// plafond, un plugin fautif suspendrait le tour indéfiniment.
const maxPluginToolTimeout = 10 * time.Minute

// maxToolResultBytes borne la taille d'un résultat relayé au modèle.
const maxToolResultBytes = 48 * 1024

// SubAgentSpec décrit le sous-agent d'un plugin actif, prêt à être monté.
type SubAgentSpec struct {
	// PluginName route : c'est lui qui désigne le sous-processus à appeler
	// et la clé de l'exécuteur d'actions confirmées.
	PluginName string
	// AgentName est le nom vu par le modèle (delegate_to_<nom>). Il vaut
	// PluginName pour un plugin à sous-agent unique, et le nom de l'entrée
	// pour un plugin qui en fournit un catalogue.
	AgentName string
	// SubAgentName est l'entrée du catalogue à laquelle appartiennent les
	// outils, transmise à CallTool. Vide pour un sous-agent unique.
	SubAgentName     string
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
	// SubAgent désigne l'entrée du catalogue à laquelle appartient l'outil
	// appelé. Vide pour un plugin à sous-agent unique. Il voyage à part du
	// contexte transmis au plugin (toProtoContext) : c'est un routage, pas
	// une identité.
	SubAgent string
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
	// mounted retient les noms déjà pris CE tour : deux delegate_to_<x>
	// homonymes seraient indiscernables pour le modèle.
	mounted := make(map[string]struct{})

	for _, name := range enabled {
		client, desc, ok := m.GetOrRestart(ctx, name)
		if !ok {
			continue
		}

		var candidates []SubAgentSpec
		switch {
		case desc.ProvidesSubAgents:
			candidates = m.catalogSubAgents(ctx, client, desc, name, callCtx)
		case desc.SubAgent != nil:
			candidates = m.singleSubAgent(ctx, client, desc, name, callCtx)
		}

		for _, spec := range candidates {
			if reason, taken := m.nameTaken(spec.AgentName, mounted); taken {
				slog.WarnContext(ctx, "plugin: nom de sous-agent déjà pris, entrée ignorée",
					"plugin", name, "sub_agent", spec.AgentName, "reason", reason)
				continue
			}
			mounted[spec.AgentName] = struct{}{}
			specs = append(specs, spec)
		}
	}

	return specs
}

// nameTaken dit si agentName ne peut pas être monté, et pourquoi. Les
// collisions avec les agents configurés sont refusées au chargement pour
// le NOM du plugin (voir Manager.agentNames) ; les entrées d'un catalogue,
// elles, ne sont connues qu'ici, à l'exécution.
func (m *Manager) nameTaken(agentName string, mounted map[string]struct{}) (string, bool) {
	if agentName == "" {
		return "nom vide", true
	}
	if _, ok := m.agentNames[agentName]; ok {
		return "agent configuré", true
	}
	if _, ok := mounted[agentName]; ok {
		return "déjà monté ce tour", true
	}
	return "", false
}

// singleSubAgent est le chemin historique : un plugin, un sous-agent, dont
// les outils viennent de ListTools taillé pour le membre.
func (m *Manager) singleSubAgent(ctx context.Context, client proto.AutomataPluginClient, desc *proto.PluginDescriptor, name string, callCtx CallContext) []SubAgentSpec {
	listCtx, cancel := context.WithTimeout(ctx, pluginToolTimeout)
	tools, err := client.ListTools(listCtx, &proto.ListToolsInput{Ctx: toProtoContext(callCtx)})
	cancel()
	if err != nil {
		slog.WarnContext(ctx, "plugin: ListTools en échec, sous-agent ignoré ce tour",
			"plugin", name, "error", err)
		return nil
	}

	return []SubAgentSpec{{
		PluginName:       name,
		AgentName:        name,
		SystemPrompt:     desc.SubAgent.SystemPrompt,
		Description:      desc.SubAgent.Description,
		PermissionDomain: desc.PermissionDomain,
		MaxToolCalls:     int(desc.SubAgent.MaxSequentialToolCalls),
		SupportsFiles:    desc.SupportsFiles,
		Tools:            toToolSpecs(tools.Tools),
	}}
}

// catalogSubAgents interroge ListSubAgents : un plugin qui fournit un
// catalogue rend les entrées que CE membre a activées, chacune avec son
// prompt, sa description et ses outils.
//
// Un plugin qui pose le drapeau sans implémenter la RPC (binaire compilé
// contre un SDK antérieur) retombe sur son sous-agent unique : mieux vaut
// un plugin dégradé qu'un plugin muet.
func (m *Manager) catalogSubAgents(ctx context.Context, client proto.AutomataPluginClient, desc *proto.PluginDescriptor, name string, callCtx CallContext) []SubAgentSpec {
	listCtx, cancel := context.WithTimeout(ctx, pluginToolTimeout)
	out, err := client.ListSubAgents(listCtx, &proto.ListSubAgentsInput{Ctx: toProtoContext(callCtx)})
	cancel()
	if err != nil {
		if status.Code(err) == codes.Unimplemented && desc.SubAgent != nil {
			slog.WarnContext(ctx, "plugin: ListSubAgents absent malgré provides_sub_agents, repli sur le sous-agent unique",
				"plugin", name)
			return m.singleSubAgent(ctx, client, desc, name, callCtx)
		}
		slog.WarnContext(ctx, "plugin: ListSubAgents en échec, sous-agents ignorés ce tour",
			"plugin", name, "error", err)
		return nil
	}

	specs := make([]SubAgentSpec, 0, len(out.SubAgents))
	for _, entry := range out.SubAgents {
		if entry.SubAgent == nil {
			slog.WarnContext(ctx, "plugin: entrée de catalogue sans sous-agent, ignorée",
				"plugin", name, "sub_agent", entry.Name)
			continue
		}

		specs = append(specs, SubAgentSpec{
			PluginName:       name,
			AgentName:        entry.Name,
			SubAgentName:     entry.Name,
			SystemPrompt:     entry.SubAgent.SystemPrompt,
			Description:      entry.SubAgent.Description,
			PermissionDomain: desc.PermissionDomain,
			MaxToolCalls:     int(entry.SubAgent.MaxSequentialToolCalls),
			SupportsFiles:    desc.SupportsFiles,
			Tools:            toToolSpecs(entry.Tools),
		})
	}

	return specs
}

// toToolSpecs convertit les descripteurs d'outils du protocole vers le
// type local du pont.
func toToolSpecs(tools []*proto.ToolDescriptor) []ToolSpec {
	specs := make([]ToolSpec, 0, len(tools))
	for _, t := range tools {
		specs = append(specs, ToolSpec{
			Name:           t.Name,
			Description:    t.Description,
			SchemaJSON:     t.InputSchemaJson,
			ReadOnly:       t.ReadOnly,
			TimeoutSeconds: int(t.TimeoutSeconds),
		})
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
		SubAgent:      callCtx.SubAgent,
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
			// En cas d'échec, le motif : un message d'erreur est rédigé
			// POUR être lu, et sans lui un plugin en panne n'est pas
			// diagnosticable en production — les journaux du
			// sous-processus, eux, ne remontent pas jusqu'ici. Tronqué, et
			// réservé à l'échec : un résultat NOMINAL peut porter le
			// contenu d'un fichier ou d'un courriel, jamais journalisé.
			logCtx = append(logCtx,
				"argument_keys", argumentKeys(argsJSON),
				"error_text", truncateUTF8(out.ResultText, maxLoggedErrorBytes))
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

// FileMeta décrit un fichier échangé avec un plugin : ce que l'appelant
// doit connaître AVANT de disposer des octets — pour poser des en-têtes
// HTTP, refuser une taille, ou nommer une pièce jointe.
type FileMeta struct {
	Filename string
	MimeType string
	// Size est la taille annoncée par la source. Zéro quand elle n'est pas
	// connue à l'avance : c'est une indication, jamais une garantie.
	Size int64
}

// PutFile pousse un fichier vers le plugin, en le lisant au fil de l'eau.
// Retourne le chemin sous lequel le plugin l'a rangé.
//
// Les octets ne passent JAMAIS par un résultat d'outil : ce canal existe
// précisément pour qu'une vidéo n'ait pas à traverser la conversation.
// PutFile et OpenFile : voir agent.PluginFileTransfer pour le contrat côté
// appelant.
func (m *Manager) PutFile(ctx context.Context, pluginName string, callCtx CallContext, meta FileMeta, r io.Reader) (string, bool, string, error) {
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

	header := &proto.PutFileChunk{Payload: &proto.PutFileChunk_Metadata{Metadata: &proto.PutFileMetadata{
		Ctx:      toProtoContext(callCtx),
		Filename: meta.Filename,
		MimeType: meta.MimeType,
		Size:     uint64(max(meta.Size, 0)),
	}}}
	if err := stream.Send(header); err != nil {
		return "", false, "", fmt.Errorf("envoi des métadonnées à %q: %w", pluginName, err)
	}

	sent := 0
	err = pluginsdk.SendFile(func(data []byte) error {
		sent += len(data)
		return stream.Send(&proto.PutFileChunk{Payload: &proto.PutFileChunk_Data{Data: data}})
	}, r)
	if err != nil {
		return "", false, "", fmt.Errorf("envoi d'une tranche à %q: %w", pluginName, err)
	}

	result, err := stream.CloseAndRecv()
	if err != nil {
		return "", false, "", fmt.Errorf("fin du flux PutFile vers %q: %w", pluginName, err)
	}

	// Identifiants et compteurs seulement : jamais le nom du fichier, qui
	// vient de l'utilisateur.
	slog.InfoContext(ctx, "plugin: fichier transmis au plugin",
		"plugin", pluginName, "bytes", sent, "is_error", result.IsError)

	return result.Path, result.IsError, result.ErrorText, nil
}

// OpenFile ouvre un fichier du plugin et rend ses métadonnées puis un
// lecteur qui tire les tranches au fil de l'eau.
//
// Rien n'est accumulé ici : c'est ce qui permet de servir un fichier de
// plusieurs centaines de mégaoctets à un navigateur sans le charger en
// mémoire. Un appelant qui a besoin des octets en entier les lit lui-même
// avec SA borne (io.LimitReader) — voir attach_file et view_file, qui
// refusent au-delà de attachments.max_tool_size.
//
// Le lecteur retourné DOIT être fermé : il tient le flux gRPC ouvert.
func (m *Manager) OpenFile(ctx context.Context, pluginName string, callCtx CallContext, path string) (FileMeta, io.ReadCloser, error) {
	client, desc, ok := m.GetOrRestart(ctx, pluginName)
	if !ok {
		return FileMeta{}, nil, fmt.Errorf("plugin %q indisponible", pluginName)
	}
	if desc == nil || !desc.SupportsFiles {
		return FileMeta{}, nil, fmt.Errorf("le plugin %q ne prend pas en charge les fichiers", pluginName)
	}

	// Le contexte n'est PAS borné par maxPluginToolTimeout : il gouverne
	// désormais toute la durée de la lecture, qui dure aussi longtemps que
	// le téléchargement du client. C'est à l'appelant de le borner.
	stream, err := client.GetFile(ctx, &proto.GetFileRequest{
		Ctx:  toProtoContext(callCtx),
		Path: path,
	})
	if err != nil {
		return FileMeta{}, nil, fmt.Errorf("ouverture du flux GetFile depuis %q: %w", pluginName, err)
	}

	// La première trame porte les métadonnées : on l'attend avant de rendre
	// la main, pour que l'appelant sache ce qu'il s'apprête à lire.
	var meta FileMeta
	first, err := stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return FileMeta{}, nil, fmt.Errorf("le plugin %q n'a rien renvoyé pour ce fichier", pluginName)
		}
		return FileMeta{}, nil, fmt.Errorf("lecture du flux GetFile depuis %q: %w", pluginName, err)
	}

	// Une trame de données peut précéder les métadonnées si le plugin les
	// omet : on la garde plutôt que de la perdre.
	var pending []byte
	switch payload := first.Payload.(type) {
	case *proto.FileChunk_Metadata:
		meta = FileMeta{
			Filename: payload.Metadata.Filename,
			MimeType: payload.Metadata.MimeType,
			Size:     int64(payload.Metadata.Size),
		}
	case *proto.FileChunk_Data:
		pending = payload.Data
	}

	body := pluginsdk.RecvFile(func() ([]byte, error) {
		if pending != nil {
			data := pending
			pending = nil
			return data, nil
		}

		chunk, recvErr := stream.Recv()
		if recvErr != nil {
			return nil, recvErr
		}
		if data, ok := chunk.Payload.(*proto.FileChunk_Data); ok {
			return data.Data, nil
		}

		return nil, nil
	})

	slog.InfoContext(ctx, "plugin: fichier ouvert depuis le plugin",
		"plugin", pluginName, "announced_bytes", meta.Size)

	return meta, body, nil
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
