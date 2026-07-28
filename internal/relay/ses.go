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
