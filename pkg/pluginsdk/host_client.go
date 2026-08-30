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

	// Object store: binary objects grouped in named collections, scoped
	// like configs and secrets. NOT sealed at rest by default — it exists
	// to hold content meant to be served publicly, never secrets. Use
	// PutObjectSealed for a member's own documents. Collection and key
	// names are restricted to lowercase [a-z0-9._/-] path segments.

	// PutObject stores one object; writing an existing (collection, key)
	// replaces it. Quotas are enforced host-side.
	PutObject(ctx context.Context, orgID, memberID, collection, key, contentType string, data []byte) error
	// PutObjectSealed stores one object encrypted at rest, for a member's
	// own documents. A collection holding sealed objects can never be
	// published. Fails outright when the host has no usable key — it never
	// falls back to storing in the clear.
	PutObjectSealed(ctx context.Context, orgID, memberID, collection, key, contentType string, data []byte) error
	// GetObject returns one object; found is false when it is absent.
	GetObject(ctx context.Context, orgID, memberID, collection, key string) (data []byte, contentType string, found bool, err error)
	// DeleteObject removes one object.
	DeleteObject(ctx context.Context, orgID, memberID, collection, key string) (bool, error)
	// DeleteCollection removes every object of a collection, along with
	// its publication if any.
	DeleteCollection(ctx context.Context, orgID, memberID, collection string) (int64, error)
	// ListObjects returns the metadata of a collection's objects.
	ListObjects(ctx context.Context, orgID, memberID, collection string) ([]ObjectEntry, error)
	// ListCollections returns the non-empty collections matching a
	// literal prefix; empty prefix lists them all.
	ListCollections(ctx context.Context, orgID, memberID, prefix string) ([]string, error)
	// CopyCollection replaces the target collection with a copy of the
	// source, atomically.
	CopyCollection(ctx context.Context, orgID, memberID, from, to string) (int64, error)
	// PublishCollection exposes a collection under the host's public
	// /s/<slug> route and returns the stable slug and absolute URL.
	PublishCollection(ctx context.Context, orgID, memberID, collection string) (slug, url string, err error)
	// UnpublishCollection kills the public link. The content stays.
	UnpublishCollection(ctx context.Context, orgID, memberID, collection string) (bool, error)
	// PreviewCollection mints a signed, short-lived URL serving the
	// collection — for the member's own eyes, nothing becomes public.
	// expiresAt is RFC3339 UTC.
	PreviewCollection(ctx context.Context, orgID, memberID, collection string) (url, expiresAt string, err error)
	// ShareFile mints a signed, expiring URL serving ONE file of this
	// plugin, for a member who cannot receive it as an attachment — a
	// video is far larger than any messaging cap. The host streams the
	// bytes when the link is opened; check the file exists first, the
	// host cannot.
	ShareFile(ctx context.Context, orgID, memberID, path string) (url, expiresAt string, err error)
	// ListPublications returns the member's published collections.
	ListPublications(ctx context.Context, orgID, memberID string) ([]Publication, error)
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
