package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/mailtext"
)

const (
	DedupeMessageID = "message_id"
	DedupeOff       = "off"
)

const maxImportEmails = 500

// ImportEmailItem is one message to import into a mailbox's INBOX.
type ImportEmailItem struct {
	MailboxID     string           `json:"mailbox_id" binding:"required"`
	MessageID     string           `json:"message_id,omitempty"`
	FromAddress   string           `json:"from_address" binding:"required"`
	FromName      string           `json:"from_name"`
	Subject       string           `json:"subject"`
	Body          string           `json:"body"`
	ContentType   string           `json:"content_type"` // text/plain (default) or text/html
	To            []RecipientInput `json:"to"`
	Cc            []RecipientInput `json:"cc"`
	SentAt        *time.Time       `json:"sent_at,omitempty"`
	AttachmentIDs []string         `json:"attachment_ids"` // DysonFS IDs, resolved like send
}

// ImportEmailsInput is the batch import request body.
type ImportEmailsInput struct {
	Emails []ImportEmailItem `json:"emails" binding:"required,min=1,max=500"`
	Dedupe string            `json:"dedupe"` // "" or "message_id" (default), or "off"
}

// ImportItemResult reports the outcome of one item in an import batch.
type ImportItemResult struct {
	Index   int    `json:"index"`
	Status  string `json:"status"` // imported | duplicate | failed
	EmailID string `json:"email_id,omitempty"`
	Error   string `json:"error,omitempty"`
}

// ImportResult summarizes an import batch. Whole-request failures return an
// error instead; per-item failures only mark the offending item.
type ImportResult struct {
	Imported   int                `json:"imported"`
	Duplicates int                `json:"duplicates"`
	Failed     int                `json:"failed"`
	Items      []ImportItemResult `json:"items"`
}

