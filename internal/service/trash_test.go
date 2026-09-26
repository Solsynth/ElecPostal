package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/filesystem"
)

// fakeFileStore records the DysonFS attachment bytes the service drops.
type fakeFileStore struct {
	deleted []string
}

func (f *fakeFileStore) UploadAttachment(context.Context, filesystem.AttachmentUpload) (database.CloudFileReferenceObject, error) {
	return database.CloudFileReferenceObject{}, errors.New("not implemented")
}

func (f *fakeFileStore) OpenAttachment(context.Context, string) (filesystem.AttachmentReader, error) {
	return filesystem.AttachmentReader{}, errors.New("not implemented")
}

func (f *fakeFileStore) DeleteAttachment(_ context.Context, fileID string) error {
	f.deleted = append(f.deleted, fileID)
	return nil
}

func (f *fakeFileStore) Close() error { return nil }

// newTrashTestService builds a service with one account and mailbox, plus a
// DysonFS stand-in so attachment byte deletion is observable.
func newTrashTestService(t *testing.T) (*EmailService, *gorm.DB, *fakeFileStore, uuid.UUID, database.Mailbox) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+database.NewID()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open() error = %v", err)
	}
	if err := db.AutoMigrate(
		&database.Mailbox{}, &database.Email{}, &database.Recipient{}, &database.Attachment{},
		&database.MessageSource{}, &database.MailFolder{}, &database.FolderMessage{},
		&database.EmailLabel{}, &database.EmailLabelMapping{},
		&database.DmarcReport{}, &database.DmarcReportRecord{},
	); err != nil {
		t.Fatalf("AutoMigrate() error = %v", err)
	}
	store := &fakeFileStore{}
	svc := NewEmailService(&database.DB{DB: db}, nil)
	svc.SetWorkspaceProvider(fakeWorkspaceProvider{})
	svc.SetAttachmentByteStore(store)

	accountID := uuid.New()
	mailbox := database.Mailbox{ID: database.NewID(), AccountID: accountID, WorkspaceID: "workspace-a", Address: "ada@example.com", Name: "Ada"}
	if err := db.Create(&mailbox).Error; err != nil {
		t.Fatalf("create mailbox: %v", err)
	}
	return svc, db, store, accountID, mailbox
}

func createTrashTestEmail(t *testing.T, db *gorm.DB, accountID uuid.UUID, mailbox database.Mailbox, folder, storageKey string) database.Email {
	t.Helper()
	email := database.Email{
		ID: database.NewID(), AccountID: accountID, MailboxID: mailbox.ID,
		Subject: "Subject " + folder, Body: "body", Folder: folder,
		FromAddress: "sender@example.com", DeliveryStatus: "sent",
	}
	if err := db.Create(&email).Error; err != nil {
		t.Fatalf("create email: %v", err)
	}
	if err := db.Create(&database.Recipient{EmailID: email.ID, Address: "ada@example.com", Kind: "to"}).Error; err != nil {
		t.Fatalf("create recipient: %v", err)
	}
	if storageKey != "" {
		key := storageKey
		if err := db.Create(&database.Attachment{EmailID: email.ID, Filename: "note.txt", MimeType: "text/plain", StorageKey: &key}).Error; err != nil {
			t.Fatalf("create attachment: %v", err)
		}
	}
	if err := db.Create(&database.MessageSource{EmailID: email.ID, SHA256: "x", ReceivedAt: email.CreatedAt}).Error; err != nil {
		t.Fatalf("create message source: %v", err)
	}
	folderRow := database.MailFolder{MailboxID: mailbox.ID, Name: "INBOX", UIDValidity: 1, NextUID: 2, HighestModSeq: 7}
	if err := db.FirstOrCreate(&folderRow, database.MailFolder{MailboxID: mailbox.ID, Name: "INBOX"}).Error; err != nil {
		t.Fatalf("create folder: %v", err)
	}
	var existing int64
	if err := db.Model(&database.FolderMessage{}).Where("folder_id = ?", folderRow.ID).Count(&existing).Error; err != nil {
		t.Fatalf("count folder messages: %v", err)
	}
	if err := db.Create(&database.FolderMessage{FolderID: folderRow.ID, EmailID: email.ID, UID: uint32(existing) + 1, ModSeq: 7, Flags: []byte("[]")}).Error; err != nil {
		t.Fatalf("create folder message: %v", err)
	}
	return email
}

