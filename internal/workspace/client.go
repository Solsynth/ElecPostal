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
	CustomDomainLimit(context.Context, string) (int64, error)
	SendLimits(context.Context, string) (SendLimits, error)
	Close() error
}

// SendLimits constrains outbound message attempts for both one mailbox and
// all mailboxes in a workspace. A zero value disables that particular limit.
type SendLimits struct {
	MailboxDaily     int64
	MailboxMonthly   int64
	WorkspaceDaily   int64
	WorkspaceMonthly int64
}

// SendLimitPolicy selects send limits by Workspace plan.
type SendLimitPolicy struct {
	Free       SendLimits
	Pro        SendLimits
	Enterprise SendLimits
}

func DefaultSendLimitPolicy() SendLimitPolicy {
	return SendLimitPolicy{
		Free:       SendLimits{MailboxDaily: 100, MailboxMonthly: 2000, WorkspaceDaily: 100, WorkspaceMonthly: 2000},
		Pro:        SendLimits{MailboxDaily: 1000, MailboxMonthly: 20000, WorkspaceDaily: 3000, WorkspaceMonthly: 60000},
		Enterprise: SendLimits{MailboxDaily: 5000, MailboxMonthly: 100000, WorkspaceDaily: 25000, WorkspaceMonthly: 500000},
	}
}

type Client struct {
	conn       *grpc.ClientConn
	client     gen.DyWorkspaceServiceClient
	sendLimits SendLimitPolicy
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
	return &Client{conn: conn, client: gen.NewDyWorkspaceServiceClient(conn), sendLimits: DefaultSendLimitPolicy()}, nil
}

// SetSendLimitPolicy applies deployment-specific plan limits.
func (c *Client) SetSendLimitPolicy(policy SendLimitPolicy) { c.sendLimits = policy }

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

// CustomDomainLimit returns the maximum number of SES custom domains permitted
// by a workspace plan: Free 0, Pro 1, and Enterprise 3.
func (c *Client) CustomDomainLimit(ctx context.Context, workspaceID string) (int64, error) {
	workspace, err := c.getWorkspace(ctx, workspaceID)
	if err != nil {
		return 0, err
	}
	return customDomainLimitForPlan(workspace.GetPlan()), nil
}

func (c *Client) SendLimits(ctx context.Context, workspaceID string) (SendLimits, error) {
	workspace, err := c.getWorkspace(ctx, workspaceID)
	if err != nil {
		return SendLimits{}, err
	}
	switch workspace.GetPlan() {
	case gen.DyWorkspacePlan_ENTERPRISE:
		return c.sendLimits.Enterprise, nil
	case gen.DyWorkspacePlan_PRO:
		return c.sendLimits.Pro, nil
	default:
		return c.sendLimits.Free, nil
	}
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

func customDomainLimitForPlan(plan gen.DyWorkspacePlan) int64 {
	switch plan {
	case gen.DyWorkspacePlan_ENTERPRISE:
		return 3
	case gen.DyWorkspacePlan_PRO:
		return 1
	default:
		return 0
	}
}

func (c *Client) Close() error { return c.conn.Close() }
