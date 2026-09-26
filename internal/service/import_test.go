package service

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/workspace"
)

// fakeWorkspaceProvider lets ImportEmails run quota enforcement and workspace
// membership checks without a live provider.
type fakeWorkspaceProvider struct{}

func (fakeWorkspaceProvider) AuthorizeMember(context.Context, string, string) error    { return nil }
func (fakeWorkspaceProvider) PlanStorageBytes(context.Context, string) (int64, error)  { return 1 << 30, nil }
func (fakeWorkspaceProvider) MailboxLimit(context.Context, string) (int64, error)      { return 10, nil }
func (fakeWorkspaceProvider) CustomDomainLimit(context.Context, string) (int64, error) { return 0, nil }
func (fakeWorkspaceProvider) SendLimits(context.Context, string) (workspace.SendLimits, error) {
	return workspace.SendLimits{}, nil
}
func (fakeWorkspaceProvider) Close() error { return nil }

func newImportTestService(t *testing.T) (*EmailService, *gorm.DB, uuid.UUID, database.Mailbox, database.Mailbox) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+database.NewID()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open() error = %v", err)
	}
	if err := db.AutoMigrate(
		&database.Mailbox{}, &database.MailboxAlias{}, &database.MailForwarding{}, &database.CustomDomain{}, &database.Email{}, &database.Recipient{}, &database.Attachment{},
		&database.MailProtocolCredential{}, &database.EmailLabel{}, &database.EmailLabelMapping{},
		&database.MailSendUsage{}, &database.MailBlockRule{}, &database.MessageSource{}, &database.DmarcReport{},
		&database.DmarcReportRecord{}, &database.MailFolder{}, &database.FolderMessage{}, &database.MailOutbox{},
	); err != nil {
		t.Fatalf("AutoMigrate() error = %v", err)
	}
	svc := NewEmailService(&database.DB{DB: db}, nil)
	svc.SetWorkspaceProvider(fakeWorkspaceProvider{})

	accountID := uuid.New()
	mailboxA := database.Mailbox{ID: database.NewID(), AccountID: accountID, WorkspaceID: "workspace-a", Address: "alice@example.com", Name: "Alice"}
	mailboxB := database.Mailbox{ID: database.NewID(), AccountID: accountID, WorkspaceID: "workspace-b", Address: "bob@example.com", Name: "Bob"}
	for _, mailbox := range []database.Mailbox{mailboxA, mailboxB} {
		if err := db.Create(&mailbox).Error; err != nil {
			t.Fatalf("create mailbox: %v", err)
		}
	}
	return svc, db, accountID, mailboxA, mailboxB
}

func importItem(mailboxID, messageID string) ImportEmailItem {
	return ImportEmailItem{
		MailboxID: mailboxID, MessageID: messageID, FromAddress: "sender@example.net",
		FromName: "Sender", Subject: "Hello", Body: "Message body",
	}
}

func TestImportEmailsDedupesWithinBatch(t *testing.T) {
	svc, _, accountID, mailboxA, _ := newImportTestService(t)
	result, err := svc.ImportEmails(context.Background(), accountID, ImportEmailsInput{
		Emails: []ImportEmailItem{
			importItem(mailboxA.ID, "<abc@example.com>"),
			importItem(mailboxA.ID, "<abc@example.com>"),
		},
	})
	if err != nil {
		t.Fatalf("ImportEmails() error = %v", err)
	}
	if result.Imported != 1 || result.Duplicates != 1 || result.Failed != 0 {
		t.Fatalf("counts = imported %d duplicates %d failed %d, want 1/1/0", result.Imported, result.Duplicates, result.Failed)
	}
	if result.Items[0].Status != "imported" || result.Items[0].EmailID == "" {
		t.Fatalf("first item = %+v, want imported with email_id", result.Items[0])
	}
	if result.Items[1].Status != "duplicate" || result.Items[1].EmailID != "" {
		t.Fatalf("second item = %+v, want duplicate without email_id", result.Items[1])
	}
}

func TestImportEmailsSameMessageIDDifferentMailboxes(t *testing.T) {
	svc, _, accountID, mailboxA, mailboxB := newImportTestService(t)
	result, err := svc.ImportEmails(context.Background(), accountID, ImportEmailsInput{
		Emails: []ImportEmailItem{
			importItem(mailboxA.ID, "<fwd@example.com>"),
			importItem(mailboxB.ID, "<fwd@example.com>"),
		},
	})
	if err != nil {
		t.Fatalf("ImportEmails() error = %v", err)
	}
	if result.Imported != 2 || result.Duplicates != 0 || result.Failed != 0 {
		t.Fatalf("counts = imported %d duplicates %d failed %d, want 2/0/0", result.Imported, result.Duplicates, result.Failed)
	}
}

