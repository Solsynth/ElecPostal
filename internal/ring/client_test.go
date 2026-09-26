package ring

import (
	"context"
	"encoding/json"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"

	"src.solsynth.dev/sosys/elecpostal/internal/mailintel"
	gen "src.solsynth.dev/sosys/go/proto"
)

type fakeRingService struct {
	gen.UnimplementedDyRingServiceServer
	notifications []*gen.DyPushNotification
}

func (f *fakeRingService) SendPushNotificationToUser(_ context.Context, req *gen.DySendPushNotificationToUserRequest) (*emptypb.Empty, error) {
	f.notifications = append(f.notifications, req.GetNotification())
	return &emptypb.Empty{}, nil
}

// send pushes one notification and returns what reached Ring.
func send(t *testing.T, notification EmailNotification) *gen.DyPushNotification {
	t.Helper()
	service := &fakeRingService{}
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	gen.RegisterDyRingServiceServer(server, service)
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

	client := &Client{conn: conn, client: gen.NewDyRingServiceClient(conn)}
	if err := client.SendEmailNotification(context.Background(), notification); err != nil {
		t.Fatalf("SendEmailNotification() error = %v", err)
	}
	if len(service.notifications) != 1 {
		t.Fatalf("notifications sent = %d, want 1", len(service.notifications))
	}
	return service.notifications[0]
}

func metaOf(t *testing.T, notification *gen.DyPushNotification) map[string]string {
	t.Helper()
	meta := map[string]string{}
	if err := json.Unmarshal(notification.GetMeta(), &meta); err != nil {
		t.Fatalf("meta %s is not a JSON object: %v", notification.GetMeta(), err)
	}
	return meta
}

func TestSendEmailNotificationTargetsSolWattApp(t *testing.T) {
	notification := send(t, EmailNotification{
		AccountID: "account-1", EmailID: "email-1", Language: "en",
		Subject: "Lunch?", FromName: "Ada Lovelace",
	})
	if got := notification.GetAppId(); got != AppID {
		t.Fatalf("app_id = %q, want %q", got, AppID)
	}
	if AppID != "dev.solsynth.solarwatt" {
		t.Fatalf("AppID = %q, want dev.solsynth.solarwatt", AppID)
	}
	if got := notification.GetTitle(); got != "New email" {
		t.Fatalf("title = %q, want New email", got)
	}
	if got := notification.GetSubtitle(); got != "Lunch?" {
		t.Fatalf("subtitle = %q, want Lunch?", got)
	}
	if got := notification.GetBody(); got != "From Ada Lovelace" {
		t.Fatalf("body = %q, want From Ada Lovelace", got)
	}
	if got := metaOf(t, notification); got["email_id"] != "email-1" || len(got) != 1 {
		t.Fatalf("meta = %v, want only the email id", got)
	}
}

func TestSendEmailNotificationRendersHighlights(t *testing.T) {
	notification := send(t, EmailNotification{
		AccountID: "account-1", EmailID: "email-1", Language: "zh-CN",
		Subject: "GitHub 登录验证", FromName: "GitHub",
		Highlight: mailintel.Highlight{Kind: mailintel.KindCode, Text: "482913", Code: "482913"},
	})
	if got := notification.GetTitle(); got != "验证码" {
		t.Fatalf("title = %q, want 验证码", got)
	}
	if got := notification.GetSubtitle(); got != "482913" {
		t.Fatalf("subtitle = %q, want the extracted code", got)
	}
	if got := notification.GetBody(); got != "来自 GitHub" {
		t.Fatalf("body = %q, want 来自 GitHub", got)
	}
	meta := metaOf(t, notification)
	if meta["kind"] != "code" || meta["code"] != "482913" {
		t.Fatalf("meta = %v, want the code and its kind", meta)
	}
}

func TestSendEmailNotificationRendersSummaries(t *testing.T) {
	notification := send(t, EmailNotification{
		AccountID: "account-1", EmailID: "email-1", Language: "en",
		Subject: "This week at Acme", FromName: "Acme",
		Summary: "Acme shipped three new features and a price change",
	})
	if got := notification.GetTitle(); got != "New email" {
		t.Fatalf("title = %q, want New email", got)
	}
	if got := notification.GetSubtitle(); got != "Acme shipped three new features and a price change" {
		t.Fatalf("subtitle = %q, want the summary", got)
	}
	if got := metaOf(t, notification)["source"]; got != "summary" {
		t.Fatalf("meta source = %q, want summary", got)
	}
}

func TestSendEmailNotificationPrefersHighlightOverSummary(t *testing.T) {
	notification := send(t, EmailNotification{
		AccountID: "account-1", EmailID: "email-1", Language: "en",
		Subject:   "Sign in",
		Highlight: mailintel.Highlight{Kind: mailintel.KindSecurity, Text: "Your password was changed"},
		Summary:   "Acme sent a security notice",
	})
	if got := notification.GetSubtitle(); got != "Your password was changed" {
		t.Fatalf("subtitle = %q, want the highlight", got)
	}
	meta := metaOf(t, notification)
	if meta["kind"] != "security" {
		t.Fatalf("meta = %v, want the security kind", meta)
	}
	if _, ok := meta["source"]; ok {
		t.Fatalf("meta = %v, want no summary source", meta)
	}
}

func TestSendEmailNotificationFallsBackToEnglishPlaceholders(t *testing.T) {
	notification := send(t, EmailNotification{AccountID: "account-1", EmailID: "email-1", Language: "fr-FR"})
	if got := notification.GetTitle(); got != "New email" {
		t.Fatalf("title = %q, want New email", got)
	}
	if got := notification.GetSubtitle(); got != "(No subject)" {
		t.Fatalf("subtitle = %q, want (No subject)", got)
	}
	if got := notification.GetBody(); got != "From New sender" {
		t.Fatalf("body = %q, want From New sender", got)
	}
}

func TestSendEmailNotificationRejectsEmptyTarget(t *testing.T) {
	if _, err := NewClient("  ", false, false); err == nil {
		t.Fatal("NewClient() error = nil, want an error for an empty target")
	}
}
