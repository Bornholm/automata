package plugin

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
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

// maxToolResultBytes borne la taille d'un résultat relayé au modèle.
const maxToolResultBytes = 48 * 1024

// SubAgentSpec décrit le sous-agent d'un plugin actif, prêt à être monté.
type SubAgentSpec struct {
	PluginName       string
	SystemPrompt     string
	Description      string
	PermissionDomain string
	MaxToolCalls     int
	Tools            []ToolSpec
}

// ToolSpec décrit un outil du plugin.
type ToolSpec struct {
	Name        string
	Description string
	SchemaJSON  string
	ReadOnly    bool
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
		}
		for _, t := range tools.Tools {
			spec.Tools = append(spec.Tools, ToolSpec{
				Name:        t.Name,
				Description: t.Description,
				SchemaJSON:  t.InputSchemaJson,
				ReadOnly:    t.ReadOnly,
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
func (m *Manager) CallTool(ctx context.Context, pluginName, toolName string, callCtx CallContext, argsJSON string) (string, bool, error) {
	client, _, ok := m.GetOrRestart(ctx, pluginName)
	if !ok {
		return "", false, fmt.Errorf("plugin %q indisponible", pluginName)
	}

	callTimeout, cancel := context.WithTimeout(ctx, pluginToolTimeout)
	defer cancel()

	started := time.Now()
	out, err := client.CallTool(callTimeout, &proto.CallToolInput{
		Ctx:           toProtoContext(callCtx),
		Name:          toolName,
		ArgumentsJson: argsJSON,
	})
	duration := time.Since(started)

	// Identifiants et compteurs seulement : jamais les arguments ni le
	// résultat, qui peuvent porter des contenus privés.
	slog.InfoContext(ctx, "plugin: appel d'outil",
		"plugin", pluginName, "tool", toolName,
		"duration", duration.String(), "error", err != nil)

	if err != nil {
		return "", false, fmt.Errorf("appel de %s.%s: %w", pluginName, toolName, err)
	}

	text := out.ResultText
	if len(text) > maxToolResultBytes {
		text = truncateUTF8(text, maxToolResultBytes) + fmt.Sprintf("\n[résultat tronqué à %d octets]", maxToolResultBytes)
	}

	return text, out.IsError, nil
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
