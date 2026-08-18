package grpcsvc

import (
	"context"
	"strings"

	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/service"
	gen "src.solsynth.dev/sosys/go/proto"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type quotaServer struct {
	gen.UnimplementedDyQuotaServiceServer
	mail *service.EmailService
}

func (s *quotaServer) GetUsedQuota(ctx context.Context, req *gen.DyGetUsedQuotaRequest) (*gen.DyGetUsedQuotaResponse, error) {
	workspaceID := strings.TrimSpace(req.GetWorkspaceId())
	if workspaceID == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	if s.mail == nil || s.mail.DB() == nil {
		return nil, status.Error(codes.FailedPrecondition, "email service is not configured")
	}

	var usedBytes int64
	if err := s.mail.DB().DB.WithContext(ctx).
		Model(&database.Email{}).
		Select("COALESCE(SUM(emails.raw_size_bytes), 0)").
		Joins("JOIN mailboxes ON mailboxes.id = emails.mailbox_id AND mailboxes.deleted_at IS NULL").
		Where("mailboxes.workspace_id = ? AND emails.archived_at IS NULL", workspaceID).
		Scan(&usedBytes).Error; err != nil {
		return nil, status.Errorf(codes.Internal, "calculate workspace mail usage: %v", err)
	}

	return &gen.DyGetUsedQuotaResponse{UsedBytes: usedBytes}, nil
}

func RegisterQuotaService(server *grpc.Server, mail *service.EmailService) {
	gen.RegisterDyQuotaServiceServer(server, &quotaServer{mail: mail})
}
