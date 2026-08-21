// Package filesystem provides the small FileSystem client surface needed by
// ElecPostal. Keeping it here avoids leaking generated gRPC details into email
// handlers and makes the upload path straightforward to test.
package filesystem

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"src.solsynth.dev/sosys/elecpostal/internal/database"
	gen "src.solsynth.dev/sosys/go/proto"
)

const uploadBufferSize = 64 * 1024

// AttachmentUpload describes content that is to be stored for an email.
type AttachmentUpload struct {
	AccountID   uuid.UUID
	WorkspaceID string
	Filename    string
	MimeType    string
	Size        int64
	Content     io.Reader
}

// AttachmentReader exposes a fresh DysonFS metadata snapshot and a streaming
// reader for its content. Callers must close Content.
type AttachmentReader struct {
	File    database.CloudFileReferenceObject
	Content io.ReadCloser
}

// ByteStore stores and streams attachment content.
type ByteStore interface {
	UploadAttachment(context.Context, AttachmentUpload) (database.CloudFileReferenceObject, error)
	OpenAttachment(context.Context, string) (AttachmentReader, error)
	DeleteAttachment(context.Context, string) error
	Close() error
}

type Client struct {
	conn   *grpc.ClientConn
	client gen.DyFileServiceClient
}

func NewClient(target string, useTLS, tlsSkipVerify bool) (*Client, error) {
	if strings.TrimSpace(target) == "" {
		return nil, fmt.Errorf("filesystem target is required")
	}
	var transport credentials.TransportCredentials
	if useTLS {
		transport = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: tlsSkipVerify}) // #nosec G402 -- explicitly configured for internal deployments.
	} else {
		transport = insecure.NewCredentials()
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(transport))
	if err != nil {
		return nil, fmt.Errorf("connect to filesystem: %w", err)
	}
	return &Client{conn: conn, client: gen.NewDyFileServiceClient(conn)}, nil
}

func (c *Client) UploadAttachment(ctx context.Context, upload AttachmentUpload) (database.CloudFileReferenceObject, error) {
	if upload.Content == nil {
		return database.CloudFileReferenceObject{}, fmt.Errorf("attachment content is required")
	}
	if upload.Size < 0 {
		return database.CloudFileReferenceObject{}, fmt.Errorf("attachment size cannot be negative")
	}
	if strings.TrimSpace(upload.Filename) == "" {
		return database.CloudFileReferenceObject{}, fmt.Errorf("attachment filename is required")
	}

	stream, err := c.client.UploadFile(ctx)
	if err != nil {
		return database.CloudFileReferenceObject{}, fmt.Errorf("start attachment upload: %w", err)
	}
	options := &gen.DyFileUploadOptions{
		AccountId:   upload.AccountID.String(),
		FileName:    upload.Filename,
		FileSize:    upload.Size,
		ContentType: upload.MimeType,
		Index:       false,
		Usage:       stringPointer("email_attachment"),
	}
	if workspaceID := strings.TrimSpace(upload.WorkspaceID); workspaceID != "" {
		options.WorkspaceId = &workspaceID
	}
	if err := stream.Send(&gen.DyUploadFileRequest{Payload: &gen.DyUploadFileRequest_Options{Options: options}}); err != nil {
		return database.CloudFileReferenceObject{}, fmt.Errorf("send attachment options: %w", err)
	}

	buffer := make([]byte, uploadBufferSize)
	for {
		count, readErr := upload.Content.Read(buffer)
		if count > 0 {
			if err := stream.Send(&gen.DyUploadFileRequest{Payload: &gen.DyUploadFileRequest_Data{Data: buffer[:count]}}); err != nil {
				return database.CloudFileReferenceObject{}, fmt.Errorf("send attachment content: %w", err)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return database.CloudFileReferenceObject{}, fmt.Errorf("read attachment content: %w", readErr)
		}
	}
	file, err := stream.CloseAndRecv()
	if err != nil {
		return database.CloudFileReferenceObject{}, fmt.Errorf("complete attachment upload: %w", err)
	}
	if file.GetId() == "" {
		return database.CloudFileReferenceObject{}, fmt.Errorf("filesystem returned an attachment without an ID")
	}
	return database.CloudFileReferenceObject{
		ID:             file.GetId(),
		Name:           upload.Filename,
		FileMeta:       map[string]any{},
		UserMeta:       map[string]any{},
		SensitiveMarks: []int{},
		MimeType:       upload.MimeType,
		Size:           upload.Size,
	}, nil
}

func (c *Client) OpenAttachment(ctx context.Context, fileID string) (AttachmentReader, error) {
	if strings.TrimSpace(fileID) == "" {
		return AttachmentReader{}, fmt.Errorf("attachment file ID is required")
	}
	file, err := c.client.GetFile(ctx, &gen.DyGetFileRequest{Id: fileID})
	if err != nil {
		return AttachmentReader{}, fmt.Errorf("get attachment metadata: %w", err)
	}
	if file.GetIsFolder() {
		return AttachmentReader{}, fmt.Errorf("attachment file ID refers to a folder")
	}
	if file.GetId() == "" {
		return AttachmentReader{}, fmt.Errorf("filesystem returned an attachment without an ID")
	}
	streamCtx, cancel := context.WithCancel(ctx)
	stream, err := c.client.DownloadFile(streamCtx, &gen.DyDownloadFileRequest{Id: file.GetId()})
	if err != nil {
		cancel()
		return AttachmentReader{}, fmt.Errorf("start attachment download: %w", err)
	}
	return AttachmentReader{
		File:    cloudFileReference(file),
		Content: &downloadReader{stream: stream, cancel: cancel},
	}, nil
}

func (c *Client) DeleteAttachment(ctx context.Context, fileID string) error {
	if strings.TrimSpace(fileID) == "" {
		return fmt.Errorf("attachment file ID is required")
	}
	if _, err := c.client.DeleteFile(ctx, &gen.DyDeleteFileRequest{Id: fileID, Purge: true}); err != nil {
		return fmt.Errorf("delete attachment: %w", err)
	}
	return nil
}

type downloadReader struct {
	stream gen.DyFileService_DownloadFileClient
	cancel context.CancelFunc
	buffer []byte
	closed bool
}

func (r *downloadReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, io.ErrClosedPipe
	}
	for len(r.buffer) == 0 {
		chunk, err := r.stream.Recv()
		if err != nil {
			return 0, err
		}
		r.buffer = chunk.GetData()
	}
	count := copy(p, r.buffer)
	r.buffer = r.buffer[count:]
	return count, nil
}

func (r *downloadReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	r.cancel()
	return nil
}

func cloudFileReference(file *gen.DyCloudFile) database.CloudFileReferenceObject {
	reference := database.CloudFileReferenceObject{
		ID:              file.GetId(),
		Name:            file.GetName(),
		MimeType:        file.GetMimeType(),
		Hash:            file.GetHash(),
		Size:            file.GetSize(),
		URL:             file.GetUrl(),
		Usage:           file.GetUsage(),
		ApplicationType: file.GetApplicationType(),
		FileMeta:        map[string]any{},
		UserMeta:        map[string]any{},
		SensitiveMarks:  []int{},
	}
	_ = json.Unmarshal(file.GetFileMeta(), &reference.FileMeta)
	_ = json.Unmarshal(file.GetUserMeta(), &reference.UserMeta)
	if reference.MimeType == "" {
		reference.MimeType = file.GetContentType()
	}
	return reference
}

func (c *Client) Close() error { return c.conn.Close() }

func stringPointer(value string) *string { return &value }
