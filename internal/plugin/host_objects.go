package plugin

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/secretbox"
	"github.com/bornholm/automata/internal/weblink"
	"github.com/bornholm/automata/pkg/pluginsdk"
	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// Magasin d'objets du service hôte : des octets groupés en collections,
// scopés (plugin, org, membre) comme les configs et les secrets. Par
// défaut les objets ne sont PAS scellés au repos — le magasin porte du
// contenu destiné à être servi publiquement (route /s/), jamais de secret.
//
// Un plugin peut demander le scellement objet par objet (PutObjectMetadata
// .sealed) : c'est le casier personnel, où une personne range ses propres
// documents. Deux règles l'encadrent, toutes deux ci-dessous : une
// collection contenant un objet scellé ne peut jamais être publiée, et une
// écriture scellée est refusée si la clé manque — jamais dégradée en clair.

// ObjectStoreLimits borne le magasin d'objets. Les zéros prennent les
// défauts ci-dessous.
type ObjectStoreLimits struct {
	// MaxObjectBytes est la taille maximale d'un objet.
	MaxObjectBytes int64
	// MaxMemberBytes est le volume total permis à un (plugin, org, membre).
	MaxMemberBytes int64
	// MaxMemberObjects est le nombre d'objets permis au même périmètre.
	MaxMemberObjects int64
}

const (
	defaultMaxObjectBytes   = 16 << 20
	defaultMaxMemberBytes   = 64 << 20
	defaultMaxMemberObjects = 500

	// slugLength donne ~50 bits d'aléa : le lien public n'est pas listé,
	// sa non-devinabilité est la seule discrétion offerte.
	slugLength = 10
)

// withDefaults remplace les zéros par les défauts.
func (l ObjectStoreLimits) withDefaults() ObjectStoreLimits {
	if l.MaxObjectBytes <= 0 {
		l.MaxObjectBytes = defaultMaxObjectBytes
	}
	if l.MaxMemberBytes <= 0 {
		l.MaxMemberBytes = defaultMaxMemberBytes
	}
	if l.MaxMemberObjects <= 0 {
		l.MaxMemberObjects = defaultMaxMemberObjects
	}
	return l
}

// PreviewMinter fabrique une URL de prévisualisation signée pour une
// collection. Fournie par le serveur web via le registre : le secret de
// signature ne traverse jamais ce paquet.
type PreviewMinter func(pluginName, orgID, memberID, collection string) (url string, expiresAt time.Time, err error)

// WithObjectStore branche le magasin d'objets et les publications. baseURL
// est l'URL publique du serveur web, dont dérivent les URLs /s/<slug>.
func (h *HostService) WithObjectStore(baseURL string, limits ObjectStoreLimits) *HostService {
	h.objects = persistence.NewPluginObjectRepository()
	h.sites = persistence.NewPluginPublicSiteRepository()
	h.baseURL = strings.TrimRight(baseURL, "/")
	h.objectLimits = limits.withDefaults()
	return h
}

// WithObjectSealing branche la clé du casier personnel. Sans elle, les
// objets ordinaires continuent de fonctionner et seules les écritures
// scellées sont refusées : une instance sans secret exploitable perd le
// casier, pas le magasin.
func (h *HostService) WithObjectSealing(box *secretbox.Box) *HostService {
	h.objectBox = box
	return h
}

// sealObject chiffre les octets d'un objet de casier. L'absence de clé est
// une erreur franche : enregistrer en clair un document qu'on a promis de
// sceller serait une trahison silencieuse du contrat.
func (s *scopedHostService) sealObject(data []byte) ([]byte, error) {
	if s.objectBox == nil {
		return nil, status.Error(codes.FailedPrecondition,
			"sealed objects are unavailable on this instance (no usable encryption key)")
	}

	sealed, err := s.objectBox.SealRaw(data)
	if err != nil {
		return nil, status.Error(codes.Internal, "object sealing failed")
	}

	return sealed, nil
}

// openObject rend les octets en clair d'un objet, scellé ou non.
func (s *scopedHostService) openObject(o persistence.PluginObject) ([]byte, error) {
	if !o.Sealed {
		return o.Data, nil
	}
	if s.objectBox == nil {
		return nil, status.Error(codes.FailedPrecondition,
			"this object is sealed and no usable encryption key is configured")
	}

	data, err := s.objectBox.OpenRaw(o.Data)
	if err != nil {
		return nil, status.Error(codes.Internal, "object unsealing failed")
	}

	return data, nil
}

