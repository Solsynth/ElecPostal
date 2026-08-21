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
	"google.golang.org/protobuf/types/known/emptypb"
	gen "src.solsynth.dev/sosys/go/proto"
)

type recordingFileService struct {
	gen.UnimplementedDyFileServiceServer
	options   *gen.DyFileUploadOptions
	content   []byte
	downloads int
	deleted   string
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

func (s *recordingFileService) GetFile(_ context.Context, req *gen.DyGetFileRequest) (*gen.DyCloudFile, error) {
	return &gen.DyCloudFile{Id: req.GetId(), Name: "image.png", MimeType: "image/png", ContentType: "image/png", Size: 3}, nil
}

func (s *recordingFileService) DownloadFile(_ *gen.DyDownloadFileRequest, stream gen.DyFileService_DownloadFileServer) error {
	s.downloads++
	_ = stream.Send(&gen.DyDownloadFileChunk{Data: []byte{1, 2, 3}})
	return nil
}

func (s *recordingFileService) DeleteFile(_ context.Context, req *gen.DyDeleteFileRequest) (*emptypb.Empty, error) {
	s.deleted = req.GetId()
	return &emptypb.Empty{}, nil
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
	file, err := client.UploadAttachment(context.Background(), AttachmentUpload{
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
	if file.ID != "file-123" || file.Name != "invoice.pdf" {
		t.Fatalf("file = %#v, want file-123/invoice.pdf", file)
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

func TestOpenAndDeleteAttachment(t *testing.T) {
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	recorder := &recordingFileService{}
	gen.RegisterDyFileServiceServer(server, recorder)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///filesystem-test-download",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := &Client{conn: conn, client: gen.NewDyFileServiceClient(conn)}
	reader, err := client.OpenAttachment(context.Background(), "file-123")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader.Content)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Content.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, []byte{1, 2, 3}) || recorder.downloads != 1 || reader.File.Name != "image.png" {
		t.Fatalf("download = %v, calls=%d, file=%#v", data, recorder.downloads, reader.File)
	}
	if err := client.DeleteAttachment(context.Background(), "file-123"); err != nil {
		t.Fatal(err)
	}
	if recorder.deleted != "file-123" {
		t.Fatalf("deleted ID = %q", recorder.deleted)
	}
}
