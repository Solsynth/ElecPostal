package ring

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"

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

type fakeLanguageResolver struct {
	language string
	err      error
}

func (f *fakeLanguageResolver) Language(context.Context, string) (string, error) {
	return f.language, f.err
}

func (f *fakeLanguageResolver) Close() error { return nil }

func newTestClient(t *testing.T, service *fakeRingService) *Client {
	t.Helper()
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
	return &Client{conn: conn, client: gen.NewDyRingServiceClient(conn)}
}

func TestSendEmailNotificationTargetsSolWattApp(t *testing.T) {
	service := &fakeRingService{}
	client := newTestClient(t, service)
	client.SetLanguageResolver(&fakeLanguageResolver{language: "en"})

	err := client.SendEmailNotification(context.Background(), EmailNotification{
		AccountID: "account-1", EmailID: "email-1", Subject: "Lunch?", FromName: "Ada Lovelace",
		Body: "see you at noon", ContentType: "text/plain",
	})
	if err != nil {
		t.Fatalf("SendEmailNotification() error = %v", err)
	}
	if len(service.notifications) != 1 {
		t.Fatalf("notifications sent = %d, want 1", len(service.notifications))
	}
	notification := service.notifications[0]
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

func TestSendEmailNotificationSurfacesVerificationCodes(t *testing.T) {
	service := &fakeRingService{}
	client := newTestClient(t, service)
	client.SetLanguageResolver(&fakeLanguageResolver{language: "zh-CN"})

	err := client.SendEmailNotification(context.Background(), EmailNotification{
		AccountID: "account-1", EmailID: "email-1", Subject: "登录验证", FromName: "Acme",
		Body:        "您好，\n您的验证码为 638415，请在 10 分钟内输入。\n如果这不是您本人操作，请忽略此邮件。",
		ContentType: "text/plain",
	})
	if err != nil {
		t.Fatalf("SendEmailNotification() error = %v", err)
	}
	notification := service.notifications[0]
	if got := notification.GetTitle(); got != "验证码" {
		t.Fatalf("title = %q, want 验证码", got)
	}
	if got := notification.GetSubtitle(); got != "638415" {
		t.Fatalf("subtitle = %q, want the code instead of the leading part", got)
	}
	if got := notification.GetBody(); got != "来自 Acme" {
		t.Fatalf("body = %q, want 来自 Acme", got)
	}
	meta := metaOf(t, notification)
	if meta["kind"] != "code" || meta["code"] != "638415" {
		t.Fatalf("meta = %v, want the code and its kind", meta)
	}
}

func TestSendEmailNotificationSurfacesTheImportantSentence(t *testing.T) {
	service := &fakeRingService{}
	client := newTestClient(t, service)
	client.SetLanguageResolver(&fakeLanguageResolver{language: "en"})

	err := client.SendEmailNotification(context.Background(), EmailNotification{
		AccountID: "account-1", EmailID: "email-1", Subject: "Acme account notice", FromName: "Acme",
		Body:        "Hello Ada,\n\nThanks for using Acme. Your password was changed on 2026-09-26.\nIf you did not do this, contact support. Unsubscribe from these emails.",
		ContentType: "text/html",
	})
	if err != nil {
		t.Fatalf("SendEmailNotification() error = %v", err)
	}
	notification := service.notifications[0]
	if got := notification.GetTitle(); got != "Security alert" {
		t.Fatalf("title = %q, want Security alert", got)
	}
	if got := notification.GetSubtitle(); got != "Your password was changed on 2026-09-26" {
		t.Fatalf("subtitle = %q, want the security sentence", got)
	}
	if got := metaOf(t, notification)["kind"]; got != "security" {
		t.Fatalf("meta kind = %q, want security", got)
	}
}

func TestSendEmailNotificationLocalizesRecipientLanguage(t *testing.T) {
	service := &fakeRingService{}
	client := newTestClient(t, service)
	client.SetLanguageResolver(&fakeLanguageResolver{language: "zh-CN"})

	err := client.SendEmailNotification(context.Background(), EmailNotification{
		AccountID: "account-1", EmailID: "email-1", Subject: "午餐？", FromName: "艾达",
		Body: "中午见", ContentType: "text/plain",
	})
	if err != nil {
		t.Fatalf("SendEmailNotification() error = %v", err)
	}
	notification := service.notifications[0]
	if got := notification.GetTitle(); got != "新邮件" {
		t.Fatalf("title = %q, want 新邮件", got)
	}
	if got := notification.GetBody(); got != "来自 艾达" {
		t.Fatalf("body = %q, want 来自 艾达", got)
	}
}

func TestSendEmailNotificationLocalizesEmptyFields(t *testing.T) {
	service := &fakeRingService{}
	client := newTestClient(t, service)
	client.SetLanguageResolver(&fakeLanguageResolver{language: "zh-hans"})

	err := client.SendEmailNotification(context.Background(), EmailNotification{
		AccountID: "account-1", EmailID: "email-1", Subject: "  ", Body: "  ", ContentType: "text/plain",
	})
	if err != nil {
		t.Fatalf("SendEmailNotification() error = %v", err)
	}
	notification := service.notifications[0]
	if got := notification.GetSubtitle(); got != "（无主题）" {
		t.Fatalf("subtitle = %q, want （无主题）", got)
	}
	if got := notification.GetBody(); got != "来自 新发件人" {
		t.Fatalf("body = %q, want 来自 新发件人", got)
	}
}

func TestSendEmailNotificationFallsBackToEnglish(t *testing.T) {
	for name, resolver := range map[string]LanguageResolver{
		"no resolver":    nil,
		"lookup failure": &fakeLanguageResolver{err: errors.New("account service unavailable")},
		"unsupported":    &fakeLanguageResolver{language: "fr-FR"},
	} {
		t.Run(name, func(t *testing.T) {
			service := &fakeRingService{}
			client := newTestClient(t, service)
			if resolver != nil {
				client.SetLanguageResolver(resolver)
			}

			err := client.SendEmailNotification(context.Background(), EmailNotification{
				AccountID: "account-1", EmailID: "email-1", ContentType: "text/plain",
			})
			if err != nil {
				t.Fatalf("SendEmailNotification() error = %v", err)
			}
			notification := service.notifications[0]
			if got := notification.GetTitle(); got != "New email" {
				t.Fatalf("title = %q, want New email", got)
			}
			if got := notification.GetSubtitle(); got != "(No subject)" {
				t.Fatalf("subtitle = %q, want (No subject)", got)
			}
			if got := notification.GetBody(); got != "From New sender" {
				t.Fatalf("body = %q, want From New sender", got)
			}
		})
	}
}

func metaOf(t *testing.T, notification *gen.DyPushNotification) map[string]string {
	t.Helper()
	meta := map[string]string{}
	if err := json.Unmarshal(notification.GetMeta(), &meta); err != nil {
		t.Fatalf("meta %s is not a JSON object: %v", notification.GetMeta(), err)
	}
	return meta
}