// WithPreviewMinter branche la fabrique de liens de prévisualisation ;
// sans elle, PreviewCollection répond une erreur claire.
func (h *HostService) WithPreviewMinter(mint PreviewMinter) *HostService {
	h.previewMint = mint
	return h
}

// objectStoreReady répond une erreur claire tant que le magasin n'est pas
// câblé, plutôt qu'un panic sur repo nil.
func (s *scopedHostService) objectStoreReady() error {
	if s.objects == nil || s.sites == nil {
		return status.Error(codes.Unavailable, "object store not wired")
	}
	return nil
}

// objectNamePattern encadre collections et clés : minuscules, chiffres et
// séparateurs sûrs. Les valeurs finissent dans des URLs publiques et des
// requêtes SQL de préfixe — pas de place pour la fantaisie.
var objectNamePattern = regexp.MustCompile(`^[a-z0-9._/-]{1,200}$`)

// validObjectName refuse les noms dangereux : métacaractères, segments
// vides, « .. », séparateur en tête ou en queue.
func validObjectName(name string) bool {
	if !objectNamePattern.MatchString(name) {
		return false
	}
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") {
		return false
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func checkObjectNames(collection, key string) error {
	if !validObjectName(collection) {
		return status.Error(codes.InvalidArgument, "invalid collection name")
	}
	if key != "" && !validObjectName(key) {
		return status.Error(codes.InvalidArgument, "invalid object key")
	}
	return nil
}

// PutObject implémente proto.AutomataHostServiceServer : réception en
// tranches, quotas, puis upsert.
func (s *scopedHostService) PutObject(stream proto.AutomataHostService_PutObjectServer) error {
	if err := s.objectStoreReady(); err != nil {
		return err
	}

	first, err := stream.Recv()
	if err != nil {
		return status.Error(codes.InvalidArgument, "metadata chunk expected")
	}
	meta := first.GetMetadata()
	if meta == nil {
		return status.Error(codes.InvalidArgument, "metadata chunk expected first")
	}
	if err := checkObjectNames(meta.Collection, meta.Key); err != nil {
		return err
	}
	if meta.Key == "" {
		return status.Error(codes.InvalidArgument, "invalid object key")
	}

	var buf bytes.Buffer
	for {
		chunk, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return status.Error(codes.Internal, "object upload interrupted")
		}
		data := chunk.GetData()
		if int64(buf.Len())+int64(len(data)) > s.objectLimits.MaxObjectBytes {
			return status.Errorf(codes.ResourceExhausted,
				"object exceeds the %d MiB per-object limit", s.objectLimits.MaxObjectBytes>>20)
		}
		buf.Write(data)
	}

	ctx := stream.Context()
	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkScope(ctx, tx, meta.OrgId, meta.MemberId); err != nil {
			return err
		}

		// L'assiette du quota déduit l'objet remplacé le cas échéant :
		// réécrire index.html ne doit pas consommer deux fois sa taille.
		usedBytes, usedCount, err := s.objects.Usage(ctx, tx, s.plugin, meta.OrgId, meta.MemberId)
		if err != nil {
			return status.Error(codes.Internal, "object store usage lookup failed")
		}
		previous, replacing, err := s.objects.Get(ctx, tx, s.plugin, meta.OrgId, meta.MemberId, meta.Collection, meta.Key)
		if err != nil {
			return status.Error(codes.Internal, "object lookup failed")
		}
		if replacing {
			usedBytes -= previous.Size
			usedCount--
		}
		if usedBytes+int64(buf.Len()) > s.objectLimits.MaxMemberBytes {
			return status.Errorf(codes.ResourceExhausted,
				"storage quota exceeded (%d MiB total): delete unused objects or spaces first",
				s.objectLimits.MaxMemberBytes>>20)
		}
		if usedCount+1 > s.objectLimits.MaxMemberObjects {
			return status.Errorf(codes.ResourceExhausted,
				"object count quota exceeded (%d objects): delete unused objects or spaces first",
				s.objectLimits.MaxMemberObjects)
		}

		// Le scellement n'intervient qu'ici, une fois les quotas vérifiés :
		// ils portent sur la taille EN CLAIR, celle que voit la personne.
		data := buf.Bytes()
		if meta.Sealed {
			var err error
			if data, err = s.sealObject(data); err != nil {
				return err
			}
		}

		now := time.Now().UTC()
		createdAt := now
		if replacing {
			createdAt = previous.CreatedAt
		}
		return s.objects.Upsert(ctx, tx, persistence.PluginObject{
			PluginName:  s.plugin,
			OrgID:       meta.OrgId,
			MemberID:    meta.MemberId,
			Collection:  meta.Collection,
			Key:         meta.Key,
			ContentType: meta.ContentType,
			Size:        int64(buf.Len()),
			Data:        data,
			Sealed:      meta.Sealed,
			CreatedAt:   createdAt,
			UpdatedAt:   now,
		})
	})
	if err != nil {
		return grpcErr(err)
	}

	return stream.SendAndClose(&proto.PutObjectResult{Size: int64(buf.Len())})
}

