// Package account provides the small account surface ElecPostal needs from
// Stargate: the recipient's preferred language for localized notifications.
package account

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	gen "src.solsynth.dev/sosys/go/proto"
)

// languageLookupTimeout bounds the account lookup performed while delivering
// an incoming-mail notification so a slow account service cannot stall the
// mail pipeline.
const languageLookupTimeout = 3 * time.Second

// Client reads accounts through Stargate's DyAccountService.
type Client struct {
	conn   *grpc.ClientConn
	client gen.DyAccountServiceClient
}

func NewClient(target string, useTLS, tlsSkipVerify bool) (*Client, error) {
	if strings.TrimSpace(target) == "" {
		return nil, fmt.Errorf("account target is required")
	}
	var transport credentials.TransportCredentials
	if useTLS {
		transport = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: tlsSkipVerify}) // #nosec G402 -- explicitly configured for internal deployments.
	} else {
		transport = insecure.NewCredentials()
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(transport))
	if err != nil {
		return nil, fmt.Errorf("connect to account service: %w", err)
	}
	return &Client{conn: conn, client: gen.NewDyAccountServiceClient(conn)}, nil
}

// Language returns the account's preferred notification language. The raw
// language tag is returned as stored; callers pass it to localization.Localize,
// which normalizes it and falls back to English.
func (c *Client) Language(ctx context.Context, accountID string) (string, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return "", fmt.Errorf("account id is required")
	}
	ctx, cancel := context.WithTimeout(ctx, languageLookupTimeout)
	defer cancel()
	account, err := c.client.GetAccount(ctx, &gen.DyGetAccountRequest{Id: accountID})
	if err != nil {
		return "", fmt.Errorf("get account %s: %w", accountID, err)
	}
	return account.GetLanguage(), nil
}

func (c *Client) Close() error { return c.conn.Close() }
