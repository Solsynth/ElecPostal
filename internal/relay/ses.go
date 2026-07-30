package relay

import (
	"context"
	"fmt"
	"net/mail"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"

	"src.solsynth.dev/sosys/elecpostal/internal/logging"
)

// SESConfig controls the AWS SES API v2 adapter. Authentication is supplied by
// the AWS SDK default credential chain (environment, shared config, ECS, or an
// EC2 role), not SMTP credentials.
type SESConfig struct {
	Region string
}

type sesClient interface {
	SendEmail(context.Context, *sesv2.SendEmailInput, ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error)
	CreateEmailIdentity(context.Context, *sesv2.CreateEmailIdentityInput, ...func(*sesv2.Options)) (*sesv2.CreateEmailIdentityOutput, error)
	GetEmailIdentity(context.Context, *sesv2.GetEmailIdentityInput, ...func(*sesv2.Options)) (*sesv2.GetEmailIdentityOutput, error)
	DeleteEmailIdentity(context.Context, *sesv2.DeleteEmailIdentityInput, ...func(*sesv2.Options)) (*sesv2.DeleteEmailIdentityOutput, error)
}

// SESAdapter sends outbound mail with the AWS SES API v2.
type SESAdapter struct {
	client sesClient
}

func NewSESAdapter(ctx context.Context, cfg SESConfig) (*SESAdapter, error) {
	if strings.TrimSpace(cfg.Region) == "" {
		return nil, fmt.Errorf("AWS SES region is required")
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration: %w", err)
	}
	return &SESAdapter{client: sesv2.NewFromConfig(awsCfg)}, nil
}

func (a *SESAdapter) Send(ctx context.Context, message Message) (DeliveryResult, error) {
	if len(message.AttachmentIDs) > 0 {
		return DeliveryResult{}, ErrAttachmentSourceRequired
	}
	recipients, err := envelopeRecipients(message)
	if err != nil {
		return DeliveryResult{}, err
	}
	if len(recipients) > 50 {
		return DeliveryResult{}, fmt.Errorf("SES accepts at most 50 recipients per message")
	}
	from, err := mail.ParseAddress(message.FromAddress)
	if err != nil || from.Address == "" {
		return DeliveryResult{}, fmt.Errorf("invalid sender address %q", message.FromAddress)
	}
	from.Name = message.FromName

	output, err := a.client.SendEmail(ctx, &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(from.String()),
		Destination: &types.Destination{
			ToAddresses:  message.To,
			CcAddresses:  message.Cc,
			BccAddresses: message.Bcc,
		},
		Content: &types.EmailContent{Simple: sesMessage(message)},
	})
	if err != nil {
		return DeliveryResult{}, fmt.Errorf("send email with SES: %w", err)
	}
	logging.Log.Debug().Str("provider_message_id", aws.ToString(output.MessageId)).Int("recipient_count", len(recipients)).Msg("SES accepted email")
	return DeliveryResult{ProviderMessageID: aws.ToString(output.MessageId)}, nil
}

func sesMessage(message Message) *types.Message {
	body := &types.Body{Text: &types.Content{Data: aws.String(message.Body), Charset: aws.String("UTF-8")}}
	if message.ContentType == "text/html" {
		body = &types.Body{Html: &types.Content{Data: aws.String(message.Body), Charset: aws.String("UTF-8")}}
	}
	return &types.Message{Subject: &types.Content{Data: aws.String(message.Subject), Charset: aws.String("UTF-8")}, Body: body}
}

func (a *SESAdapter) Close() error { return nil }

// CreateIdentity starts SES verification. Domains use Easy DKIM and return the
// three CNAME records that must be published; email-address identities receive
// an SES verification email instead.
func (a *SESAdapter) CreateIdentity(ctx context.Context, identity string) (IdentityStatus, error) {
	output, err := a.client.CreateEmailIdentity(ctx, &sesv2.CreateEmailIdentityInput{EmailIdentity: aws.String(identity)})
	if err != nil {
		return IdentityStatus{}, fmt.Errorf("create SES email identity: %w", err)
	}
	verificationStatus := "PENDING"
	if output.VerifiedForSendingStatus {
		verificationStatus = "SUCCESS"
	}
	return sesIdentityStatus(identity, string(output.IdentityType), verificationStatus, output.VerifiedForSendingStatus, output.DkimAttributes), nil
}

// GetIdentity refreshes identity verification and Easy DKIM state from SES.
func (a *SESAdapter) GetIdentity(ctx context.Context, identity string) (IdentityStatus, error) {
	output, err := a.client.GetEmailIdentity(ctx, &sesv2.GetEmailIdentityInput{EmailIdentity: aws.String(identity)})
	if err != nil {
		return IdentityStatus{}, fmt.Errorf("get SES email identity: %w", err)
	}
	return sesIdentityStatus(identity, string(output.IdentityType), string(output.VerificationStatus), output.VerifiedForSendingStatus, output.DkimAttributes), nil
}

// DeleteIdentity removes an SES identity. Callers must first ensure that no
// other workspace still depends on it.
func (a *SESAdapter) DeleteIdentity(ctx context.Context, identity string) error {
	if _, err := a.client.DeleteEmailIdentity(ctx, &sesv2.DeleteEmailIdentityInput{EmailIdentity: aws.String(identity)}); err != nil {
		return fmt.Errorf("delete SES email identity: %w", err)
	}
	return nil
}

func sesIdentityStatus(identity, identityType, verificationStatus string, verified bool, dkim *types.DkimAttributes) IdentityStatus {
	status := IdentityStatus{Identity: identity, IdentityType: identityType, VerificationStatus: verificationStatus, VerifiedForSendingStatus: verified}
	if dkim == nil {
		return status
	}
	status.DKIMStatus = string(dkim.Status)
	for _, token := range dkim.Tokens {
		status.DNSRecords = append(status.DNSRecords, DNSRecord{
			Name:  token + "._domainkey." + identity,
			Type:  "CNAME",
			Value: token + ".dkim.amazonses.com",
		})
	}
	return status
}