// GetObject implémente proto.AutomataHostServiceServer.
func (s *scopedHostService) GetObject(req *proto.GetObjectRequest, stream proto.AutomataHostService_GetObjectServer) error {
	if err := s.objectStoreReady(); err != nil {
		return err
	}
	if err := checkObjectNames(req.Collection, req.Key); err != nil {
		return err
	}

	ctx := stream.Context()

	var (
		object persistence.PluginObject
		found  bool
	)
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkScope(ctx, tx, req.OrgId, req.MemberId); err != nil {
			return err
		}

		var err error
		object, found, err = s.objects.Get(ctx, tx, s.plugin, req.OrgId, req.MemberId, req.Collection, req.Key)
		if err != nil {
			return status.Error(codes.Internal, "object lookup failed")
		}
		return nil
	})
	if err != nil {
		return grpcErr(err)
	}

	if !found {
		return stream.Send(&proto.GetObjectChunk{Payload: &proto.GetObjectChunk_Metadata{
			Metadata: &proto.GetObjectMetadata{Found: false},
		}})
	}

	// L'ouverture précède l'envoi des métadonnées : un objet illisible doit
	// ressortir en erreur franche, pas en flux tronqué derrière un en-tête
	// annonçant une taille qui ne viendra jamais.
	data, err := s.openObject(object)
	if err != nil {
		return err
	}

	if err := stream.Send(&proto.GetObjectChunk{Payload: &proto.GetObjectChunk_Metadata{
		Metadata: &proto.GetObjectMetadata{ContentType: object.ContentType, Size: object.Size, Found: true},
	}}); err != nil {
		return status.Error(codes.Internal, "object download interrupted")
	}

	for offset := 0; offset < len(data); offset += pluginsdk.ChunkBytes {
		end := min(offset+pluginsdk.ChunkBytes, len(data))
		if err := stream.Send(&proto.GetObjectChunk{Payload: &proto.GetObjectChunk_Data{Data: data[offset:end]}}); err != nil {
			return status.Error(codes.Internal, "object download interrupted")
		}
	}

	return nil
}

// DeleteObject implémente proto.AutomataHostServiceServer.
func (s *scopedHostService) DeleteObject(ctx context.Context, req *proto.DeleteObjectRequest) (*proto.DeleteObjectResponse, error) {
	if err := s.objectStoreReady(); err != nil {
		return nil, err
	}
	if err := checkObjectNames(req.Collection, req.Key); err != nil {
		return nil, err
	}

	var deleted bool
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkScope(ctx, tx, req.OrgId, req.MemberId); err != nil {
			return err
		}

		var err error
		deleted, err = s.objects.Delete(ctx, tx, s.plugin, req.OrgId, req.MemberId, req.Collection, req.Key)
		if err != nil {
			return status.Error(codes.Internal, "object deletion failed")
		}
		return nil
	})
	if err != nil {
		return nil, grpcErr(err)
	}

	return &proto.DeleteObjectResponse{Deleted: deleted}, nil
}

