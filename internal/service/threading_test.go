package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/relay"
)

// newThreadingTestService builds a service with one account and mailbox, no
// notification wiring, for reply-chain resolution tests.
func newThreadingTestService(t *testing.T) (*EmailService, *gorm.DB, uuid.UUID, database.Mailbox) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+database.NewID()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open() error = %v", err)
	}
	if err := db.AutoMigrate(
		&database.Mailbox{}, &database.MailboxAlias{}, &database.MailForwarding{}, &database.Email{}, &database.Recipient{}, &database.Attachment{},
		&database.MessageSource{}, &database.MailBlockRule{}, &database.MailFolder{}, &database.FolderMessage{},
		&database.MailSendUsage{}, &database.MailOutbox{}, &database.DmarcReport{}, &database.DmarcReportRecord{},
	); err != nil {
		t.Fatalf("AutoMigrate() error = %v", err)
	}
	svc := NewEmailService(&database.DB{DB: db}, nil)
	svc.SetWorkspaceProvider(fakeWorkspaceProvider{})

	accountID := uuid.New()
	mailbox := database.Mailbox{ID: database.NewID(), AccountID: accountID, WorkspaceID: "workspace-a", Address: "ada@example.com", Name: "Ada"}
	if err := db.Create(&mailbox).Error; err != nil {
		t.Fatalf("create mailbox: %v", err)
	}
	return svc, db, accountID, mailbox
}

