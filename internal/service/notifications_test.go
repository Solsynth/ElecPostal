package service

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/logging"
	"src.solsynth.dev/sosys/elecpostal/internal/mailintel"
	"src.solsynth.dev/sosys/elecpostal/internal/personality"
	"src.solsynth.dev/sosys/elecpostal/internal/ring"
)

// fakeNotifier captures the payloads that would be pushed by Ring.
type fakeNotifier struct {
	sent []ring.EmailNotification
}

func (f *fakeNotifier) SendEmailNotification(_ context.Context, notification ring.EmailNotification) error {
	f.sent = append(f.sent, notification)
	return nil
}

func (f *fakeNotifier) Close() error { return nil }

// fakeSummarizer records what the personality service was asked to summarize.
type fakeSummarizer struct {
	requests []personality.SummaryRequest
	summary  string
	err      error
}

func (f *fakeSummarizer) Summarize(_ context.Context, request personality.SummaryRequest) (string, error) {
	f.requests = append(f.requests, request)
	if f.err != nil {
		return "", f.err
	}
	return f.summary, nil
}

func (f *fakeSummarizer) Close() error { return nil }

// fakeLanguageProvider answers with one language for every account.
type fakeLanguageProvider struct{ language string }

func (f fakeLanguageProvider) Language(context.Context, string) (string, error) {
	return f.language, nil
}

func (f fakeLanguageProvider) Close() error { return nil }

type notificationFixture struct {
	svc        *EmailService
	db         *gorm.DB
	notifier   *fakeNotifier
	summarizer *fakeSummarizer
	accountID  uuid.UUID
	mailbox    database.Mailbox
}

func newNotificationFixture(t *testing.T) notificationFixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+database.NewID()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open() error = %v", err)
	}
	if err := db.AutoMigrate(
		&database.Mailbox{}, &database.MailboxAlias{}, &database.MailForwarding{}, &database.Email{}, &database.Recipient{}, &database.Attachment{},
		&database.MessageSource{}, &database.MailBlockRule{}, &database.MailFolder{}, &database.FolderMessage{},
		&database.AccountNotificationSettings{}, &database.MailSendUsage{}, &database.MailOutbox{},
	); err != nil {
		t.Fatalf("AutoMigrate() error = %v", err)
	}

	notifier := &fakeNotifier{}
	summarizer := &fakeSummarizer{summary: "Acme shipped three features"}
	svc := NewEmailService(&database.DB{DB: db}, notifier)
	svc.SetWorkspaceProvider(fakeWorkspaceProvider{})
	svc.SetAccountLanguageProvider(fakeLanguageProvider{language: "zh-CN"})
	svc.SetNotificationSummarizer(summarizer)
	svc.SetDomain("example.com")

	accountID := uuid.New()
	mailbox := database.Mailbox{ID: database.NewID(), AccountID: accountID, WorkspaceID: "workspace-a", Address: "ada@example.com", Name: "Ada"}
	if err := db.Create(&mailbox).Error; err != nil {
		t.Fatalf("create mailbox: %v", err)
	}
	return notificationFixture{svc: svc, db: db, notifier: notifier, summarizer: summarizer, accountID: accountID, mailbox: mailbox}
}

// receive delivers one inbound message through the public delivery path.
func (f notificationFixture) receive(t *testing.T, subject, body string) {
	t.Helper()
	if _, err := f.svc.ReceiveEmail(context.Background(), ReceiveEmailInput{
		MailboxID:   f.mailbox.ID,
		FromAddress: "sender@remote.example",
		FromName:    "Acme",
		Subject:     subject,
		Body:        body,
		ContentType: "text/plain",
	}); err != nil {
		t.Fatalf("ReceiveEmail() error = %v", err)
	}
}

// sent returns the single notification the fixture pushed.
func (f notificationFixture) sent(t *testing.T) ring.EmailNotification {
	t.Helper()
	if len(f.notifier.sent) != 1 {
		t.Fatalf("notifications sent = %d, want 1", len(f.notifier.sent))
	}
	return f.notifier.sent[0]
}