// DeleteCollection implémente proto.AutomataHostServiceServer. La
// publication éventuelle de la collection tombe dans la même transaction :
// pas de lien public qui survive à son contenu.
func (s *scopedHostService) DeleteCollection(ctx context.Context, req *proto.DeleteCollectionRequest) (*proto.DeleteCollectionResponse, error) {
	if err := s.objectStoreReady(); err != nil {
		return nil, err
	}
	if err := checkObjectNames(req.Collection, ""); err != nil {
		return nil, err
	}

	var deleted int64
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkScope(ctx, tx, req.OrgId, req.MemberId); err != nil {
			return err
		}

		if _, err := s.sites.DeleteByCollection(ctx, tx, s.plugin, req.OrgId, req.MemberId, req.Collection); err != nil {
			return status.Error(codes.Internal, "unpublish failed")
		}

		var err error
		deleted, err = s.objects.DeleteCollection(ctx, tx, s.plugin, req.OrgId, req.MemberId, req.Collection)
		if err != nil {
			return status.Error(codes.Internal, "collection deletion failed")
		}
		return nil
	})
	if err != nil {
		return nil, grpcErr(err)
	}

	return &proto.DeleteCollectionResponse{Deleted: deleted}, nil
}

// ListObjects implémente proto.AutomataHostServiceServer.
func (s *scopedHostService) ListObjects(ctx context.Context, req *proto.ListObjectsRequest) (*proto.ListObjectsResponse, error) {
	if err := s.objectStoreReady(); err != nil {
		return nil, err
	}
	if err := checkObjectNames(req.Collection, ""); err != nil {
		return nil, err
	}

	var entries []*proto.ObjectEntry
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkScope(ctx, tx, req.OrgId, req.MemberId); err != nil {
			return err
		}

		metas, err := s.objects.List(ctx, tx, s.plugin, req.OrgId, req.MemberId, req.Collection)
		if err != nil {
			return status.Error(codes.Internal, "object listing failed")
		}
		for _, m := range metas {
			entries = append(entries, &proto.ObjectEntry{
				Key:         m.Key,
				ContentType: m.ContentType,
				Size:        m.Size,
				UpdatedAt:   m.UpdatedAt.Format(time.RFC3339),
				Sealed:      m.Sealed,
			})
		}
		return nil
	})
	if err != nil {
		return nil, grpcErr(err)
	}

	return &proto.ListObjectsResponse{Entries: entries}, nil
}

// ListCollections implémente proto.AutomataHostServiceServer.
func (s *scopedHostService) ListCollections(ctx context.Context, req *proto.ListCollectionsRequest) (*proto.ListCollectionsResponse, error) {
	if err := s.objectStoreReady(); err != nil {
		return nil, err
	}

	var collections []string
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkScope(ctx, tx, req.OrgId, req.MemberId); err != nil {
			return err
		}

		var err error
		collections, err = s.objects.ListCollections(ctx, tx, s.plugin, req.OrgId, req.MemberId, req.Prefix)
		if err != nil {
			return status.Error(codes.Internal, "collection listing failed")
		}
		return nil
	})
	if err != nil {
		return nil, grpcErr(err)
	}

	return &proto.ListCollectionsResponse{Collections: collections}, nil
}

// CopyCollection implémente proto.AutomataHostServiceServer : la cible est
// remplacée par une copie de la source, dans une seule transaction.
func (s *scopedHostService) CopyCollection(ctx context.Context, req *proto.CopyCollectionRequest) (*proto.CopyCollectionResponse, error) {
	if err := s.objectStoreReady(); err != nil {
		return nil, err
	}
	if err := checkObjectNames(req.From, ""); err != nil {
		return nil, err
	}
	if err := checkObjectNames(req.To, ""); err != nil {
		return nil, err
	}
	if req.From == req.To {
		return nil, status.Error(codes.InvalidArgument, "source and target collections are the same")
	}

	var copied int64
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkScope(ctx, tx, req.OrgId, req.MemberId); err != nil {
			return err
		}

		// La copie double transitoirement le volume de la collection ; le
		// quota s'applique au résultat final, ancienne cible déduite.
		usedBytes, _, err := s.objects.Usage(ctx, tx, s.plugin, req.OrgId, req.MemberId)
		if err != nil {
			return status.Error(codes.Internal, "object store usage lookup failed")
		}
		fromBytes, err := collectionBytes(ctx, tx, s.objects, s.plugin, req.OrgId, req.MemberId, req.From)
		if err != nil {
			return err
		}
		toBytes, err := collectionBytes(ctx, tx, s.objects, s.plugin, req.OrgId, req.MemberId, req.To)
		if err != nil {
			return err
		}
		if usedBytes-toBytes+fromBytes > s.objectLimits.MaxMemberBytes {
			return status.Errorf(codes.ResourceExhausted,
				"storage quota exceeded (%d MiB total): delete unused objects or spaces first",
				s.objectLimits.MaxMemberBytes>>20)
		}

		copied, err = s.objects.ReplaceCollection(ctx, tx, s.plugin, req.OrgId, req.MemberId, req.From, req.To)
		if err != nil {
			return status.Error(codes.Internal, "collection copy failed")
		}
		return nil
	})
	if err != nil {
		return nil, grpcErr(err)
	}

	return &proto.CopyCollectionResponse{Copied: copied}, nil
}

