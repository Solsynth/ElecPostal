package filesystem

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	gen "src.solsynth.dev/sosys/go/proto"
)

type recordingFileService struct {
	gen.UnimplementedDyFileServiceServer
	options *gen.DyFileUploadOptions
	content []byte
}

func (s *recordingFileService) UploadFile(stream grpc.ClientStreamingServer[gen.DyUploadFileRequest, gen.DyCloudFile]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	s.options = first.GetOptions()
	for {
		request, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		s.content = append(s.content, request.GetData()...)
	}
	return stream.SendAndClose(&gen.DyCloudFile{Id: "file-123"})
}

func TestUploadAttachmentStreamsContentWithEmailUsage(t *testing.T) {
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	recorder := &recordingFileService{}
	gen.RegisterDyFileServiceServer(server, recorder)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient("passthrough:///filesystem-test",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	client := &Client{conn: conn, client: gen.NewDyFileServiceClient(conn)}
	accountID := uuid.New()
	fileID, err := client.UploadAttachment(context.Background(), AttachmentUpload{
		AccountID:   accountID,
		WorkspaceID: "workspace-1",
		Filename:    "invoice.pdf",
		MimeType:    "application/pdf",
		Size:        int64(len("attachment body")),
		Content:     bytes.NewBufferString("attachment body"),
	})
	if err != nil {
		t.Fatalf("UploadAttachment() error = %v", err)
	}
	if fileID != "file-123" {
		t.Fatalf("file ID = %q, want file-123", fileID)
	}
	if recorder.options.GetAccountId() != accountID.String() || recorder.options.GetWorkspaceId() != "workspace-1" {
		t.Fatalf("owner options = %+v", recorder.options)
	}
	if recorder.options.GetUsage() != "email_attachment" || recorder.options.GetFileName() != "invoice.pdf" {
		t.Fatalf("upload options = %+v", recorder.options)
	}
	if got := string(recorder.content); got != "attachment body" {
		t.Fatalf("content = %q", got)
	}
}