func TestReceiveEmailChainsReplyToParentThread(t *testing.T) {
	svc, db, accountID, mailbox := newThreadingTestService(t)
	ctx := context.Background()

	// The original message lands as its own thread root.
	original, err := svc.ReceiveEmail(ctx, ReceiveEmailInput{
		MailboxID: mailbox.ID, MessageID: "<m1@example.com>",
		FromAddress: "sender@remote.example", Subject: "Release plan", Body: "original",
		ContentType: "text/plain",
	})
	if err != nil {
		t.Fatalf("ReceiveEmail(original) error = %v", err)
	}
	if original.ThreadID == nil || *original.ThreadID == "" {
		t.Fatalf("original thread = %v, want a thread id", original.ThreadID)
	}

	// A reply naming that message joins its thread even though the caller
	// supplied no thread_id.
	reply, err := svc.ReceiveEmail(ctx, ReceiveEmailInput{
		MailboxID: mailbox.ID, MessageID: "<m2@example.com>",
		InReplyTo:  []string{"<m1@example.com>"},
		References: []string{"<m1@example.com>"},
		FromAddress: "sender@remote.example", Subject: "Re: Release plan", Body: "reply",
		ContentType: "text/plain",
	})
	if err != nil {
		t.Fatalf("ReceiveEmail(reply) error = %v", err)
	}
	if reply.ThreadID == nil || *reply.ThreadID != *original.ThreadID {
		t.Fatalf("reply thread = %v, want %s", reply.ThreadID, *original.ThreadID)
	}
	if reply.InReplyTo != "m1@example.com" || reply.References != "m1@example.com" {
		t.Fatalf("reply chain = in_reply_to %q references %q, want m1@example.com", reply.InReplyTo, reply.References)
	}

	// The chain continues: reply-to-reply joins the same thread.
	reReply, err := svc.ReceiveEmail(ctx, ReceiveEmailInput{
		MailboxID: mailbox.ID, MessageID: "<m3@example.com>",
		InReplyTo:  []string{"<m2@example.com>"},
		References: []string{"<m1@example.com>", "<m2@example.com>"},
		FromAddress: "other@remote.example", Subject: "Re: Release plan", Body: "re-reply",
		ContentType: "text/plain",
	})
	if err != nil {
		t.Fatalf("ReceiveEmail(re-reply) error = %v", err)
	}
	if reReply.ThreadID == nil || *reReply.ThreadID != *original.ThreadID {
		t.Fatalf("re-reply thread = %v, want %s", reReply.ThreadID, *original.ThreadID)
	}

	// GetThread returns the whole conversation, oldest first.
	conversation, err := svc.GetThread(ctx, accountID, *original.ThreadID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if len(conversation) != 3 {
		t.Fatalf("conversation has %d messages, want 3", len(conversation))
	}
	if conversation[0].ID != original.ID || conversation[2].ID != reReply.ID {
		t.Fatalf("conversation order = %s..%s, want %s..%s", conversation[0].ID, conversation[2].ID, original.ID, reReply.ID)
	}
	_ = db
}

func TestReceiveEmailChainSkipsUnknownReferences(t *testing.T) {
	svc, _, _, mailbox := newThreadingTestService(t)
	ctx := context.Background()

	// A reply whose chain names no known message starts its own thread.
	orphan, err := svc.ReceiveEmail(ctx, ReceiveEmailInput{
		MailboxID: mailbox.ID, MessageID: "<o1@example.com>",
		InReplyTo:  []string{"<missing@example.com>"},
		References: []string{"<missing@example.com>"},
		FromAddress: "sender@remote.example", Subject: "Hello", Body: "body",
		ContentType: "text/plain",
	})
	if err != nil {
		t.Fatalf("ReceiveEmail(orphan) error = %v", err)
	}
	if orphan.ThreadID == nil || *orphan.ThreadID == "" {
		t.Fatalf("orphan thread = %v, want a thread id", orphan.ThreadID)
	}
}

func TestImportEmailsChainsRepliesFromReferences(t *testing.T) {
	svc, db, accountID, mailbox := newThreadingTestService(t)
	ctx := context.Background()

	result, err := svc.ImportEmails(ctx, accountID, ImportEmailsInput{Emails: []ImportEmailItem{
		{MailboxID: mailbox.ID, MessageID: "<i1@example.com>", FromAddress: "a@remote.example", Subject: "Plan", Body: "one", ContentType: "text/plain"},
		{MailboxID: mailbox.ID, MessageID: "<i2@example.com>", FromAddress: "b@remote.example", Subject: "Re: Plan", Body: "two", ContentType: "text/plain",
			InReplyTo: []string{"<i1@example.com>"}, References: []string{"<i1@example.com>"}},
	}})
	if err != nil {
		t.Fatalf("ImportEmails() error = %v", err)
	}
	if result.Imported != 2 || result.Failed != 0 {
		t.Fatalf("counts = imported %d failed %d, want 2/0", result.Imported, result.Failed)
	}

	var first, second database.Email
	if err := db.First(&first, "message_id = ?", "i1@example.com").Error; err != nil {
		t.Fatalf("load first: %v", err)
	}
	if err := db.First(&second, "message_id = ?", "i2@example.com").Error; err != nil {
		t.Fatalf("load second: %v", err)
	}
	if second.ThreadID == nil || first.ThreadID == nil || *second.ThreadID != *first.ThreadID {
		t.Fatalf("threads = %v / %v, want a shared thread", first.ThreadID, second.ThreadID)
	}
}

func TestImportEmailsChainsChildBeforeParentInBatch(t *testing.T) {
	svc, db, accountID, mailbox := newThreadingTestService(t)
	ctx := context.Background()

	// The child arrives first in the file; the parent later. The batch map
	// still joins both into one thread.
	result, err := svc.ImportEmails(ctx, accountID, ImportEmailsInput{Emails: []ImportEmailItem{
		{MailboxID: mailbox.ID, MessageID: "<c1@example.com>", FromAddress: "b@remote.example", Subject: "Re: Plan", Body: "two", ContentType: "text/plain",
			InReplyTo: []string{"<p1@example.com>"}, References: []string{"<p1@example.com>"}},
		{MailboxID: mailbox.ID, MessageID: "<p1@example.com>", FromAddress: "a@remote.example", Subject: "Plan", Body: "one", ContentType: "text/plain"},
	}})
	if err != nil {
		t.Fatalf("ImportEmails() error = %v", err)
	}
	if result.Imported != 2 || result.Failed != 0 {
		t.Fatalf("counts = imported %d failed %d, want 2/0", result.Imported, result.Failed)
	}

	var child, parent database.Email
	if err := db.First(&child, "message_id = ?", "c1@example.com").Error; err != nil {
		t.Fatalf("load child: %v", err)
	}
	if err := db.First(&parent, "message_id = ?", "p1@example.com").Error; err != nil {
		t.Fatalf("load parent: %v", err)
	}
	if child.ThreadID == nil || parent.ThreadID == nil || *child.ThreadID != *parent.ThreadID {
		t.Fatalf("threads = %v / %v, want a shared thread", parent.ThreadID, child.ThreadID)
	}
}

func TestSendEmailCarriesMessageIDAndReplyChain(t *testing.T) {
	svc, db, accountID, mailbox := newThreadingTestService(t)
	svc.SetRelay(relay.DisabledAdapter{})
	ctx := context.Background()

	// A reply to a known message carries the chain headers and joins its thread.
	original, err := svc.ReceiveEmail(ctx, ReceiveEmailInput{
		MailboxID: mailbox.ID, MessageID: "<s1@example.com>",
		FromAddress: "sender@remote.example", Subject: "Plan", Body: "one",
		ContentType: "text/plain",
	})
	if err != nil {
		t.Fatalf("ReceiveEmail(original) error = %v", err)
	}

	reply, err := svc.SendEmail(ctx, accountID, SendEmailInput{
		MailboxID: mailbox.ID, ReplyToID: original.ID,
		To:      []RecipientInput{{Address: "sender@remote.example", Kind: "to"}},
		Subject: "Re: Plan", Body: "reply", ContentType: "text/plain",
	})
	if err != nil {
		t.Fatalf("SendEmail(reply) error = %v", err)
	}
	if reply.MessageID == nil || *reply.MessageID == "" {
		t.Fatalf("reply has no message-id")
	}
	if reply.InReplyTo != "s1@example.com" {
		t.Fatalf("reply in_reply_to = %q, want s1@example.com", reply.InReplyTo)
	}
	if reply.References != "s1@example.com" {
		t.Fatalf("reply references = %q, want s1@example.com", reply.References)
	}
	if reply.ThreadID == nil || *reply.ThreadID != *original.ThreadID {
		t.Fatalf("reply thread = %v, want %s", reply.ThreadID, *original.ThreadID)
	}

	var stored database.Email
	if err := db.First(&stored, "id = ?", reply.ID).Error; err != nil {
		t.Fatalf("reload reply: %v", err)
	}
	if stored.MessageID == nil || *stored.MessageID == "" {
		t.Fatalf("stored reply has no message-id")
	}
}