// collectionBytes additionne les tailles d'une collection.
func collectionBytes(ctx context.Context, tx *sql.Tx, repo *persistence.PluginObjectRepository, plugin, orgID, memberID, collection string) (int64, error) {
	metas, err := repo.List(ctx, tx, plugin, orgID, memberID, collection)
	if err != nil {
		return 0, status.Error(codes.Internal, "object listing failed")
	}
	var total int64
	for _, m := range metas {
		total += m.Size
	}
	return total, nil
}

// PublishCollection implémente proto.AutomataHostServiceServer. Le slug
// est stable tant que la publication existe ; une collection vide est
// refusée — publier du rien donnerait un lien qui ne sert que des 404.
func (s *scopedHostService) PublishCollection(ctx context.Context, req *proto.PublishCollectionRequest) (*proto.PublishCollectionResponse, error) {
	if err := s.objectStoreReady(); err != nil {
		return nil, err
	}
	if err := checkObjectNames(req.Collection, ""); err != nil {
		return nil, err
	}

	var slug string
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkScope(ctx, tx, req.OrgId, req.MemberId); err != nil {
			return err
		}

		metas, err := s.objects.List(ctx, tx, s.plugin, req.OrgId, req.MemberId, req.Collection)
		if err != nil {
			return status.Error(codes.Internal, "object listing failed")
		}
		if len(metas) == 0 {
			return status.Error(codes.FailedPrecondition, "cannot publish an empty collection")
		}

		// Un casier ne se publie pas. Le site public sert les octets
		// stockés tels quels : ceux d'un objet scellé sont chiffrés, et
		// « réparer » l'affichage en les ouvrant à la volée reviendrait à
		// exposer en clair, sur une URL publique, ce que le scellement
		// promet de protéger. Le refus est ici, à la source, plutôt que
		// dans la route de service — un contrôle par voie d'accès finit
		// toujours par en oublier une.
		for _, meta := range metas {
			if meta.Sealed {
				return status.Error(codes.FailedPrecondition,
					"cannot publish a collection holding sealed objects")
			}
		}

		now := time.Now().UTC()
		existing, found, err := s.sites.FindByCollection(ctx, tx, s.plugin, req.OrgId, req.MemberId, req.Collection)
		if err != nil {
			return status.Error(codes.Internal, "publication lookup failed")
		}
		if found {
			slug = existing.Slug
			if err := s.sites.Touch(ctx, tx, slug, now); err != nil {
				return status.Error(codes.Internal, "publication refresh failed")
			}
			return nil
		}

		// Trois tentatives sur collision de slug : à ~50 bits d'aléa la
		// première suffit, la boucle n'est là que pour l'honnêteté.
		var lastErr error
		for range 3 {
			candidate, err := weblink.RandomCrockford(slugLength)
			if err != nil {
				return status.Error(codes.Internal, "slug generation failed")
			}
			candidate = strings.ToLower(candidate)

			if err := s.sites.Insert(ctx, tx, persistence.PluginPublicSite{
				Slug:        candidate,
				PluginName:  s.plugin,
				OrgID:       req.OrgId,
				MemberID:    req.MemberId,
				Collection:  req.Collection,
				PublishedAt: now,
			}); err != nil {
				lastErr = err
				continue
			}
			slug = candidate
			return nil
		}
		slog.ErrorContext(ctx, "plugin: génération de slug en échec", "plugin", s.plugin, "error", lastErr)
		return status.Error(codes.Internal, "slug allocation failed")
	})
	if err != nil {
		return nil, grpcErr(err)
	}

	slog.InfoContext(ctx, "plugin: collection publiée",
		"plugin", s.plugin, "org_id", req.OrgId, "slug", slug)

	return &proto.PublishCollectionResponse{Slug: slug, Url: s.publicURL(slug)}, nil
}