func TestImportEmailsDedupeOff(t *testing.T) {
	svc, _, accountID, mailboxA, _ := newImportTestService(t)
	result, err := svc.ImportEmails(context.Background(), accountID, ImportEmailsInput{
		Dedupe: DedupeOff,
		Emails: []ImportEmailItem{
			importItem(mailboxA.ID, "<abc@example.com>"),
			importItem(mailboxA.ID, "<abc@example.com>"),
		},
	})
	if err != nil {
		t.Fatalf("ImportEmails() error = %v", err)
	}
	if result.Imported != 2 || result.Duplicates != 0 || result.Failed != 0 {
		t.Fatalf("counts = imported %d duplicates %d failed %d, want 2/0/0", result.Imported, result.Duplicates, result.Failed)
	}
}

func TestImportEmailsDedupesAgainstHistory(t *testing.T) {
	svc, _, accountID, mailboxA, _ := newImportTestService(t)
	item := importItem(mailboxA.ID, "<abc@example.com>")
	if _, err := svc.ImportEmails(context.Background(), accountID, ImportEmailsInput{Emails: []ImportEmailItem{item}}); err != nil {
		t.Fatalf("first ImportEmails() error = %v", err)
	}
	result, err := svc.ImportEmails(context.Background(), accountID, ImportEmailsInput{Emails: []ImportEmailItem{item}})
	if err != nil {
		t.Fatalf("second ImportEmails() error = %v", err)
	}
	if result.Imported != 0 || result.Duplicates != 1 || result.Failed != 0 {
		t.Fatalf("counts = imported %d duplicates %d failed %d, want 0/1/0", result.Imported, result.Duplicates, result.Failed)
	}
	if result.Items[0].Status != "duplicate" {
		t.Fatalf("item status = %q, want duplicate", result.Items[0].Status)
	}
}

func TestImportEmailsWithoutMessageIDNeverDedupes(t *testing.T) {
	svc, _, accountID, mailboxA, _ := newImportTestService(t)
	item := importItem(mailboxA.ID, "")
	result, err := svc.ImportEmails(context.Background(), accountID, ImportEmailsInput{Emails: []ImportEmailItem{item, item}})
	if err != nil {
		t.Fatalf("ImportEmails() error = %v", err)
	}
	if result.Imported != 2 || result.Duplicates != 0 || result.Failed != 0 {
		t.Fatalf("counts = imported %d duplicates %d failed %d, want 2/0/0", result.Imported, result.Duplicates, result.Failed)
	}
}

func TestImportEmailsPersistsInboxMembershipAndSource(t *testing.T) {
	svc, db, accountID, mailboxA, _ := newImportTestService(t)
	sentAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	item := ImportEmailItem{
		MailboxID: mailboxA.ID, MessageID: "<abc@example.com>", FromAddress: "sender@example.net",
		FromName: "Sender", Subject: "Hello", Body: "Message body", ContentType: "text/html",
		To:     []RecipientInput{{Address: "alice@example.com", Name: "Alice", Kind: "to"}},
		SentAt: &sentAt,
	}
	result, err := svc.ImportEmails(context.Background(), accountID, ImportEmailsInput{Emails: []ImportEmailItem{item}})
	if err != nil {
		t.Fatalf("ImportEmails() error = %v", err)
	}
	if result.Imported != 1 || result.Items[0].EmailID == "" {
		t.Fatalf("result = %+v, want one imported email", result)
	}
	emailID := result.Items[0].EmailID

	var email database.Email
	if err := db.First(&email, "id = ?", emailID).Error; err != nil {
		t.Fatalf("load email: %v", err)
	}
	if email.Folder != "inbox" {
		t.Fatalf("folder = %q, want inbox", email.Folder)
	}
	// message_ids are stored normalized (without angle brackets), matching what
	// the client sends and what reply-chain lookups compare against.
	if email.MessageID == nil || *email.MessageID != "abc@example.com" {
		t.Fatalf("message_id = %v, want abc@example.com", email.MessageID)
	}
	if email.ContentType != "text/html" {
		t.Fatalf("content_type = %q, want text/html", email.ContentType)
	}
	if email.SentAt == nil || !email.SentAt.Equal(sentAt) {
		t.Fatalf("sent_at = %v, want %v", email.SentAt, sentAt)
	}

	var inbox database.MailFolder
	if err := db.Where("mailbox_id = ? AND name = ?", mailboxA.ID, "INBOX").First(&inbox).Error; err != nil {
		t.Fatalf("load INBOX folder: %v", err)
	}
	var membership database.FolderMessage
	if err := db.Where("folder_id = ? AND email_id = ?", inbox.ID, emailID).First(&membership).Error; err != nil {
		t.Fatalf("load FolderMessage: %v", err)
	}
	var source database.MessageSource
	if err := db.Where("email_id = ?", emailID).First(&source).Error; err != nil {
		t.Fatalf("load MessageSource: %v", err)
	}
	var recipient database.Recipient
	if err := db.Where("email_id = ? AND kind = ?", emailID, "to").First(&recipient).Error; err != nil {
		t.Fatalf("load recipient: %v", err)
	}
}
