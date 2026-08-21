package app

import (
	"context"
	"io"

	"src.solsynth.dev/sosys/elecpostal/internal/filesystem"
	"src.solsynth.dev/sosys/elecpostal/internal/relay"
)

type appAttachmentSource struct{ client *filesystem.Client }

func (source appAttachmentSource) Open(ctx context.Context, id string) (io.ReadCloser, relay.AttachmentMetadata, error) {
	if source.client == nil {
		return nil, relay.AttachmentMetadata{}, relay.ErrAttachmentSourceRequired
	}
	reader, err := source.client.OpenAttachment(ctx, id)
	if err != nil {
		return nil, relay.AttachmentMetadata{}, err
	}
	return reader.Content, relay.AttachmentMetadata{
		ID: reader.File.ID, Filename: reader.File.Name, MimeType: reader.File.MimeType, Size: reader.File.Size,
	}, nil
}
