package relay

import (
	"context"
	"fmt"
	"strings"

	"src.solsynth.dev/sosys/elecpostal/internal/mailmime"
)

func renderMessage(ctx context.Context, message Message, source AttachmentSource) ([]byte, error) {
	manifest := mailmime.Manifest{Version: mailmime.ManifestVersion, BodyType: message.ContentType}
	attachments := make(map[string]mailmime.Part, len(message.AttachmentIDs))
	supplied := make(map[string]AttachmentMetadata, len(message.Attachments))
	for _, metadata := range message.Attachments {
		supplied[metadata.ID] = metadata
	}
	for _, id := range message.AttachmentIDs {
		if strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("attachment file ID is empty")
		}
		if source == nil {
			return nil, ErrAttachmentSourceRequired
		}
		reader, metadata, err := source.Open(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("open relay attachment %q: %w", id, err)
		}
		if reader == nil {
			return nil, fmt.Errorf("relay attachment %q returned a nil reader", id)
		}
		_ = reader.Close()
		if provided, ok := supplied[id]; ok {
			if provided.Filename != "" {
				metadata.Filename = provided.Filename
			}
			if provided.MimeType != "" {
				metadata.MimeType = provided.MimeType
			}
			if provided.Size >= 0 {
				metadata.Size = provided.Size
			}
			if provided.ContentID != "" {
				metadata.ContentID = provided.ContentID
			}
			if provided.Disposition != "" {
				metadata.Disposition = provided.Disposition
			}
		}
		if metadata.Filename == "" || metadata.MimeType == "" || metadata.Size < 0 {
			return nil, fmt.Errorf("relay attachment %q has invalid metadata", id)
		}
		part := mailmime.Part{AttachmentID: id, Filename: metadata.Filename, MimeType: metadata.MimeType, Size: metadata.Size, ContentID: metadata.ContentID, Disposition: metadata.Disposition}
		manifest.Parts = append(manifest.Parts, part)
		attachments[id] = part
	}
	recipients := make([]mailmime.Recipient, 0, len(message.To)+len(message.Cc)+len(message.Bcc))
	for _, address := range message.To {
		recipients = append(recipients, mailmime.Recipient{Address: address, Kind: "to"})
	}
	for _, address := range message.Cc {
		recipients = append(recipients, mailmime.Recipient{Address: address, Kind: "cc"})
	}
	for _, address := range message.Bcc {
		recipients = append(recipients, mailmime.Recipient{Address: address, Kind: "bcc"})
	}
	return mailmime.RenderBytes(ctx, mailmime.MessageSource{
		FromAddress: message.FromAddress, FromName: message.FromName, Subject: message.Subject,
		Body: message.Body, BodyType: message.ContentType, Recipients: recipients,
		MessageID: message.MessageID, InReplyTo: message.InReplyTo, References: message.References,
		Manifest: manifest, Attachments: attachments, Source: relayAttachmentSource{source: source},
	})
}

type relayAttachmentSource struct{ source AttachmentSource }

func (source relayAttachmentSource) Open(ctx context.Context, id string) (mailmime.AttachmentReader, error) {
	reader, metadata, err := source.source.Open(ctx, id)
	if err != nil {
		return mailmime.AttachmentReader{}, err
	}
	return mailmime.AttachmentReader{Metadata: mailmime.AttachmentMetadata{ID: metadata.ID, Name: metadata.Filename, MimeType: metadata.MimeType, Size: metadata.Size}, Content: reader}, nil
}

func messageSourceOrNilWithContext(ctx context.Context, message Message, source AttachmentSource) ([]byte, error) {
	if len(message.AttachmentIDs) == 0 {
		return formatMessage(message), nil
	}
	return renderMessage(ctx, message, source)
}