// ImportEmails persists structured mail items into each item's mailbox INBOX.
// Dedupe is per mailbox and keyed on the client-supplied message_id, so the
// same logical email forwarded to duplicate inboxes imports to both while a
// repeated message_id in one mailbox is skipped. Items without a message_id
// are never deduped. Imports are archival: no spam routing, notifications,
// forwarding, DMARC intake, or realtime events fire.
func (s *EmailService) ImportEmails(ctx context.Context, accountID uuid.UUID, input ImportEmailsInput) (ImportResult, error) {
	mode := strings.TrimSpace(input.Dedupe)
	if mode == "" {
		mode = DedupeMessageID
	}
	if mode != DedupeMessageID && mode != DedupeOff {
		return ImportResult{}, fmt.Errorf("invalid dedupe mode %q", input.Dedupe)
	}
	if len(input.Emails) == 0 || len(input.Emails) > maxImportEmails {
		return ImportResult{}, fmt.Errorf("emails must contain 1-%d items", maxImportEmails)
	}

	result := ImportResult{}
	seen := make(map[[2]string]struct{}, len(input.Emails))
	workspaces := make(map[string]struct{})
	for index, item := range input.Emails {
		item.Subject = mailtext.ToValidUTF8(item.Subject)
		item.Body = mailtext.ToValidUTF8(item.Body)
		item.FromAddress = mailtext.ToValidUTF8(item.FromAddress)
		item.FromName = mailtext.ToValidUTF8(item.FromName)
		for i := range item.To {
			item.To[i].Address = mailtext.ToValidUTF8(item.To[i].Address)
			item.To[i].Name = mailtext.ToValidUTF8(item.To[i].Name)
		}
		for i := range item.Cc {
			item.Cc[i].Address = mailtext.ToValidUTF8(item.Cc[i].Address)
			item.Cc[i].Name = mailtext.ToValidUTF8(item.Cc[i].Name)
		}
		var messageIDPtr *string
		if mid := strings.TrimSpace(item.MessageID); mid != "" {
			messageIDPtr = &mid
		}

		itemResult := ImportItemResult{Index: index, Status: "imported"}
		recordDuplicate := func() {
			result.Duplicates++
			itemResult.Status = "duplicate"
			result.Items = append(result.Items, itemResult)
		}
		recordFailure := func(err error) {
			result.Failed++
			itemResult.Status = "failed"
			itemResult.Error = err.Error()
			result.Items = append(result.Items, itemResult)
		}

		if strings.TrimSpace(item.FromAddress) == "" {
			recordFailure(errors.New("from_address is required"))
			continue
		}
		mailbox, err := s.authorizedMailbox(ctx, accountID, item.MailboxID)
		if err != nil {
			recordFailure(err)
			continue
		}
		if mode == DedupeMessageID && messageIDPtr != nil {
			key := [2]string{mailbox.ID, *messageIDPtr}
			if _, seenBefore := seen[key]; seenBefore {
				recordDuplicate()
				continue
			}
			var count int64
			if err := s.db.WithContext(ctx).Model(&database.Email{}).Where("mailbox_id = ? AND message_id = ?", mailbox.ID, *messageIDPtr).Count(&count).Error; err != nil {
				recordFailure(err)
				continue
			}
			if count > 0 {
				recordDuplicate()
				continue
			}
			seen[key] = struct{}{}
		}
		references, err := s.resolveAttachmentReferences(ctx, *mailbox, item.AttachmentIDs)
		if err != nil {
			recordFailure(err)
			continue
		}

		threadID := database.NewID()
		email := database.Email{
			AccountID:       mailbox.AccountID,
			MailboxID:       mailbox.ID,
			MessageID:       messageIDPtr,
			ThreadID:        &threadID,
			Subject:         item.Subject,
			Body:            item.Body,
			FromAddress:     item.FromAddress,
			FromName:        item.FromName,
			SentAt:          item.SentAt,
			Folder:          folderInbox,
			ContentType:     normalizeContentType(item.ContentType),
			OmitContentType: false,
		}
		email.RawSizeBytes = incomingRawSize(email, ReceiveEmailInput{To: item.To, Cc: item.Cc})
		if len(item.To) == 0 {
			email.RawSizeBytes += rawStringSize(mailbox.Address, mailbox.Name, "to")
		}
		err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(&email).Error; err != nil {
				return err
			}
			recipients := item.To
			if len(recipients) == 0 {
				recipients = []RecipientInput{{Address: mailbox.Address, Name: mailbox.Name, Kind: "to"}}
			}
			for _, recipient := range recipients {
				if err := tx.Create(&database.Recipient{EmailID: email.ID, Address: recipient.Address, Name: recipient.Name, Kind: normalizeKind(recipient.Kind, "to")}).Error; err != nil {
					return err
				}
			}
			for _, recipient := range item.Cc {
				if err := tx.Create(&database.Recipient{EmailID: email.ID, Address: recipient.Address, Name: recipient.Name, Kind: normalizeKind(recipient.Kind, "cc")}).Error; err != nil {
					return err
				}
			}
			for position, reference := range references {
				id := reference.StorageKey
				if err := tx.Create(&database.Attachment{EmailID: email.ID, Position: position, Filename: reference.Filename, MimeType: reference.MimeType, Size: reference.Size, StorageKey: &id, File: reference.File, ContentID: reference.ContentID, Disposition: reference.Disposition}).Error; err != nil {
					return err
				}
			}
			if err := s.storeProtocolSourceTx(tx, &email, nil, ""); err != nil {
				return err
			}
			return s.addInboxMembershipTx(tx, mailbox.ID, email.ID)
		})
		if err != nil {
			// The partial unique index is a race backstop; a concurrent import
			// that won the insert surfaces here as a duplicate.
			if errors.Is(err, gorm.ErrDuplicatedKey) || strings.Contains(strings.ToLower(err.Error()), "duplicate key") {
				recordDuplicate()
				continue
			}
			recordFailure(err)
			continue
		}
		result.Imported++
		itemResult.EmailID = email.ID
		result.Items = append(result.Items, itemResult)
		workspaces[mailbox.WorkspaceID] = struct{}{}
	}

	// Quota mirrors inbound: enforced once per affected workspace after the
	// batch, and a failure returns a whole-request error. Already-imported
	// items persist; a retry with dedupe skips them.
	for workspaceID := range workspaces {
		limit, err := s.workspaceMailboxLimit(ctx, workspaceID)
		if err != nil {
			return ImportResult{}, err
		}
		if err := s.enforceWorkspaceMailboxQuota(ctx, workspaceID, limit); err != nil {
			return ImportResult{}, err
		}
	}
	return result, nil
}
