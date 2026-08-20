package pluginsdk

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// HostClient is the typed façade plugins use to call AutomataHostService.
// There is deliberately no plugin name parameter anywhere: the host binds
// the identity of the calling plugin to the broker connection itself, so a
// plugin cannot address another plugin's data.
type HostClient interface {
	// GetConfig returns the stored configuration for (orgID, memberID).
	// memberID == "" addresses the organization-level entry. found is
	// false when nothing was stored yet.
	GetConfig(ctx context.Context, orgID, memberID string) (configJSON string, found bool, err error)
	// SaveConfig stores the configuration for (orgID, memberID).
	SaveConfig(ctx context.Context, orgID, memberID, configJSON string) error
	// ListConfigs returns every stored configuration of organizations
	// where this plugin is currently active. Watchers rely on it to know
	// which accounts to poll.
	ListConfigs(ctx context.Context) ([]ConfigEntry, error)
	// GetSecret returns the secret for (orgID, memberID, key). The value
	// comes back in clear: plugins need it to authenticate against
	// external services. It must never be echoed to a UI or a log.
	GetSecret(ctx context.Context, orgID, memberID, key string) (value string, found bool, err error)
	// SetSecret stores a secret. The host seals it at rest.
	SetSecret(ctx context.Context, orgID, memberID, key, value string) error
	// DeleteSecret removes a secret.
	DeleteSecret(ctx context.Context, orgID, memberID, key string) error
	// Notify sends an application-authored message to the member's
	// private channel. No LLM is involved.
	Notify(ctx context.Context, orgID, memberID, text string) error
}

// ConfigEntry is one stored configuration, as returned by ListConfigs.
type ConfigEntry struct {
	OrgID      string
	MemberID   string
	ConfigJSON string
}

type grpcHostClient struct {
	client proto.AutomataHostServiceClient
}

func newGRPCHostClient(conn *grpc.ClientConn) *grpcHostClient {
	return &grpcHostClient{client: proto.NewAutomataHostServiceClient(conn)}
}

func (c *grpcHostClient) GetConfig(ctx context.Context, orgID, memberID string) (string, bool, error) {
	resp, err := c.client.GetConfig(ctx, &proto.GetConfigRequest{OrgId: orgID, MemberId: memberID})
	if err != nil {
		return "", false, fmt.Errorf("GetConfig: %w", err)
	}
	return resp.ConfigJson, resp.Found, nil
}

func (c *grpcHostClient) SaveConfig(ctx context.Context, orgID, memberID, configJSON string) error {
	if _, err := c.client.SaveConfig(ctx, &proto.SaveConfigRequest{OrgId: orgID, MemberId: memberID, ConfigJson: configJSON}); err != nil {
		return fmt.Errorf("SaveConfig: %w", err)
	}
	return nil
}

func (c *grpcHostClient) ListConfigs(ctx context.Context) ([]ConfigEntry, error) {
	resp, err := c.client.ListConfigs(ctx, &proto.ListConfigsRequest{})
	if err != nil {
		return nil, fmt.Errorf("ListConfigs: %w", err)
	}
	entries := make([]ConfigEntry, 0, len(resp.Entries))
	for _, e := range resp.Entries {
		entries = append(entries, ConfigEntry{OrgID: e.OrgId, MemberID: e.MemberId, ConfigJSON: e.ConfigJson})
	}
	return entries, nil
}

func (c *grpcHostClient) GetSecret(ctx context.Context, orgID, memberID, key string) (string, bool, error) {
	resp, err := c.client.GetSecret(ctx, &proto.GetSecretRequest{OrgId: orgID, MemberId: memberID, Key: key})
	if err != nil {
		return "", false, fmt.Errorf("GetSecret: %w", err)
	}
	return resp.Value, resp.Found, nil
}

func (c *grpcHostClient) SetSecret(ctx context.Context, orgID, memberID, key, value string) error {
	if _, err := c.client.SetSecret(ctx, &proto.SetSecretRequest{OrgId: orgID, MemberId: memberID, Key: key, Value: value}); err != nil {
		return fmt.Errorf("SetSecret: %w", err)
	}
	return nil
}

func (c *grpcHostClient) DeleteSecret(ctx context.Context, orgID, memberID, key string) error {
	if _, err := c.client.DeleteSecret(ctx, &proto.DeleteSecretRequest{OrgId: orgID, MemberId: memberID, Key: key}); err != nil {
		return fmt.Errorf("DeleteSecret: %w", err)
	}
	return nil
}

func (c *grpcHostClient) Notify(ctx context.Context, orgID, memberID, text string) error {
	if _, err := c.client.Notify(ctx, &proto.NotifyRequest{OrgId: orgID, MemberId: memberID, Text: text}); err != nil {
		return fmt.Errorf("Notify: %w", err)
	}
	return nil
}

var _ HostClient = &grpcHostClient{}
