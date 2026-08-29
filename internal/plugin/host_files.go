package plugin

import (
	"context"
	"database/sql"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// Liens de téléchargement de fichiers de plugins (route /f/ de l'hôte).
//
// Ils existent parce que la messagerie plafonne ses pièces jointes bien en
// deçà de ce qu'un espace de travail produit. Le lien est la seconde voie
// de sortie : signé, daté, et servi EN FLUX depuis le plugin quand on
// l'ouvre — rien n'est copié ni stocké.

// FileLinkMinter fabrique une URL signée vers UN fichier du plugin.
// Fournie en closure par le paquet web (voir web.FileLinkMinter), de sorte
// que le secret de signature ne traverse ni ce paquet ni le plugin.
type FileLinkMinter func(pluginName, orgID, memberID, path string) (url string, expiresAt time.Time, err error)

// WithFileLinkMinter branche la fabrique de liens de téléchargement ; sans
// elle, ShareFile répond Unavailable et le plugin le dit à son agent.
func (h *HostService) WithFileLinkMinter(mint FileLinkMinter) *HostService {
	h.fileLinkMint = mint
	return h
}

// ShareFile implémente proto.AutomataHostServiceServer : une URL signée et
// expirante vers un fichier du plugin appelant.
//
// L'hôte ne vérifie PAS que le fichier existe — il ne sait pas lire le
// stockage du plugin. C'est au plugin de s'en assurer avant de demander le
// lien, sinon le membre reçoit une adresse qui rendra 404.
func (s *scopedHostService) ShareFile(ctx context.Context, req *proto.ShareFileRequest) (*proto.ShareFileResponse, error) {
	if s.fileLinkMint == nil {
		return nil, status.Error(codes.Unavailable, "file links not wired")
	}
	if req.Path == "" {
		return nil, status.Error(codes.InvalidArgument, "path is required")
	}

	// Même contrôle d'appartenance que partout ailleurs : un plugin ne
	// fabrique un lien que pour un membre de l'organisation qu'il sert.
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		return s.checkScope(ctx, tx, req.OrgId, req.MemberId)
	})
	if err != nil {
		return nil, grpcErr(err)
	}

	url, expires, err := s.fileLinkMint(s.plugin, req.OrgId, req.MemberId, req.Path)
	if err != nil {
		return nil, status.Error(codes.Internal, "file link minting failed")
	}

	return &proto.ShareFileResponse{Url: url, ExpiresAt: expires.UTC().Format(time.RFC3339)}, nil
}
