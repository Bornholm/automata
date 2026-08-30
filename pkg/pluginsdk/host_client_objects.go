package pluginsdk

import (
	"context"
	"errors"
	"fmt"
	"io"

	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// Façade typée du magasin d'objets hôte. Le streaming gRPC (tranches de
// 1 Mio) est encapsulé ici : côté plugin l'API travaille en []byte, un
// objet plafonné à 16 Mio tient en mémoire.

// ObjectEntry is one stored object, as returned by ListObjects.
type ObjectEntry struct {
	Key         string
	ContentType string
	// Size is the plaintext size, sealed or not.
	Size      int64
	UpdatedAt string
	Sealed    bool
}

// Publication is one published collection, as returned by ListPublications.
type Publication struct {
	Collection  string
	Slug        string
	URL         string
	PublishedAt string
}

// objectChunkBytes est la taille des tranches montantes, alignée sur le
// découpage PutFile/GetFile de l'hôte.
const objectChunkBytes = 1 << 20

func (c *grpcHostClient) PutObject(ctx context.Context, orgID, memberID, collection, key, contentType string, data []byte) error {
	return c.putObject(ctx, orgID, memberID, collection, key, contentType, data, false)
}

func (c *grpcHostClient) PutObjectSealed(ctx context.Context, orgID, memberID, collection, key, contentType string, data []byte) error {
	return c.putObject(ctx, orgID, memberID, collection, key, contentType, data, true)
}

func (c *grpcHostClient) putObject(ctx context.Context, orgID, memberID, collection, key, contentType string, data []byte, sealed bool) error {
	stream, err := c.client.PutObject(ctx)
	if err != nil {
		return fmt.Errorf("PutObject: %w", err)
	}

	if err := stream.Send(&proto.PutObjectChunk{Payload: &proto.PutObjectChunk_Metadata{
		Metadata: &proto.PutObjectMetadata{
			OrgId: orgID, MemberId: memberID,
			Collection: collection, Key: key, ContentType: contentType,
			Sealed: sealed,
		},
	}}); err != nil {
		return fmt.Errorf("PutObject: %w", err)
	}

	for offset := 0; offset < len(data); offset += objectChunkBytes {
		end := min(offset+objectChunkBytes, len(data))
		if err := stream.Send(&proto.PutObjectChunk{Payload: &proto.PutObjectChunk_Data{Data: data[offset:end]}}); err != nil {
			// L'erreur réelle (quota…) arrive à la clôture du flux.
			break
		}
	}

	if _, err := stream.CloseAndRecv(); err != nil {
		return fmt.Errorf("PutObject: %w", err)
	}
	return nil
}

func (c *grpcHostClient) GetObject(ctx context.Context, orgID, memberID, collection, key string) (data []byte, contentType string, found bool, err error) {
	stream, err := c.client.GetObject(ctx, &proto.GetObjectRequest{
		OrgId: orgID, MemberId: memberID, Collection: collection, Key: key,
	})
	if err != nil {
		return nil, "", false, fmt.Errorf("GetObject: %w", err)
	}

	first, err := stream.Recv()
	if err != nil {
		return nil, "", false, fmt.Errorf("GetObject: %w", err)
	}
	meta := first.GetMetadata()
	if meta == nil {
		return nil, "", false, fmt.Errorf("GetObject: metadata chunk expected first")
	}
	if !meta.Found {
		return nil, "", false, nil
	}

	buf := make([]byte, 0, meta.Size)
	for {
		chunk, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, "", false, fmt.Errorf("GetObject: %w", err)
		}
		buf = append(buf, chunk.GetData()...)
	}

	return buf, meta.ContentType, true, nil
}

func (c *grpcHostClient) DeleteObject(ctx context.Context, orgID, memberID, collection, key string) (bool, error) {
	resp, err := c.client.DeleteObject(ctx, &proto.DeleteObjectRequest{
		OrgId: orgID, MemberId: memberID, Collection: collection, Key: key,
	})
	if err != nil {
		return false, fmt.Errorf("DeleteObject: %w", err)
	}
	return resp.Deleted, nil
}

func (c *grpcHostClient) DeleteCollection(ctx context.Context, orgID, memberID, collection string) (int64, error) {
	resp, err := c.client.DeleteCollection(ctx, &proto.DeleteCollectionRequest{
		OrgId: orgID, MemberId: memberID, Collection: collection,
	})
	if err != nil {
		return 0, fmt.Errorf("DeleteCollection: %w", err)
	}
	return resp.Deleted, nil
}