func TestReceiveEmailHighlightsCodesByDefault(t *testing.T) {
	f := newNotificationFixture(t)
	f.receive(t, "Your GitHub verification code", "Your code is 482913. It expires in 10 minutes.")

	notification := f.sent(t)
	if notification.Highlight.Kind != mailintel.KindCode || notification.Highlight.Text != "482913" {
		t.Fatalf("highlight = %+v, want the verification code", notification.Highlight)
	}
	if notification.Language != "zh-CN" {
		t.Fatalf("language = %q, want the account language", notification.Language)
	}
	if notification.AccountID != f.accountID.String() || notification.EmailID == "" {
		t.Fatalf("payload identity = %s/%s, want the account and a stored email id", notification.AccountID, notification.EmailID)
	}
	if len(f.summarizer.requests) != 0 {
		t.Fatalf("summaries requested = %d, want 0 for secret-bearing mail", len(f.summarizer.requests))
	}
}

func TestReceiveEmailNeverSummarizesSecretBearingMail(t *testing.T) {
	f := newNotificationFixture(t)
	if _, err := f.svc.UpdateNotificationSettings(context.Background(), f.accountID, UpdateNotificationSettingsInput{
		Highlight: boolPtr(false),
		Summarize: boolPtr(true),
	}); err != nil {
		t.Fatalf("UpdateNotificationSettings() error = %v", err)
	}

	f.receive(t, "Your Acme verification code", "Your code is 771203.")
	if notification := f.sent(t); notification.Highlight != (mailintel.Highlight{}) {
		t.Fatalf("highlight = %+v, want none while the account disabled highlighting", notification.Highlight)
	}
	if len(f.summarizer.requests) != 0 {
		t.Fatalf("summaries requested = %d, want 0: a code was extracted even though highlighting is off", len(f.summarizer.requests))
	}
}

func TestReceiveEmailSummarizesOrdinaryMail(t *testing.T) {
	f := newNotificationFixture(t)
	if _, err := f.svc.UpdateNotificationSettings(context.Background(), f.accountID, UpdateNotificationSettingsInput{
		Summarize: boolPtr(true),
	}); err != nil {
		t.Fatalf("UpdateNotificationSettings() error = %v", err)
	}

	f.receive(t, "This week at Acme", "Hello Ada, here is everything that shipped this week. Enjoy the read.")
	notification := f.sent(t)
	if notification.Summary != "Acme shipped three features" {
		t.Fatalf("summary = %q, want the personality answer", notification.Summary)
	}
	if len(f.summarizer.requests) != 1 {
		t.Fatalf("summaries requested = %d, want 1", len(f.summarizer.requests))
	}
	request := f.summarizer.requests[0]
	if request.Subject != "This week at Acme" || request.FromName != "Acme" || request.Language != "zh-CN" {
		t.Fatalf("summary request = %+v, want the message context", request)
	}

	var stored database.Email
	if err := f.db.Order("created_at desc").First(&stored).Error; err != nil {
		t.Fatalf("load stored email: %v", err)
	}
	if stored.Summary != "Acme shipped three features" {
		t.Fatalf("stored summary = %q, want it persisted on the message", stored.Summary)
	}
}

func TestReceiveEmailSkipsSummariesWhenTheAccountIsOverItsPersonalityLimit(t *testing.T) {
	f := newNotificationFixture(t)
	f.summarizer.err = status.Error(codes.ResourceExhausted, "Personality usage threshold exceeded: golds usage limit is 5")
	if _, err := f.svc.UpdateNotificationSettings(context.Background(), f.accountID, UpdateNotificationSettingsInput{
		Summarize: boolPtr(true),
	}); err != nil {
		t.Fatalf("UpdateNotificationSettings() error = %v", err)
	}

	f.receive(t, "This week at Acme", "Hello Ada, here is everything that shipped this week.")
	notification := f.sent(t)
	if notification.Summary != "" {
		t.Fatalf("summary = %q, want none when Personality refuses the call", notification.Summary)
	}
	if notification.Subject != "This week at Acme" {
		t.Fatalf("subject = %q, want the notification to keep its fallback", notification.Subject)
	}
}

