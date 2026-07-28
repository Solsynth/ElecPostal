// Package realtime publishes account-scoped mail events through DysonNetwork's
// shared WebSocket gateway.
package realtime

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

const Namespace = "dev.solsynth.solarwatt"

type Publisher interface {
	Publish(context.Context, string, string, any) error
	Close() error
}

type Client struct {
	conn   *grpc.ClientConn
	client gen.WebSocketServiceClient
}

func NewClient(target string, useTLS, tlsSkipVerify bool) (*Client, error) {
	if strings.TrimSpace(target) == "" {
		return nil, fmt.Errorf("websocket target is required")
	}
	var transport credentials.TransportCredentials
	if useTLS {
		transport = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: tlsSkipVerify})
	} else {
		transport = insecure.NewCredentials()
	} // #nosec G402 -- internal deployment setting.
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(transport))
	if err != nil {
		return nil, fmt.Errorf("connect to websocket gateway: %w", err)
	}
	return &Client{conn: conn, client: gen.NewWebSocketServiceClient(conn)}, nil
}

func (c *Client) Publish(ctx context.Context, accountID, eventType string, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = c.client.PushWebSocketPacket(ctx, &gen.DyPushWebSocketPacketRequest{UserId: accountID, Namespace: Namespace, Packet: &gen.DyWebSocketPacket{Type: eventType, Data: payload}})
	return err
}

func (c *Client) Close() error { return c.conn.Close() }
