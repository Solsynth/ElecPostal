package service

import (
	"context"
	"encoding/json"
	"testing"

	"gorm.io/gorm"

	"src.solsynth.dev/sosys/elecpostal/internal/database"
)

// protocolFoldersOf reports the protocol folder names a message is filed in.
func protocolFoldersOf(t *testing.T, svc *EmailService, mailboxID, emailID string) []string {
	t.Helper()
	var names []string
	if err := svc.db.WithContext(context.Background()).Table("folder_messages").
		Joins("JOIN mail_folders ON mail_folders.id = folder_messages.folder_id").
		Where("mail_folders.mailbox_id = ? AND folder_messages.email_id = ?", mailboxID, emailID).
		Order("mail_folders.name").Pluck("mail_folders.name", &names).Error; err != nil {
		t.Fatalf("load protocol folders: %v", err)
	}
	return names
}

// ensureProtocolNamespace provisions the folder namespace a live mailbox has.
func ensureProtocolNamespace(t *testing.T, svc *EmailService, mailboxID string) {
	t.Helper()
	if err := svc.db.WithContext(context.Background()).Transaction(func(tx *gorm.DB) error {
		return svc.ensureProtocolFoldersTx(tx, mailboxID)
	}); err != nil {
		t.Fatalf("ensure protocol folders: %v", err)
	}
}

func loadEmailFolder(t *testing.T, svc *EmailService, emailID string) database.Email {
	t.Helper()
	var stored database.Email
	if err := svc.db.WithContext(context.Background()).First(&stored, "id = ?", emailID).Error; err != nil {
		t.Fatalf("load email: %v", err)
	}
	return stored
}

// TestMoveEmailFilesTheMessageIntoTheProtocolFolder covers an app-side move: the
// mail the user marks as spam has to leave the IMAP INBOX and appear in Spam.
func TestMoveEmailFilesTheMessageIntoTheProtocolFolder(t *testing.T) {
	svc, db, accountID, mailbox, _ := newImportTestService(t)
	ctx := context.Background()
	email := createTrashTestEmail(t, db, accountID, mailbox, folderInbox, "")
	ensureProtocolNamespace(t, svc, mailbox.ID)
	if err := db.Model(&database.FolderMessage{}).Where("email_id = ?", email.ID).
		Update("flags", marshalFlags([]string{`\Seen`, `\Flagged`})).Error; err != nil {
		t.Fatalf("seed client flags: %v", err)
	}

	if err := svc.MoveEmail(ctx, accountID, email.ID, folderSpam); err != nil {
		t.Fatalf("MoveEmail() error = %v", err)
	}
	stored := loadEmailFolder(t, svc, email.ID)
	if stored.Folder != folderSpam {
		t.Fatalf("emails.folder = %q, want %q", stored.Folder, folderSpam)
	}
	if stored.SpamAt == nil {
		t.Fatalf("spam_at was not recorded")
	}
	if got := protocolFoldersOf(t, svc, mailbox.ID, email.ID); len(got) != 1 || got[0] != "Spam" {
		t.Fatalf("protocol folders = %v, want [Spam]", got)
	}
	var membership database.FolderMessage
	if err := db.First(&membership, "email_id = ?", email.ID).Error; err != nil {
		t.Fatalf("load membership: %v", err)
	}
	var flags []string
	if err := json.Unmarshal(membership.Flags, &flags); err != nil {
		t.Fatalf("decode flags: %v", err)
	}
	if !hasSystemFlag(flags, `\Seen`) || !hasSystemFlag(flags, `\Flagged`) {
		t.Fatalf("flags = %v, want the client's \\Seen and \\Flagged to survive the move", flags)
	}
}

// TestDeleteEmailFilesTheMessageIntoTrash covers the app-side delete.
func TestDeleteEmailFilesTheMessageIntoTrash(t *testing.T) {
	svc, db, accountID, mailbox, _ := newImportTestService(t)
	ctx := context.Background()
	email := createTrashTestEmail(t, db, accountID, mailbox, folderInbox, "")
	ensureProtocolNamespace(t, svc, mailbox.ID)

	if err := svc.DeleteEmail(ctx, accountID, email.ID); err != nil {
		t.Fatalf("DeleteEmail() error = %v", err)
	}
	stored := loadEmailFolder(t, svc, email.ID)
	if stored.Folder != folderTrash || stored.TrashedAt == nil {
		t.Fatalf("folder = %q trashed_at = %v, want trash with a timestamp", stored.Folder, stored.TrashedAt)
	}
	if got := protocolFoldersOf(t, svc, mailbox.ID, email.ID); len(got) != 1 || got[0] != "Trash" {
		t.Fatalf("protocol folders = %v, want [Trash]", got)
	}
}