// PreviewCollection implémente proto.AutomataHostServiceServer : une URL
// signée éphémère vers la collection, pour les yeux du membre — aucune
// exposition publique, rien d'écrit en base.
func (s *scopedHostService) PreviewCollection(ctx context.Context, req *proto.PreviewCollectionRequest) (*proto.PreviewCollectionResponse, error) {
	if err := s.objectStoreReady(); err != nil {
		return nil, err
	}
	if s.previewMint == nil {
		return nil, status.Error(codes.Unavailable, "preview links not wired")
	}
	if err := checkObjectNames(req.Collection, ""); err != nil {
		return nil, err
	}

	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkScope(ctx, tx, req.OrgId, req.MemberId); err != nil {
			return err
		}

		metas, err := s.objects.List(ctx, tx, s.plugin, req.OrgId, req.MemberId, req.Collection)
		if err != nil {
			return status.Error(codes.Internal, "object listing failed")
		}
		if len(metas) == 0 {
			return status.Error(codes.FailedPrecondition, "cannot preview an empty collection")
		}
		return nil
	})
	if err != nil {
		return nil, grpcErr(err)
	}

	url, expires, err := s.previewMint(s.plugin, req.OrgId, req.MemberId, req.Collection)
	if err != nil {
		return nil, status.Error(codes.Internal, "preview link minting failed")
	}

	return &proto.PreviewCollectionResponse{Url: url, ExpiresAt: expires.UTC().Format(time.RFC3339)}, nil
}

// UnpublishCollection implémente proto.AutomataHostServiceServer.
func (s *scopedHostService) UnpublishCollection(ctx context.Context, req *proto.UnpublishCollectionRequest) (*proto.UnpublishCollectionResponse, error) {
	if err := s.objectStoreReady(); err != nil {
		return nil, err
	}
	if err := checkObjectNames(req.Collection, ""); err != nil {
		return nil, err
	}

	var existed bool
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkScope(ctx, tx, req.OrgId, req.MemberId); err != nil {
			return err
		}

		var err error
		existed, err = s.sites.DeleteByCollection(ctx, tx, s.plugin, req.OrgId, req.MemberId, req.Collection)
		if err != nil {
			return status.Error(codes.Internal, "unpublish failed")
		}
		return nil
	})
	if err != nil {
		return nil, grpcErr(err)
	}

	return &proto.UnpublishCollectionResponse{Existed: existed}, nil
}

// ListPublications implémente proto.AutomataHostServiceServer.
func (s *scopedHostService) ListPublications(ctx context.Context, req *proto.ListPublicationsRequest) (*proto.ListPublicationsResponse, error) {
	if err := s.objectStoreReady(); err != nil {
		return nil, err
	}

	var publications []*proto.Publication
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkScope(ctx, tx, req.OrgId, req.MemberId); err != nil {
			return err
		}

		sites, err := s.sites.ListByMember(ctx, tx, s.plugin, req.OrgId, req.MemberId)
		if err != nil {
			return status.Error(codes.Internal, "publication listing failed")
		}
		for _, site := range sites {
			publications = append(publications, &proto.Publication{
				Collection:  site.Collection,
				Slug:        site.Slug,
				Url:         s.publicURL(site.Slug),
				PublishedAt: site.PublishedAt.Format(time.RFC3339),
			})
		}
		return nil
	})
	if err != nil {
		return nil, grpcErr(err)
	}

	return &proto.ListPublicationsResponse{Publications: publications}, nil
}

// publicURL compose l'URL publique d'un slug.
func (s *scopedHostService) publicURL(slug string) string {
	return fmt.Sprintf("%s/s/%s/", s.baseURL, slug)
}
