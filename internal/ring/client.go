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
	"src.solsynth.dev/sosys/elecpostal/internal/mailintel"
	gen "src.solsynth.dev/sosys/go/proto"
)

// AppID is the SolWatt product tenant. Ring routes the push to the
// dev.solsynth.solarwatt app and WebSocket namespace; notifications without an
// app id fall back to the Solian (Island) tenant.
const AppID = "dev.solsynth.solarwatt"

// EmailNotification is an incoming email as it reaches the notification layer.
// The caller decides the content: Highlight and Summary are already resolved
// against the recipient's preferences.
type EmailNotification struct {
	AccountID string
	EmailID   string
	// Language is the recipient's language tag; an empty value renders English.
	Language string
	// Subject is the fallback subtitle when nothing was extracted.
	Subject  string
	FromName string
	// Highlight is the message's code, security event, or action request,
	// empty when nothing stood out or the account disabled highlighting.
	Highlight mailintel.Highlight
	// Summary is a personality-service summary, used only when Highlight is
	// empty.
	Summary string
}

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

// SendEmailNotification pushes a localized incoming-mail notification to the
// mailbox owner's SolWatt clients. The subtitle carries whatever the message
// most wants from its recipient: an extracted highlight, a summary, or the
// subject.
func (c *Client) SendEmailNotification(ctx context.Context, notification EmailNotification) error {
	language := notification.Language
	fromName := strings.TrimSpace(notification.FromName)
	if fromName == "" {
		fromName = localization.Localize(language, "newEmailUnknownSender", nil)
	}
	subtitle := notification.Highlight.Text
	source := ""
	switch {
	case subtitle != "":
	case notification.Summary != "":
		subtitle, source = notification.Summary, "summary"
	default:
		subtitle = strings.TrimSpace(notification.Subject)
		if subtitle == "" {
			subtitle = localization.Localize(language, "newEmailNoSubject", nil)
		}
	}
	meta := map[string]string{"email_id": notification.EmailID}
	if notification.Highlight.Kind != mailintel.KindNone {
		meta["kind"] = string(notification.Highlight.Kind)
	}
	if notification.Highlight.Code != "" {
		meta["code"] = notification.Highlight.Code
	}
	if source != "" {
		meta["source"] = source
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
			Title:    localization.Localize(language, titleKey(notification.Highlight.Kind), nil),
			Subtitle: subtitle,
			Body:     localization.Localize(language, "newEmailFromBody", map[string]string{"sender": fromName}),
			Meta:     payload,
			AppId:    &appID,
			// Ring records a notification for the account's history only when
			// the sender marks it savable, and an email that is not in the
			// history is a message the reader never learns about.
			IsSavable: true,
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

func (c *Client) Close() error { return c.conn.Close() }
