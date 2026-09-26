package account

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	gen "src.solsynth.dev/sosys/go/proto"
)

type fakeAccountService struct {
	gen.UnimplementedDyAccountServiceServer
	accounts map[string]*gen.DyAccount
}

func (f *fakeAccountService) GetAccount(_ context.Context, req *gen.DyGetAccountRequest) (*gen.DyAccount, error) {
	account, ok := f.accounts[req.GetId()]
	if !ok {
		return nil, status.Error(codes.NotFound, "account not found")
	}
	return account, nil
}

func newTestClient(t *testing.T, service gen.DyAccountServiceServer) *Client {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	gen.RegisterDyAccountServiceServer(server, service)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &Client{conn: conn, client: gen.NewDyAccountServiceClient(conn)}
}

func TestLanguageReturnsAccountLanguage(t *testing.T) {
	client := newTestClient(t, &fakeAccountService{accounts: map[string]*gen.DyAccount{
		"account-1": {Id: "account-1", Language: "zh-CN"},
	}})

	language, err := client.Language(context.Background(), " account-1 ")
	if err != nil {
		t.Fatalf("Language() error = %v", err)
	}
	if language != "zh-CN" {
		t.Fatalf("Language() = %q, want zh-CN", language)
	}
}

func TestLanguageRequiresAccountID(t *testing.T) {
	client := newTestClient(t, &fakeAccountService{})

	if _, err := client.Language(context.Background(), "  "); err == nil {
		t.Fatal("Language() error = nil, want an error for an empty account id")
	}
}

func TestLanguageReportsLookupFailure(t *testing.T) {
	client := newTestClient(t, &fakeAccountService{})

	_, err := client.Language(context.Background(), "missing")
	if err == nil {
		t.Fatal("Language() error = nil, want an error for an unknown account")
	}
	if got := status.Code(err); got != codes.NotFound {
		t.Fatalf("status.Code(error) = %v, want NotFound", got)
	}
}