func (c *grpcHostClient) ListObjects(ctx context.Context, orgID, memberID, collection string) ([]ObjectEntry, error) {
	resp, err := c.client.ListObjects(ctx, &proto.ListObjectsRequest{
		OrgId: orgID, MemberId: memberID, Collection: collection,
	})
	if err != nil {
		return nil, fmt.Errorf("ListObjects: %w", err)
	}

	entries := make([]ObjectEntry, 0, len(resp.Entries))
	for _, e := range resp.Entries {
		entries = append(entries, ObjectEntry{
			Key: e.Key, ContentType: e.ContentType, Size: e.Size,
			UpdatedAt: e.UpdatedAt, Sealed: e.Sealed,
		})
	}
	return entries, nil
}

func (c *grpcHostClient) ListCollections(ctx context.Context, orgID, memberID, prefix string) ([]string, error) {
	resp, err := c.client.ListCollections(ctx, &proto.ListCollectionsRequest{
		OrgId: orgID, MemberId: memberID, Prefix: prefix,
	})
	if err != nil {
		return nil, fmt.Errorf("ListCollections: %w", err)
	}
	return resp.Collections, nil
}

func (c *grpcHostClient) CopyCollection(ctx context.Context, orgID, memberID, from, to string) (int64, error) {
	resp, err := c.client.CopyCollection(ctx, &proto.CopyCollectionRequest{
		OrgId: orgID, MemberId: memberID, From: from, To: to,
	})
	if err != nil {
		return 0, fmt.Errorf("CopyCollection: %w", err)
	}
	return resp.Copied, nil
}

func (c *grpcHostClient) PublishCollection(ctx context.Context, orgID, memberID, collection string) (slug, url string, err error) {
	resp, err := c.client.PublishCollection(ctx, &proto.PublishCollectionRequest{
		OrgId: orgID, MemberId: memberID, Collection: collection,
	})
	if err != nil {
		return "", "", fmt.Errorf("PublishCollection: %w", err)
	}
	return resp.Slug, resp.Url, nil
}

func (c *grpcHostClient) UnpublishCollection(ctx context.Context, orgID, memberID, collection string) (bool, error) {
	resp, err := c.client.UnpublishCollection(ctx, &proto.UnpublishCollectionRequest{
		OrgId: orgID, MemberId: memberID, Collection: collection,
	})
	if err != nil {
		return false, fmt.Errorf("UnpublishCollection: %w", err)
	}
	return resp.Existed, nil
}

func (c *grpcHostClient) PreviewCollection(ctx context.Context, orgID, memberID, collection string) (url, expiresAt string, err error) {
	resp, err := c.client.PreviewCollection(ctx, &proto.PreviewCollectionRequest{
		OrgId: orgID, MemberId: memberID, Collection: collection,
	})
	if err != nil {
		return "", "", fmt.Errorf("PreviewCollection: %w", err)
	}
	return resp.Url, resp.ExpiresAt, nil
}

// ShareFile implémente HostClient.
func (c *grpcHostClient) ShareFile(ctx context.Context, orgID, memberID, path string) (url, expiresAt string, err error) {
	resp, err := c.client.ShareFile(ctx, &proto.ShareFileRequest{
		OrgId:    orgID,
		MemberId: memberID,
		Path:     path,
	})
	if err != nil {
		return "", "", fmt.Errorf("ShareFile: %w", err)
	}

	return resp.GetUrl(), resp.GetExpiresAt(), nil
}

func (c *grpcHostClient) ListPublications(ctx context.Context, orgID, memberID string) ([]Publication, error) {
	resp, err := c.client.ListPublications(ctx, &proto.ListPublicationsRequest{
		OrgId: orgID, MemberId: memberID,
	})
	if err != nil {
		return nil, fmt.Errorf("ListPublications: %w", err)
	}

	publications := make([]Publication, 0, len(resp.Publications))
	for _, p := range resp.Publications {
		publications = append(publications, Publication{
			Collection: p.Collection, Slug: p.Slug, URL: p.Url, PublishedAt: p.PublishedAt,
		})
	}
	return publications, nil
}