func TestReceiveEmailKeepsNotificationWhenSummarizerFails(t *testing.T) {
	f := newNotificationFixture(t)
	f.summarizer.err = errors.New("personality service unavailable")
	if _, err := f.svc.UpdateNotificationSettings(context.Background(), f.accountID, UpdateNotificationSettingsInput{
		Summarize: boolPtr(true),
	}); err != nil {
		t.Fatalf("UpdateNotificationSettings() error = %v", err)
	}

	f.receive(t, "This week at Acme", "Hello Ada, here is everything that shipped this week.")
	notification := f.sent(t)
	if notification.Summary != "" {
		t.Fatalf("summary = %q, want none after a summarizer failure", notification.Summary)
	}
	if notification.Subject != "This week at Acme" {
		t.Fatalf("subject = %q, want the notification to keep its fallback", notification.Subject)
	}
}

func TestNotificationSettingsDefaultAndUpdate(t *testing.T) {
	f := newNotificationFixture(t)
	ctx := context.Background()

	settings, err := f.svc.NotificationSettings(ctx, f.accountID)
	if err != nil {
		t.Fatalf("NotificationSettings() error = %v", err)
	}
	if !settings.Highlight || settings.Summarize {
		t.Fatalf("defaults = %+v, want highlighting on and summaries off", settings)
	}

	updated, err := f.svc.UpdateNotificationSettings(ctx, f.accountID, UpdateNotificationSettingsInput{Highlight: boolPtr(false)})
	if err != nil {
		t.Fatalf("UpdateNotificationSettings() error = %v", err)
	}
	if updated.Highlight || updated.Summarize {
		t.Fatalf("updated = %+v, want only the provided field changed", updated)
	}
	// A second update must keep the stored value it did not mention.
	updated, err = f.svc.UpdateNotificationSettings(ctx, f.accountID, UpdateNotificationSettingsInput{Summarize: boolPtr(true)})
	if err != nil {
		t.Fatalf("UpdateNotificationSettings() error = %v", err)
	}
	if updated.Highlight || !updated.Summarize {
		t.Fatalf("updated = %+v, want highlight off and summarize on", updated)
	}
	stored, err := f.svc.NotificationSettings(ctx, f.accountID)
	if err != nil {
		t.Fatalf("NotificationSettings() error = %v", err)
	}
	if stored.Highlight || !stored.Summarize {
		t.Fatalf("stored = %+v, want the update persisted", stored)
	}
}

func boolPtr(value bool) *bool { return &value }

func TestNotifySummarizerRefusalsAreNotWarnings(t *testing.T) {
	// A user over their Personality quota is an expected outcome, not an
	// operator problem: it must not warn on every delivered message.
	for name, test := range map[string]struct {
		err  error
		want string
	}{
		"quota refusal is quiet": {
			err:  status.Error(codes.ResourceExhausted, "Personality usage threshold exceeded"),
			want: "skipping notification summary",
		},
		"service failure warns": {
			err:  status.Error(codes.Unavailable, "personality service unavailable"),
			want: "failed to summarize incoming email",
		},
	} {
		t.Run(name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := logging.Log
			logging.Log = zerolog.New(&logs).Level(zerolog.DebugLevel)
			t.Cleanup(func() { logging.Log = previous })

			f := newNotificationFixture(t)
			f.summarizer.err = test.err
			if _, err := f.svc.UpdateNotificationSettings(context.Background(), f.accountID, UpdateNotificationSettingsInput{
				Summarize: boolPtr(true),
			}); err != nil {
				t.Fatalf("UpdateNotificationSettings() error = %v", err)
			}
			f.receive(t, "This week at Acme", "Hello Ada, here is everything that shipped this week.")

			if got := logs.String(); !strings.Contains(got, test.want) {
				t.Fatalf("logs = %q, want %q", got, test.want)
			}
		})
	}
}
