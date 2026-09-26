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

	"src.solsynth.dev/sosys/elecpostal/internal/localization"
	"src.solsynth.dev/sosys/elecpostal/internal/logging"
	"src.solsynth.dev/sosys/elecpostal/internal/mailintel"
	gen "src.solsynth.dev/sosys/go/proto"
)

// AppID is the SolWatt product tenant. Ring routes the push to the
// dev.solsynth.solarwatt app and WebSocket namespace; notifications without an
// app id fall back to the Solian (Island) tenant.
const AppID = "dev.solsynth.solarwatt"

// LanguageResolver resolves the notification language of an account.
type LanguageResolver interface {
	Language(context.Context, string) (string, error)
	Close() error
}

// EmailNotification is an incoming email as it reaches the notification layer.
type EmailNotification struct {
	AccountID string
	EmailID   string
	Subject   string
	FromName  string
	Body      string
	// ContentType distinguishes HTML bodies, which are reduced to text before
	// the important part of the message is picked out.
	ContentType string
}

// Client sends account notifications through DyRingService.
type Client struct {
	conn     *grpc.ClientConn
	client   gen.DyRingServiceClient
	language LanguageResolver
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

// SetLanguageResolver enables per-recipient notification localization. Without
// a resolver (or when a lookup fails) notifications are sent in English.
func (c *Client) SetLanguageResolver(resolver LanguageResolver) { c.language = resolver }

// SendEmailNotification pushes a localized incoming-mail notification to the
// mailbox owner's SolWatt clients. The subtitle carries whatever the message
// most wants from its recipient (verification code, security event, or action
// request), falling back to the subject.
func (c *Client) SendEmailNotification(ctx context.Context, notification EmailNotification) error {
	language := c.resolveLanguage(ctx, notification.AccountID)
	fromName := strings.TrimSpace(notification.FromName)
	if fromName == "" {
		fromName = localization.Localize(language, "newEmailUnknownSender", nil)
	}
	highlight := mailintel.Analyze(notification.Subject, notification.Body, notification.ContentType)
	subtitle := highlight.Text
	if subtitle == "" {
		subtitle = strings.TrimSpace(notification.Subject)
		if subtitle == "" {
			subtitle = localization.Localize(language, "newEmailNoSubject", nil)
		}
	}
	meta := map[string]string{"email_id": notification.EmailID}
	if highlight.Kind != mailintel.KindNone {
		meta["kind"] = string(highlight.Kind)
	}
	if highlight.Code != "" {
		meta["code"] = highlight.Code
	}
	payload, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	appID := AppID
	_, err = c.client.SendPushNotificationToUser(ctx, &gen.DySendPushNotificationToUserRequest{
		UserId: notification.AccountID,
		Notification: &gen.DyPushNotification{
			Topic:    "email",
			Title:    localization.Localize(language, titleKey(highlight.Kind), nil),
			Subtitle: subtitle,
			Body:     localization.Localize(language, "newEmailFromBody", map[string]string{"sender": fromName}),
			Meta:     payload,
			AppId:    &appID,
		},
	})
	return err
}

// titleKey maps a highlight to the notification title that labels it.
func titleKey(kind mailintel.Kind) string {
	switch kind {
	case mailintel.KindCode:
		return "verificationCodeTitle"
	case mailintel.KindSecurity:
		return "securityAlertTitle"
	case mailintel.KindAction:
		return "actionRequiredTitle"
	default:
		return "newEmailTitle"
	}
}

// resolveLanguage returns the account language, degrading to English when the
// resolver is missing or the account lookup fails.
func (c *Client) resolveLanguage(ctx context.Context, accountID string) string {
	if c.language == nil {
		return ""
	}
	language, err := c.language.Language(ctx, accountID)
	if err != nil {
		logging.Log.Warn().Err(err).Str("account_id", accountID).Msg("failed to resolve notification language")
		return ""
	}
	return language
}

func (c *Client) Close() error {
	if c.language != nil {
		if err := c.language.Close(); err != nil {
			return err
		}
	}
	return c.conn.Close()
}
