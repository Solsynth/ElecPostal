package service

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"src.solsynth.dev/sosys/elecpostal/internal/database"
)

// UploadMessage stores a raw RFC 5322 message into INBOX for JMAP upload
// (RFC 8620 §6). It returns the new email ID and wire size in bytes.
func (s *EmailService) UploadMessage(ctx context.Context, mailboxID string, raw []byte) (string, int64, error) {
	uid, _, err := s.AppendProtocolMessage(ctx, mailboxID, "INBOX", raw, nil, time.Now())
	if err != nil {
		return "", 0, err
	}
	// Recover the email ID from the folder membership created by AppendProtocolMessage.
	var fm database.FolderMessage
	if err := s.db.WithContext(ctx).Table("folder_messages").
		Joins("JOIN mail_folders ON mail_folders.id = folder_messages.folder_id").
		Where("mail_folders.mailbox_id = ? AND mail_folders.name = ? AND folder_messages.uid = ?", mailboxID, "INBOX", uid).
		First(&fm).Error; err != nil {
		return "", 0, err
	}
	var source database.MessageSource
	if err := s.db.WithContext(ctx).Where("email_id = ?", fm.EmailID).First(&source).Error; err != nil {
		return fm.EmailID, 0, nil
	}
	return fm.EmailID, source.WireSizeBytes, nil
}

// CopyEmail creates a full copy of sourceEmailID into destMailboxID/destFolderName.
// It copies the email row, recipients, attachments, protocol source, and folder
// membership. Returns the new email ID.
func (s *EmailService) CopyEmail(ctx context.Context, sourceEmailID, destMailboxID, destFolderName string) (string, error) {
	var source database.Email
	if err := s.db.WithContext(ctx).Preload("Recipients").Preload("Attachments").Where("id = ?", sourceEmailID).First(&source).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", ErrNotFound
		}
		return "", err
	}
	var destMailbox database.Mailbox
	if err := s.db.WithContext(ctx).Where("id = ?", destMailboxID).First(&destMailbox).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", ErrNotFound
		}
		return "", err
	}
	httpFolder := httpFolderName(destFolderName)
	newID := database.NewID()
	now := time.Now()
	txErr := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		newEmail := database.Email{
			ID: newID, AccountID: source.AccountID, MailboxID: destMailboxID,
			ThreadID: source.ThreadID, Subject: source.Subject, Body: source.Body,
			FromAddress: source.FromAddress, FromName: source.FromName,
			IsRead: source.IsRead, IsStarred: source.IsStarred,
			Folder: httpFolder, ContentType: source.ContentType,
			OmitContentType: source.OmitContentType,
			SentAt: source.SentAt, DeliveryStatus: "",
			RawSizeBytes: source.RawSizeBytes, CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.Create(&newEmail).Error; err != nil {
			return err
		}
		for _, r := range source.Recipients {
			r.ID = database.NewID()
			r.EmailID = newID
			r.CreatedAt = now
			r.UpdatedAt = now
			if err := tx.Create(&r).Error; err != nil {
				return err
			}
		}
		for _, a := range source.Attachments {
			a.ID = database.NewID()
			a.EmailID = newID
			a.CreatedAt = now
			a.UpdatedAt = now
			if err := tx.Create(&a).Error; err != nil {
				return err
			}
		}
		if err := s.storeProtocolSourceTx(tx, &newEmail, nil, ""); err != nil {
			return err
		}
		_, _, err := s.appendFolderMembershipTx(tx, destMailboxID, destFolderName, newID, nil, now)
		return err
	})
	if txErr != nil {
		return "", txErr
	}
	s.publishMailEvent(ctx, source.AccountID.String(), "mail.created", &database.Email{ID: newID, AccountID: source.AccountID})
	return newID, nil
}

// MoveEmailToFolder moves an email between protocol folders within the same mailbox.
func (s *EmailService) MoveEmailToFolder(ctx context.Context, mailboxID, fromFolder, toFolder, emailID string) error {
	return s.MoveProtocolMessages(ctx, mailboxID, fromFolder, toFolder, []string{emailID})
}
