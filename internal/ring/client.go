// Package ring provides gRPC access to the shared notification service.
package ring

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	gen "src.solsynth.dev/sosys/go/proto"
)

// Client sends account notifications through DyRingService.
type Client struct {
	conn   *grpc.ClientConn
	client gen.DyRingServiceClient
}

func NewClient(target string, useTLS, tlsSkipVerify bool) (*Client, error) {
	if strings.TrimSpace(target) == "" {
		return nil, fmt.Errorf("ring target is required")
	}
	var transport credentials.TransportCredentials
	if useTLS {
		transport = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: tlsSkipVerify}) // #nosec G402 -- explicitly configured for internal deployments.
	} else {
		transport = insecure.NewCredentials()
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(transport))
	if err != nil {
		return nil, fmt.Errorf("connect to ring: %w", err)
	}
	return &Client{conn: conn, client: gen.NewDyRingServiceClient(conn)}, nil
}

func (c *Client) SendEmailNotification(ctx context.Context, accountID, emailID, subject, fromName string) error {
	if strings.TrimSpace(subject) == "" {
		subject = "(No subject)"
	}
	fromName = strings.TrimSpace(fromName)
	if fromName == "" {
		fromName = "New sender"
	}
	meta, err := json.Marshal(map[string]string{"email_id": emailID})
	if err != nil {
		return err
	}
	_, err = c.client.SendPushNotificationToUser(ctx, &gen.DySendPushNotificationToUserRequest{
		UserId: accountID,
		Notification: &gen.DyPushNotification{
			Topic:    "email",
			Title:    "New email",
			Subtitle: subject,
			Body:     "From " + fromName,
			Meta:     meta,
		},
	})
	return err
}

func (c *Client) Close() error { return c.conn.Close() }
