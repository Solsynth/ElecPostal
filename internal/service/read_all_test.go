package service

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"src.solsynth.dev/sosys/elecpostal/internal/database"
)

// createReadTestEmail seeds one message with the read/archive/intake flags the
// scope tests care about.
func createReadTestEmail(t *testing.T, db *gorm.DB, accountID uuid.UUID, mailboxID, folder string, isRead, archived, dmarc bool) database.Email {
	t.Helper()
	email := database.Email{
		ID: database.NewID(), AccountID: accountID, MailboxID: mailboxID,
		Subject: "Subject " + folder, Body: "body", Folder: folder,
		FromAddress: "sender@example.com", DeliveryStatus: "sent", IsRead: isRead,
		IsDmarcIntake: dmarc,
	}
	if archived {
		now := time.Now()
		email.ArchivedAt = &now
	}
	if err := db.Create(&email).Error; err != nil {
		t.Fatalf("create email: %v", err)
	}
	return email
}

func TestMarkAllReadAccountWideSkipsReadArchivedAndIntake(t *testing.T) {
	svc, db, _, accountID, mailbox := newTrashTestService(t)
	second := database.Mailbox{ID: database.NewID(), AccountID: accountID, WorkspaceID: "workspace-a", Address: "bob@example.com"}
	if err := db.Create(&second).Error; err != nil {
		t.Fatalf("create second mailbox: %v", err)
	}
	createReadTestEmail(t, db, accountID, mailbox.ID, "inbox", false, false, false)
	createReadTestEmail(t, db, accountID, mailbox.ID, "sent", false, false, false)
	createReadTestEmail(t, db, accountID, second.ID, "inbox", false, false, false)
	alreadyRead := createReadTestEmail(t, db, accountID, mailbox.ID, "inbox", true, false, false)
	archived := createReadTestEmail(t, db, accountID, mailbox.ID, "inbox", false, true, false)
	intake := createReadTestEmail(t, db, accountID, mailbox.ID, "inbox", false, false, true)
	otherAccount := createReadTestEmail(t, db, uuid.New(), mailbox.ID, "inbox", false, false, false)

	updated, err := svc.MarkAllRead(context.Background(), accountID, MarkAllReadInput{})
	if err != nil {
		t.Fatalf("MarkAllRead() error = %v", err)
	}
	if updated != 3 {
		t.Fatalf("MarkAllRead() updated = %d, want 3", updated)
	}
	for _, id := range []string{archived.ID, intake.ID, otherAccount.ID} {
		var row database.Email
		if err := db.First(&row, "id = ?", id).Error; err != nil {
			t.Fatalf("load %s: %v", id, err)
		}
		if row.IsRead {
			t.Fatalf("email %s was marked read but is out of scope", id)
		}
	}
	var readRow database.Email
	if err := db.First(&readRow, "id = ?", alreadyRead.ID).Error; err != nil {
		t.Fatalf("load already-read email: %v", err)
	}
	if !readRow.IsRead {
		t.Fatalf("sweep unset the read flag on an already-read email")
	}
	var unread int64
	if err := db.Model(&database.Email{}).Where("account_id = ? AND is_read = ?", accountID, false).Count(&unread).Error; err != nil {
		t.Fatalf("count unread: %v", err)
	}
	if unread != 2 {
		t.Fatalf("remaining unread = %d, want 2 (archived, intake)", unread)
	}
}

func TestMarkAllReadNarrowsToFolder(t *testing.T) {
	svc, db, _, accountID, mailbox := newTrashTestService(t)
	second := database.Mailbox{ID: database.NewID(), AccountID: accountID, WorkspaceID: "workspace-a", Address: "bob@example.com"}
	if err := db.Create(&second).Error; err != nil {
		t.Fatalf("create second mailbox: %v", err)
	}
	createReadTestEmail(t, db, accountID, mailbox.ID, "inbox", false, false, false)
	sent := createReadTestEmail(t, db, accountID, mailbox.ID, "sent", false, false, false)
	createReadTestEmail(t, db, accountID, second.ID, "inbox", false, false, false)

	updated, err := svc.MarkAllRead(context.Background(), accountID, MarkAllReadInput{Folder: "inbox"})
	if err != nil {
		t.Fatalf("MarkAllRead(folder) error = %v", err)
	}
	if updated != 2 {
		t.Fatalf("folder sweep updated = %d, want 2 (both inboxes)", updated)
	}
	var row database.Email
	if err := db.First(&row, "id = ?", sent.ID).Error; err != nil {
		t.Fatalf("load sent: %v", err)
	}
	if row.IsRead {
		t.Fatalf("folder sweep marked mail outside the folder read")
	}
}

func TestMarkAllReadNarrowsToMailboxAndFolder(t *testing.T) {
	svc, db, _, accountID, mailbox := newTrashTestService(t)
	second := database.Mailbox{ID: database.NewID(), AccountID: accountID, WorkspaceID: "workspace-a", Address: "bob@example.com"}
	if err := db.Create(&second).Error; err != nil {
		t.Fatalf("create second mailbox: %v", err)
	}
	firstInbox := createReadTestEmail(t, db, accountID, mailbox.ID, "inbox", false, false, false)
	createReadTestEmail(t, db, accountID, second.ID, "inbox", false, false, false)
	createReadTestEmail(t, db, accountID, second.ID, "sent", false, false, false)

	updated, err := svc.MarkAllRead(context.Background(), accountID, MarkAllReadInput{MailboxID: second.ID})
	if err != nil {
		t.Fatalf("MarkAllRead(mailbox) error = %v", err)
	}
	if updated != 2 {
		t.Fatalf("mailbox sweep updated = %d, want 2 (both folders of one mailbox)", updated)
	}
	var row database.Email
	if err := db.First(&row, "id = ?", firstInbox.ID).Error; err != nil {
		t.Fatalf("load first inbox: %v", err)
	}
	if row.IsRead {
		t.Fatalf("mailbox sweep marked mail in another mailbox read")
	}

	// A mailbox plus folder scope only touches the matching slice.
	createReadTestEmail(t, db, accountID, second.ID, "sent", false, false, false)
	updated, err = svc.MarkAllRead(context.Background(), accountID, MarkAllReadInput{MailboxID: second.ID, Folder: "sent"})
	if err != nil {
		t.Fatalf("MarkAllRead(mailbox, folder) error = %v", err)
	}
	if updated != 1 {
		t.Fatalf("combined sweep updated = %d, want 1", updated)
	}
}

func TestMarkAllReadRejectsUnknownFolder(t *testing.T) {
	svc, _, _, accountID, _ := newTrashTestService(t)
	if _, err := svc.MarkAllRead(context.Background(), accountID, MarkAllReadInput{Folder: "junk"}); err == nil {
		t.Fatal("MarkAllRead() with an unknown folder = nil error, want failure")
	}
}
