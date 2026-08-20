package plugin

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/secretbox"
	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// Notifier porte un message applicatif jusqu'au canal privé d'un membre.
// Implémenté par le routeur de déclencheurs (lot C) ; nil tant qu'il n'est
// pas câblé — Notify répond alors une erreur claire.
type Notifier interface {
	NotifyMember(ctx context.Context, orgID, memberID, text string) error
}

// HostService est l'état partagé du service hôte : persistance et
// scellement. Il n'est JAMAIS exposé tel quel à un plugin : chaque
// connexion broker reçoit une vue scopedHostService liée au plugin qui l'a
// établie — c'est la correction de la principale faiblesse du patron Xolo,
// où tout plugin pouvait lire les secrets de tous.
type HostService struct {
	db          *persistence.DB
	box         *secretbox.Box
	configs     *persistence.PluginConfigRepository
	secrets     *persistence.PluginSecretRepository
	activations *persistence.PluginActivationRepository
	orgs        *persistence.OrganizationRepository
	members     *persistence.MemberRepository
	notifier    Notifier
}

// NewHostService crée le service hôte.
func NewHostService(db *persistence.DB, box *secretbox.Box) *HostService {
	return &HostService{
		db:          db,
		box:         box,
		configs:     persistence.NewPluginConfigRepository(),
		secrets:     persistence.NewPluginSecretRepository(),
		activations: persistence.NewPluginActivationRepository(),
		orgs:        persistence.NewOrganizationRepository(),
		members:     persistence.NewMemberRepository(),
	}
}

// WithNotifier branche l'envoi de notifications (lot C).
func (h *HostService) WithNotifier(n Notifier) *HostService {
	h.notifier = n
	return h
}

// scopedTo lie une vue du service au plugin nommé.
func (h *HostService) scopedTo(pluginName string) *scopedHostService {
	return &scopedHostService{HostService: h, plugin: pluginName}
}

// scopedHostService implémente proto.AutomataHostServiceServer pour UN
// plugin. Le nom est figé côté hôte à l'établissement de la connexion.
type scopedHostService struct {
	proto.UnimplementedAutomataHostServiceServer
	*HostService
	plugin string
}

// checkScope vérifie que l'organisation existe et, si un membre est
// désigné, qu'il lui appartient. Sans cette vérification, un plugin (ou un
// bogue de plugin) pourrait ranger des secrets sous des identifiants
// fantaisistes ou lire ceux d'un membre d'une autre organisation.
func (s *scopedHostService) checkScope(ctx context.Context, tx *sql.Tx, orgID, memberID string) error {
	if orgID == "" {
		return status.Error(codes.InvalidArgument, "org_id required")
	}

	if _, found, err := s.orgs.FindByID(ctx, tx, orgID); err != nil {
		return status.Error(codes.Internal, "org lookup failed")
	} else if !found {
		return status.Error(codes.NotFound, "unknown organization")
	}

	if memberID != "" {
		member, found, err := s.members.FindByID(ctx, tx, memberID)
		if err != nil {
			return status.Error(codes.Internal, "member lookup failed")
		}
		if !found || member.OrgID != orgID {
			return status.Error(codes.NotFound, "member not in organization")
		}
	}

	return nil
}

// GetConfig implémente proto.AutomataHostServiceServer.
func (s *scopedHostService) GetConfig(ctx context.Context, req *proto.GetConfigRequest) (*proto.GetConfigResponse, error) {
	var resp proto.GetConfigResponse

	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkScope(ctx, tx, req.OrgId, req.MemberId); err != nil {
			return err
		}

		cfg, found, err := s.configs.Get(ctx, tx, s.plugin, req.OrgId, req.MemberId)
		if err != nil {
			return status.Error(codes.Internal, "config lookup failed")
		}
		if !found {
			return nil
		}

		clear, err := s.box.OpenText(cfg.Config)
		if err != nil {
			return status.Error(codes.Internal, "config unreadable")
		}
		resp.ConfigJson, resp.Found = clear, true
		return nil
	})
	if err != nil {
		return nil, grpcErr(err)
	}

	return &resp, nil
}

// SaveConfig implémente proto.AutomataHostServiceServer.
func (s *scopedHostService) SaveConfig(ctx context.Context, req *proto.SaveConfigRequest) (*proto.SaveConfigResponse, error) {
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkScope(ctx, tx, req.OrgId, req.MemberId); err != nil {
			return err
		}

		sealed, err := s.box.SealText(req.ConfigJson)
		if err != nil {
			return status.Error(codes.Internal, "config sealing failed")
		}

		return s.configs.Upsert(ctx, tx, persistence.PluginConfig{
			PluginName: s.plugin,
			OrgID:      req.OrgId,
			MemberID:   req.MemberId,
			Config:     sealed,
			UpdatedAt:  time.Now().UTC(),
		})
	})
	if err != nil {
		return nil, grpcErr(err)
	}

	// Identifiants seulement : jamais le contenu de la configuration.
	slog.DebugContext(ctx, "plugin: configuration enregistrée",
		"plugin", s.plugin, "org_id", req.OrgId, "member_set", req.MemberId != "")

	return &proto.SaveConfigResponse{}, nil
}