// TestMoveProtocolMessagesRecordsTheHTTPFolder covers the other direction: mail
// a client moves over IMAP has to leave the folder the app lists it in.
func TestMoveProtocolMessagesRecordsTheHTTPFolder(t *testing.T) {
	svc, db, accountID, mailbox, _ := newImportTestService(t)
	ctx := context.Background()
	email := createTrashTestEmail(t, db, accountID, mailbox, folderInbox, "")
	ensureProtocolNamespace(t, svc, mailbox.ID)

	// IMAP EXPUNGE and MOVE out of a non-Trash folder both take this path.
	if err := svc.DeleteProtocolMessages(ctx, mailbox.ID, "INBOX", []string{email.ID}); err != nil {
		t.Fatalf("DeleteProtocolMessages() error = %v", err)
	}
	stored := loadEmailFolder(t, svc, email.ID)
	if stored.Folder != folderTrash || stored.TrashedAt == nil {
		t.Fatalf("folder = %q trashed_at = %v, want trash with a timestamp", stored.Folder, stored.TrashedAt)
	}
	if got := protocolFoldersOf(t, svc, mailbox.ID, email.ID); len(got) != 1 || got[0] != "Trash" {
		t.Fatalf("protocol folders = %v, want [Trash]", got)
	}
}

// TestReceiveEmailFilesSpamIntoTheSpamFolder covers server-side filtering: the
// Spam folder an IMAP client lists must hold the mail the filter routed there.
func TestReceiveEmailFilesSpamIntoTheSpamFolder(t *testing.T) {
	svc, db, _, mailbox, _ := newImportTestService(t)
	ctx := context.Background()
	ensureProtocolNamespace(t, svc, mailbox.ID)
	// Inbox delivery reads notification settings to pick a locale for the
	// recipient; migrate the table the import harness does not need.
	if err := db.AutoMigrate(&database.AccountNotificationSettings{}); err != nil {
		t.Fatalf("AutoMigrate() error = %v", err)
	}

	spam, err := svc.ReceiveEmail(ctx, ReceiveEmailInput{
		MailboxID: mailbox.ID, FromAddress: "spammer@example.net",
		Subject: "Cheap VIAGRA", Body: "click here to claim your prize",
	})
	if err != nil {
		t.Fatalf("ReceiveEmail(spam) error = %v", err)
	}
	if spam.Folder != folderSpam {
		t.Fatalf("spam folder = %q, want %q", spam.Folder, folderSpam)
	}
	if got := protocolFoldersOf(t, svc, mailbox.ID, spam.ID); len(got) != 1 || got[0] != "Spam" {
		t.Fatalf("spam protocol folders = %v, want [Spam]", got)
	}

	ham, err := svc.ReceiveEmail(ctx, ReceiveEmailInput{
		MailboxID: mailbox.ID, FromAddress: "friend@example.net",
		Subject: "Lunch on Friday", Body: "See you at noon",
	})
	if err != nil {
		t.Fatalf("ReceiveEmail(ham) error = %v", err)
	}
	if got := protocolFoldersOf(t, svc, mailbox.ID, ham.ID); len(got) != 1 || got[0] != "INBOX" {
		t.Fatalf("ham protocol folders = %v, want [INBOX]", got)
	}
}

// TestMoveEmailFilesAMessageWithNoProtocolMembership covers a message whose
// membership is missing, as a partially migrated mailbox has: filing it still
// has to land in the named folder and carry the read state the app shows.
func TestMoveEmailFilesAMessageWithNoProtocolMembership(t *testing.T) {
	svc, db, accountID, mailbox, _ := newImportTestService(t)
	ctx := context.Background()
	email := createTrashTestEmail(t, db, accountID, mailbox, folderInbox, "")
	if err := db.Where("email_id = ?", email.ID).Delete(&database.FolderMessage{}).Error; err != nil {
		t.Fatalf("drop membership: %v", err)
	}
	if err := db.Model(&database.Email{}).Where("id = ?", email.ID).Update("is_read", true).Error; err != nil {
		t.Fatalf("mark read: %v", err)
	}

	if err := svc.MoveEmail(ctx, accountID, email.ID, folderArchive); err != nil {
		t.Fatalf("MoveEmail() error = %v", err)
	}
	if got := protocolFoldersOf(t, svc, mailbox.ID, email.ID); len(got) != 1 || got[0] != "Archive" {
		t.Fatalf("protocol folders = %v, want [Archive]", got)
	}
	var membership database.FolderMessage
	if err := db.First(&membership, "email_id = ?", email.ID).Error; err != nil {
		t.Fatalf("load membership: %v", err)
	}
	var flags []string
	if err := json.Unmarshal(membership.Flags, &flags); err != nil {
		t.Fatalf("decode flags: %v", err)
	}
	if !hasSystemFlag(flags, `\Seen`) {
		t.Fatalf("flags = %v, want \\Seen for a message the app reports as read", flags)
	}
}