func TestDeleteEmailPermanentlyRemovesEveryReferencingRow(t *testing.T) {
	svc, db, store, accountID, mailbox := newTrashTestService(t)
	ctx := context.Background()
	email := createTrashTestEmail(t, db, accountID, mailbox, folderTrash, "file-1")
	label := database.EmailLabel{AccountID: accountID, Name: "urgent"}
	if err := db.Create(&label).Error; err != nil {
		t.Fatalf("create label: %v", err)
	}
	if err := db.Create(&database.EmailLabelMapping{EmailID: email.ID, LabelID: label.ID}).Error; err != nil {
		t.Fatalf("create label mapping: %v", err)
	}

	if err := svc.DeleteEmailPermanently(ctx, accountID, email.ID); err != nil {
		t.Fatalf("DeleteEmailPermanently() error = %v", err)
	}

	for name, model := range map[string]any{
		"email":          &database.Email{},
		"recipient":      &database.Recipient{},
		"attachment":     &database.Attachment{},
		"message source": &database.MessageSource{},
	} {
		var count int64
		if err := db.Model(model).Where("1 = 1").Count(&count).Error; err != nil {
			t.Fatalf("count %s: %v", name, err)
		}
		if count != 0 {
			t.Fatalf("%s rows = %d, want 0", name, count)
		}
	}
	var mappings int64
	if err := db.Model(&database.EmailLabelMapping{}).Count(&mappings).Error; err != nil {
		t.Fatalf("count label mappings: %v", err)
	}
	if mappings != 0 {
		t.Fatalf("label mappings = %d, want 0", mappings)
	}
	var memberships int64
	if err := db.Model(&database.FolderMessage{}).Count(&memberships).Error; err != nil {
		t.Fatalf("count folder messages: %v", err)
	}
	if memberships != 0 {
		t.Fatalf("folder messages = %d, want 0", memberships)
	}
	var folder database.MailFolder
	if err := db.First(&folder, "mailbox_id = ?", mailbox.ID).Error; err != nil {
		t.Fatalf("reload folder: %v", err)
	}
	if folder.HighestModSeq != 8 {
		t.Fatalf("folder highest_mod_seq = %d, want 8", folder.HighestModSeq)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "file-1" {
		t.Fatalf("deleted attachment files = %v, want [file-1]", store.deleted)
	}
	// The list must not resurrect the message through a stale cache row.
	if _, _, err := svc.ListEmails(ctx, accountID, mailbox.ID, ListInput{}); err != nil {
		t.Fatalf("ListEmails() error = %v", err)
	}
}

func TestDeleteEmailPermanentlyKeepsSharedAttachmentBytes(t *testing.T) {
	svc, db, store, accountID, mailbox := newTrashTestService(t)
	ctx := context.Background()
	first := createTrashTestEmail(t, db, accountID, mailbox, folderTrash, "file-shared")
	second := createTrashTestEmail(t, db, accountID, mailbox, folderInbox, "file-shared")

	if err := svc.DeleteEmailPermanently(ctx, accountID, first.ID); err != nil {
		t.Fatalf("DeleteEmailPermanently() error = %v", err)
	}
	if len(store.deleted) != 0 {
		t.Fatalf("deleted attachment files = %v, want none while the file is still referenced", store.deleted)
	}

	if err := svc.DeleteEmailPermanently(ctx, accountID, second.ID); err != nil {
		t.Fatalf("DeleteEmailPermanently() error = %v", err)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "file-shared" {
		t.Fatalf("deleted attachment files = %v, want [file-shared]", store.deleted)
	}
}

func TestDeleteEmailPermanentlyRejectsOtherAccountsAndMissingMessages(t *testing.T) {
	svc, db, _, accountID, mailbox := newTrashTestService(t)
	ctx := context.Background()
	email := createTrashTestEmail(t, db, accountID, mailbox, folderTrash, "")

	if err := svc.DeleteEmailPermanently(ctx, uuid.New(), email.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteEmailPermanently(other account) error = %v, want ErrNotFound", err)
	}
	var count int64
	if err := db.Model(&database.Email{}).Where("id = ?", email.ID).Count(&count).Error; err != nil {
		t.Fatalf("count emails: %v", err)
	}
	if count != 1 {
		t.Fatalf("email rows = %d, want 1", count)
	}

	if err := svc.DeleteEmailPermanently(ctx, accountID, database.NewID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteEmailPermanently(unknown id) error = %v, want ErrNotFound", err)
	}
}

func TestEmptyTrashRemovesOnlyTheMailboxTrash(t *testing.T) {
	svc, db, store, accountID, mailbox := newTrashTestService(t)
	ctx := context.Background()
	trashed := createTrashTestEmail(t, db, accountID, mailbox, folderTrash, "file-trash")
	kept := createTrashTestEmail(t, db, accountID, mailbox, folderInbox, "file-inbox")
	archived := createTrashTestEmail(t, db, accountID, mailbox, folderTrash, "")
	if err := db.Model(&database.Email{}).Where("id = ?", archived.ID).Update("archived_at", gorm.Expr("CURRENT_TIMESTAMP")).Error; err != nil {
		t.Fatalf("archive email: %v", err)
	}
	other := database.Mailbox{ID: database.NewID(), AccountID: accountID, WorkspaceID: "workspace-a", Address: "grace@example.com", Name: "Grace"}
	if err := db.Create(&other).Error; err != nil {
		t.Fatalf("create other mailbox: %v", err)
	}
	elsewhere := createTrashTestEmail(t, db, accountID, other, folderTrash, "")

	deleted, err := svc.EmptyTrash(ctx, accountID, mailbox.ID)
	if err != nil {
		t.Fatalf("EmptyTrash() error = %v", err)
	}
	if deleted != 1 {
		t.Fatalf("EmptyTrash() deleted = %d, want 1", deleted)
	}

	var remaining []database.Email
	if err := db.Unscoped().Order("id").Find(&remaining).Error; err != nil {
		t.Fatalf("load emails: %v", err)
	}
	want := map[string]bool{kept.ID: true, archived.ID: true, elsewhere.ID: true}
	if len(remaining) != len(want) {
		t.Fatalf("remaining emails = %d, want %d", len(remaining), len(want))
	}
	for _, email := range remaining {
		if !want[email.ID] {
			t.Fatalf("email %s survived EmptyTrash unexpectedly", email.ID)
		}
	}
	var trashedRows int64
	if err := db.Unscoped().Model(&database.Email{}).Where("id = ?", trashed.ID).Count(&trashedRows).Error; err != nil {
		t.Fatalf("count emptied email: %v", err)
	}
	if trashedRows != 0 {
		t.Fatalf("emptied email rows = %d, want 0", trashedRows)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "file-trash" {
		t.Fatalf("deleted attachment files = %v, want [file-trash]", store.deleted)
	}

	// Emptying the other mailbox's Trash still works and leaves the first
	// mailbox untouched.
	deleted, err = svc.EmptyTrash(ctx, accountID, other.ID)
	if err != nil {
		t.Fatalf("EmptyTrash(other) error = %v", err)
	}
	if deleted != 1 {
		t.Fatalf("EmptyTrash(other) deleted = %d, want 1", deleted)
	}
	if err := svc.DeleteEmailPermanently(ctx, accountID, elsewhere.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteEmailPermanently(emptied) error = %v, want ErrNotFound", err)
	}
}

func TestEmptyTrashRejectsMailboxesTheAccountDoesNotOwn(t *testing.T) {
	svc, _, _, _, mailbox := newTrashTestService(t)
	if _, err := svc.EmptyTrash(context.Background(), uuid.New(), mailbox.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("EmptyTrash(other account) error = %v, want ErrNotFound", err)
	}
}

func TestPurgeProtocolMessagesDropsOnlyThatFoldersMessages(t *testing.T) {
	svc, db, store, accountID, mailbox := newTrashTestService(t)
	ctx := context.Background()
	trashed := createTrashTestEmail(t, db, accountID, mailbox, folderTrash, "file-protocol")
	inbox := createTrashTestEmail(t, db, accountID, mailbox, folderInbox, "")
	var folder database.MailFolder
	if err := db.First(&folder, "mailbox_id = ? AND name = ?", mailbox.ID, "INBOX").Error; err != nil {
		t.Fatalf("load folder: %v", err)
	}
	trashFolder := database.MailFolder{MailboxID: mailbox.ID, Name: "Trash", UIDValidity: 1, NextUID: 1, HighestModSeq: 3}
	if err := db.Create(&trashFolder).Error; err != nil {
		t.Fatalf("create trash folder: %v", err)
	}
	if err := db.Create(&database.FolderMessage{FolderID: trashFolder.ID, EmailID: trashed.ID, UID: 1, ModSeq: 3, Flags: []byte("[]")}).Error; err != nil {
		t.Fatalf("link trashed message: %v", err)
	}

	// A message the folder does not hold is never removed, even when it lives
	// in the same mailbox.
	if err := svc.PurgeProtocolMessages(ctx, mailbox.ID, "Trash", []string{inbox.ID}); err != nil {
		t.Fatalf("PurgeProtocolMessages(other folder message) error = %v", err)
	}
	var survivors int64
	if err := db.Unscoped().Model(&database.Email{}).Where("id = ?", inbox.ID).Count(&survivors).Error; err != nil {
		t.Fatalf("count survivors: %v", err)
	}
	if survivors != 1 {
		t.Fatalf("inbox message rows = %d, want 1", survivors)
	}

	if err := svc.PurgeProtocolMessages(ctx, mailbox.ID, "Trash", []string{trashed.ID}); err != nil {
		t.Fatalf("PurgeProtocolMessages() error = %v", err)
	}
	var rows int64
	if err := db.Unscoped().Model(&database.Email{}).Where("id = ?", trashed.ID).Count(&rows).Error; err != nil {
		t.Fatalf("count purged message: %v", err)
	}
	if rows != 0 {
		t.Fatalf("purged message rows = %d, want 0", rows)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "file-protocol" {
		t.Fatalf("deleted attachment files = %v, want [file-protocol]", store.deleted)
	}

	// An unknown folder reports not-found instead of silently doing nothing.
	if err := svc.PurgeProtocolMessages(ctx, mailbox.ID, "Nowhere", []string{trashed.ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PurgeProtocolMessages(unknown folder) error = %v, want ErrNotFound", err)
	}
}

func TestDeleteProtocolMessagesTrashesOutsideTrashAndPurgesInsideIt(t *testing.T) {
	svc, db, store, accountID, mailbox := newTrashTestService(t)
	ctx := context.Background()
	inbox := createTrashTestEmail(t, db, accountID, mailbox, folderInbox, "")
	trashed := createTrashTestEmail(t, db, accountID, mailbox, folderTrash, "file-expunge")
	trashFolder := database.MailFolder{MailboxID: mailbox.ID, Name: "Trash", UIDValidity: 1, NextUID: 1, HighestModSeq: 1}
	if err := db.Create(&trashFolder).Error; err != nil {
		t.Fatalf("create trash folder: %v", err)
	}

	// Outside Trash a client delete only files the message into Trash.
	if err := svc.DeleteProtocolMessages(ctx, mailbox.ID, "INBOX", []string{inbox.ID}); err != nil {
		t.Fatalf("DeleteProtocolMessages(INBOX) error = %v", err)
	}
	var membership database.FolderMessage
	if err := db.First(&membership, "email_id = ?", inbox.ID).Error; err != nil {
		t.Fatalf("load membership: %v", err)
	}
	if membership.FolderID != trashFolder.ID {
		t.Fatalf("membership folder = %s, want the Trash folder %s", membership.FolderID, trashFolder.ID)
	}
	if len(store.deleted) != 0 {
		t.Fatalf("deleted attachment files = %v, want none for a trashed message", store.deleted)
	}

	// In Trash the same call is the permanent delete.
	if err := db.Create(&database.FolderMessage{FolderID: trashFolder.ID, EmailID: trashed.ID, UID: 99, ModSeq: 2, Flags: []byte("[]")}).Error; err != nil {
		t.Fatalf("link trashed message: %v", err)
	}
	if err := svc.DeleteProtocolMessages(ctx, mailbox.ID, "Trash", []string{trashed.ID}); err != nil {
		t.Fatalf("DeleteProtocolMessages(Trash) error = %v", err)
	}
	var rows int64
	if err := db.Unscoped().Model(&database.Email{}).Where("id = ?", trashed.ID).Count(&rows).Error; err != nil {
		t.Fatalf("count expunged message: %v", err)
	}
	if rows != 0 {
		t.Fatalf("expunged message rows = %d, want 0", rows)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "file-expunge" {
		t.Fatalf("deleted attachment files = %v, want [file-expunge]", store.deleted)
	}
	// An empty request never touches storage.
	if err := svc.DeleteProtocolMessages(ctx, mailbox.ID, "Trash", nil); err != nil {
		t.Fatalf("DeleteProtocolMessages(empty) error = %v", err)
	}
}