// ListConfigs implémente proto.AutomataHostServiceServer : seules les
// organisations où le plugin est actif sont servies.
func (s *scopedHostService) ListConfigs(ctx context.Context, _ *proto.ListConfigsRequest) (*proto.ListConfigsResponse, error) {
	var entries []*proto.ConfigEntry

	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		configs, err := s.configs.ListEnabled(ctx, tx, s.plugin)
		if err != nil {
			return status.Error(codes.Internal, "config listing failed")
		}

		for _, cfg := range configs {
			clear, err := s.box.OpenText(cfg.Config)
			if err != nil {
				return status.Error(codes.Internal, "config unreadable")
			}
			entries = append(entries, &proto.ConfigEntry{
				OrgId:      cfg.OrgID,
				MemberId:   cfg.MemberID,
				ConfigJson: clear,
			})
		}
		return nil
	})
	if err != nil {
		return nil, grpcErr(err)
	}

	return &proto.ListConfigsResponse{Entries: entries}, nil
}

// GetSecret implémente proto.AutomataHostServiceServer. La valeur part en
// clair vers le plugin — il en a besoin pour s'authentifier ailleurs —
// mais jamais vers l'interface web ni les journaux.
func (s *scopedHostService) GetSecret(ctx context.Context, req *proto.GetSecretRequest) (*proto.GetSecretResponse, error) {
	var resp proto.GetSecretResponse

	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkScope(ctx, tx, req.OrgId, req.MemberId); err != nil {
			return err
		}

		sealed, found, err := s.secrets.Get(ctx, tx, s.plugin, req.OrgId, req.MemberId, req.Key)
		if err != nil {
			return status.Error(codes.Internal, "secret lookup failed")
		}
		if !found {
			return nil
		}

		clear, err := s.box.Open(sealed)
		if err != nil {
			return status.Error(codes.Internal, "secret unreadable")
		}
		resp.Value, resp.Found = clear, true
		return nil
	})
	if err != nil {
		return nil, grpcErr(err)
	}

	return &resp, nil
}

// SetSecret implémente proto.AutomataHostServiceServer.
func (s *scopedHostService) SetSecret(ctx context.Context, req *proto.SetSecretRequest) (*proto.SetSecretResponse, error) {
	if req.Key == "" {
		return nil, status.Error(codes.InvalidArgument, "key required")
	}

	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkScope(ctx, tx, req.OrgId, req.MemberId); err != nil {
			return err
		}

		sealed, err := s.box.Seal(req.Value)
		if err != nil {
			return status.Error(codes.Internal, "secret sealing failed")
		}

		return s.secrets.Set(ctx, tx, s.plugin, req.OrgId, req.MemberId, req.Key, sealed, time.Now().UTC())
	})
	if err != nil {
		return nil, grpcErr(err)
	}

	slog.DebugContext(ctx, "plugin: secret enregistré",
		"plugin", s.plugin, "org_id", req.OrgId, "key", req.Key)

	return &proto.SetSecretResponse{}, nil
}

// DeleteSecret implémente proto.AutomataHostServiceServer.
func (s *scopedHostService) DeleteSecret(ctx context.Context, req *proto.DeleteSecretRequest) (*proto.DeleteSecretResponse, error) {
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkScope(ctx, tx, req.OrgId, req.MemberId); err != nil {
			return err
		}
		return s.secrets.Delete(ctx, tx, s.plugin, req.OrgId, req.MemberId, req.Key)
	})
	if err != nil {
		return nil, grpcErr(err)
	}

	return &proto.DeleteSecretResponse{}, nil
}

// Notify implémente proto.AutomataHostServiceServer : message applicatif
// vers le canal privé du membre, restreint aux organisations où le plugin
// est actif.
func (s *scopedHostService) Notify(ctx context.Context, req *proto.NotifyRequest) (*proto.NotifyResponse, error) {
	if s.notifier == nil {
		return nil, status.Error(codes.Unavailable, "notifications not wired")
	}
	if req.MemberId == "" || req.Text == "" {
		return nil, status.Error(codes.InvalidArgument, "member_id and text required")
	}

	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkScope(ctx, tx, req.OrgId, req.MemberId); err != nil {
			return err
		}

		enabled, err := s.activations.IsEnabled(ctx, tx, s.plugin, req.OrgId)
		if err != nil {
			return status.Error(codes.Internal, "activation lookup failed")
		}
		if !enabled {
			return status.Error(codes.PermissionDenied, "plugin not active for organization")
		}
		return nil
	})
	if err != nil {
		return nil, grpcErr(err)
	}

	if err := s.notifier.NotifyMember(ctx, req.OrgId, req.MemberId, req.Text); err != nil {
		return nil, status.Error(codes.Internal, "notification delivery failed")
	}

	return &proto.NotifyResponse{}, nil
}

// grpcErr rend les erreurs gRPC telles quelles et enveloppe le reste en
// Internal sans détail : le message d'une erreur interne peut porter des
// chemins ou des fragments qu'un plugin n'a pas à voir.
func grpcErr(err error) error {
	if _, ok := status.FromError(err); ok {
		return err
	}
	return status.Error(codes.Internal, "internal error")
}
