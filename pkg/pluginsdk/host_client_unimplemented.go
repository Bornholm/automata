package pluginsdk

import (
	"context"
	"errors"
)

// ErrNotImplemented est répondu par UnimplementedHostClient.
var ErrNotImplemented = errors.New("pluginsdk: not implemented")

// UnimplementedHostClient répond ErrNotImplemented à tout. Les doubles de
// test des plugins l'embarquent pour ne redéfinir que les méthodes qu'ils
// exercent — et survivre aux extensions futures de HostClient.
type UnimplementedHostClient struct{}

func (UnimplementedHostClient) GetConfig(context.Context, string, string) (string, bool, error) {
	return "", false, ErrNotImplemented
}

func (UnimplementedHostClient) SaveConfig(context.Context, string, string, string) error {
	return ErrNotImplemented
}

func (UnimplementedHostClient) ListConfigs(context.Context) ([]ConfigEntry, error) {
	return nil, ErrNotImplemented
}

func (UnimplementedHostClient) GetSecret(context.Context, string, string, string) (string, bool, error) {
	return "", false, ErrNotImplemented
}

func (UnimplementedHostClient) SetSecret(context.Context, string, string, string, string) error {
	return ErrNotImplemented
}

func (UnimplementedHostClient) DeleteSecret(context.Context, string, string, string) error {
	return ErrNotImplemented
}

func (UnimplementedHostClient) Notify(context.Context, string, string, string) error {
	return ErrNotImplemented
}

func (UnimplementedHostClient) PutObject(context.Context, string, string, string, string, string, []byte) error {
	return ErrNotImplemented
}

func (UnimplementedHostClient) GetObject(context.Context, string, string, string, string) ([]byte, string, bool, error) {
	return nil, "", false, ErrNotImplemented
}

func (UnimplementedHostClient) DeleteObject(context.Context, string, string, string, string) (bool, error) {
	return false, ErrNotImplemented
}

func (UnimplementedHostClient) DeleteCollection(context.Context, string, string, string) (int64, error) {
	return 0, ErrNotImplemented
}

func (UnimplementedHostClient) ListObjects(context.Context, string, string, string) ([]ObjectEntry, error) {
	return nil, ErrNotImplemented
}

func (UnimplementedHostClient) ListCollections(context.Context, string, string, string) ([]string, error) {
	return nil, ErrNotImplemented
}

func (UnimplementedHostClient) CopyCollection(context.Context, string, string, string, string) (int64, error) {
	return 0, ErrNotImplemented
}

func (UnimplementedHostClient) PublishCollection(context.Context, string, string, string) (string, string, error) {
	return "", "", ErrNotImplemented
}

func (UnimplementedHostClient) UnpublishCollection(context.Context, string, string, string) (bool, error) {
	return false, ErrNotImplemented
}

func (UnimplementedHostClient) ListPublications(context.Context, string, string) ([]Publication, error) {
	return nil, ErrNotImplemented
}

var _ HostClient = UnimplementedHostClient{}
