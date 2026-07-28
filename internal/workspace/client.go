// Package workspace provides the small Workspace API surface ElecPostal needs
// to authorize workspace mailboxes and derive their mail storage allowance.
package workspace

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	gen "src.solsynth.dev/sosys/go/proto"
)

const memberRoleLevel = 50

// Provider is the Workspace capability required by the email domain.
type Provider interface {
	AuthorizeMember(context.Context, string, string) error
	PlanStorageBytes(context.Context, string) (int64, error)
	MailboxLimit(context.Context, string) (int64, error)
	Close() error
}

type Client struct {
	conn   *grpc.ClientConn
	client gen.DyWorkspaceServiceClient
}

func NewClient(target string, useTLS, tlsSkipVerify bool) (*Client, error) {
	if strings.TrimSpace(target) == "" {
		return nil, fmt.Errorf("workspace target is required")
	}
	var transport credentials.TransportCredentials
	if useTLS {
		transport = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: tlsSkipVerify}) // #nosec G402 -- explicitly configured for internal deployments.
	} else {
		transport = insecure.NewCredentials()
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(transport))
	if err != nil {
		return nil, fmt.Errorf("connect to workspace: %w", err)
	}
	return &Client{conn: conn, client: gen.NewDyWorkspaceServiceClient(conn)}, nil
}

func (c *Client) AuthorizeMember(ctx context.Context, workspaceID, accountID string) error {
	member, err := c.client.IsMemberWithRole(ctx, &gen.DyIsWorkspaceMemberWithRoleRequest{
		WorkspaceId:   workspaceID,
		AccountId:     accountID,
		RequiredRoles: []int32{memberRoleLevel},
	})
	if err != nil {
		return fmt.Errorf("check workspace membership: %w", err)
	}
	if !member.GetValue() {
		return fmt.Errorf("workspace membership with member role is required")
	}
	return nil
}

func (c *Client) PlanStorageBytes(ctx context.Context, workspaceID string) (int64, error) {
	workspace, err := c.getWorkspace(ctx, workspaceID)
	if err != nil {
		return 0, fmt.Errorf("get workspace: %w", err)
	}
	quota, err := c.client.GetPlanQuota(ctx, &gen.DyGetPlanQuotaRequest{Plan: workspace.GetPlan()})
	if err != nil {
		return 0, fmt.Errorf("get workspace plan quota: %w", err)
	}
	if quota.GetMaxStorageBytes() <= 0 {
		return 0, fmt.Errorf("workspace plan has no storage quota")
	}
	return quota.GetMaxStorageBytes(), nil
}

// MailboxLimit returns the maximum number of mailboxes permitted by a
// workspace plan: Free 1, Pro 3, and Enterprise 10.
func (c *Client) MailboxLimit(ctx context.Context, workspaceID string) (int64, error) {
	workspace, err := c.getWorkspace(ctx, workspaceID)
	if err != nil {
		return 0, err
	}
	return mailboxLimitForPlan(workspace.GetPlan()), nil
}

func (c *Client) getWorkspace(ctx context.Context, workspaceID string) (*gen.DyWorkspace, error) {
	workspace, err := c.client.GetWorkspace(ctx, &gen.DyGetWorkspaceRequest{Query: &gen.DyGetWorkspaceRequest_Id{Id: workspaceID}})
	if err != nil {
		return nil, fmt.Errorf("get workspace: %w", err)
	}
	return workspace, nil
}

func mailboxLimitForPlan(plan gen.DyWorkspacePlan) int64 {
	switch plan {
	case gen.DyWorkspacePlan_ENTERPRISE:
		return 10
	case gen.DyWorkspacePlan_PRO:
		return 3
	default:
		return 1
	}
}

func (c *Client) Close() error { return c.conn.Close() }
