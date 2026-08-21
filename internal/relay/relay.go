// Package relay defines outbound delivery adapters. EmailService depends on
// this small contract instead of any provider, so SES can be replaced without
// changing mailbox or message persistence logic.
package relay

import (
	"context"
	"fmt"
	"io"
)

// Message is the provider-neutral representation of an outgoing email.
// AttachmentIDs remain DysonFS references; an adapter resolves their bytes
// through its configured attachment source before delivery.
type Message struct {
	FromAddress   string
	FromName      string
	To            []string
	Cc            []string
	Bcc           []string
	Subject       string
	Body          string
	ContentType   string
	ThreadID      string
	AttachmentIDs []string
	Attachments   []AttachmentMetadata
}

// AttachmentMetadata is the immutable metadata required to reconstruct a MIME
// part around a DysonFS attachment.
type AttachmentMetadata struct {
	ID          string
	Filename    string
	MimeType    string
	Size        int64
	ContentID   string
	Disposition string
}

type AttachmentSource interface {
	Open(context.Context, string) (io.ReadCloser, AttachmentMetadata, error)
}

// DeliveryResult is provider metadata returned after an outbound provider has
// accepted a message. Acceptance does not guarantee final recipient delivery.
type DeliveryResult struct {
	ProviderMessageID string
}

// Adapter delivers an outbound message through a provider such as SES.
type Adapter interface {
	Send(context.Context, Message) (DeliveryResult, error)
	Close() error
}

// DisabledAdapter is used when no relay is configured. It preserves the
// existing persistence-only behaviour until an adapter is explicitly enabled.
type DisabledAdapter struct{}

func (DisabledAdapter) Send(context.Context, Message) (DeliveryResult, error) {
	return DeliveryResult{}, nil
}
func (DisabledAdapter) Close() error { return nil }

// ErrAttachmentSourceRequired prevents a provider adapter from silently
// dropping attachment IDs when it lacks a configured DysonFS byte source.
var ErrAttachmentSourceRequired = fmt.Errorf("relay attachment source is required")

// ErrUnsupportedAdapter is returned when a configured adapter has not been
// registered with the process.
var ErrUnsupportedAdapter = fmt.Errorf("unsupported mail relay adapter")
