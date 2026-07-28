package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/filesystem"
	"src.solsynth.dev/sosys/elecpostal/internal/logging"
	"src.solsynth.dev/sosys/elecpostal/internal/solar"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("forbidden")
)

// RecipientInput is a recipient for a new email.
type RecipientInput struct {
	Address string `json:"address" binding:"required"`
	Name    string `json:"name"`
	Kind    string `json:"kind"` // to, cc, bcc
}

// AttachmentInput is an attachment for a new email.
type AttachmentInput struct {
	Filename   string  `json:"filename" binding:"required"`
	MimeType   string  `json:"mime_type"`
	Size       int64   `json:"size"`
	StorageKey *string `json:"storage_key,omitempty"`
}

// SendEmailInput is the payload for sending an email.
type SendEmailInput struct {
	MailboxID     string           `json:"mailbox_id" binding:"required"`
	To            []RecipientInput `json:"to" binding:"required,min=1"`
	Cc            []RecipientInput `json:"cc"`
	Bcc           []RecipientInput `json:"bcc"`
	Subject       string           `json:"subject"`
	Body          string           `json:"body"`
	AttachmentIDs []string         `json:"attachment_ids"`
	IsDraft       bool             `json:"is_draft"`
}

// IncomingAttachment contains the raw content delivered by another mail
// service. ElecPostal owns the subsequent FileSystem upload.
type IncomingAttachment struct {
	Filename string
	MimeType string
	Size     int64
	Content  io.Reader
}

// ReceiveEmailInput is the trusted interservice payload for an inbound email.
// MailboxID identifies the local destination; ownership is never supplied by
// the caller and is always resolved from that mailbox.
type ReceiveEmailInput struct {
	MailboxID   string
	FromAddress string
	FromName    string
	Subject     string
	Body        string
	To          []RecipientInput
	Cc          []RecipientInput
	Attachments []IncomingAttachment
	SentAt      *time.Time
}

// ListInput is pagination for list endpoints.
type ListInput struct {
	Offset int
	Take   int
}

// EmailService handles email-related business logic.
type EmailService struct {
	db    *database.DB
	solar *solar.Client
	files filesystem.Uploader
}

// NewEmailService creates a new EmailService.
func NewEmailService(db *database.DB, solarClient *solar.Client) *EmailService {
	return &EmailService{db: db, solar: solarClient}
}

// SetAttachmentUploader enables streaming attachment uploads to FileSystem.
// It is optional so deployments can continue accepting attachment IDs created
// by another trusted service.
func (s *EmailService) SetAttachmentUploader(uploader filesystem.Uploader) {
	s.files = uploader
}

// Close releases resources held by optional downstream clients.
func (s *EmailService) Close() error {
	if s.files != nil {
		return s.files.Close()
	}
	return nil
}

// DB returns the underlying database handle.
func (s *EmailService) DB() *database.DB {
	return s.db
}

// ListMailboxes returns all mailboxes for an account. When a workspace ID is
// provided, it filters to that workspace; otherwise it returns mailboxes owned
// by the account (typically the individual workspace).
func (s *EmailService) ListMailboxes(ctx context.Context, accountID uuid.UUID, workspaceID string) ([]database.Mailbox, error) {
	var items []database.Mailbox
	query := s.db.WithContext(ctx).Order("created_at desc")
	if strings.TrimSpace(workspaceID) != "" {
		query = query.Where("workspace_id = ?", workspaceID)
	} else {
		query = query.Where("account_id = ?", accountID)
	}
	if err := query.Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

// CreateMailbox creates a new mailbox for an account/workspace.
func (s *EmailService) CreateMailbox(ctx context.Context, accountID uuid.UUID, workspaceID, address, name string, isDefault bool) (*database.Mailbox, error) {
	address = strings.TrimSpace(strings.ToLower(address))
	if address == "" {
		return nil, fmt.Errorf("address is required")
	}
	if strings.TrimSpace(workspaceID) == "" {
		return nil, fmt.Errorf("workspace_id is required")
	}

	mailbox := database.Mailbox{
		AccountID:   accountID,
		WorkspaceID: workspaceID,
		Address:     address,
		Name:        strings.TrimSpace(name),
		IsDefault:   isDefault,
	}
	if err := s.db.WithContext(ctx).Create(&mailbox).Error; err != nil {
		return nil, err
	}
	return &mailbox, nil
}

// ListEmails returns emails for an account with optional mailbox filter.
func (s *EmailService) ListEmails(ctx context.Context, accountID uuid.UUID, mailboxID string, input ListInput) ([]database.Email, int64, error) {
	if input.Take <= 0 {
		input.Take = 20
	}
	if input.Take > 200 {
		input.Take = 200
	}
	if input.Offset < 0 {
		input.Offset = 0
	}

	query := s.db.WithContext(ctx).Model(&database.Email{}).Where("account_id = ?", accountID)
	if strings.TrimSpace(mailboxID) != "" {
		query = query.Where("mailbox_id = ?", mailboxID)
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var items []database.Email
	if err := query.Order("created_at desc").Offset(input.Offset).Limit(input.Take).
		Preload("Recipients").Preload("Attachments").Preload("Mailbox").Find(&items).Error; err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// GetEmail returns a single email belonging to the account.
func (s *EmailService) GetEmail(ctx context.Context, accountID uuid.UUID, id string) (*database.Email, error) {
	var email database.Email
	if err := s.db.WithContext(ctx).Where("id = ? AND account_id = ?", id, accountID).
		Preload("Recipients").Preload("Attachments").Preload("Mailbox").First(&email).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &email, nil
}

// SendEmail creates and sends an email.
func (s *EmailService) SendEmail(ctx context.Context, accountID uuid.UUID, input SendEmailInput) (*database.Email, error) {
	// Verify mailbox ownership.
	var mailbox database.Mailbox
	if err := s.db.WithContext(ctx).Where("id = ? AND account_id = ?", input.MailboxID, accountID).First(&mailbox).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	email := database.Email{
		AccountID:   accountID,
		MailboxID:   input.MailboxID,
		Subject:     input.Subject,
		Body:        input.Body,
		FromAddress: mailbox.Address,
		FromName:    mailbox.Name,
		IsDraft:     input.IsDraft,
	}
	if !input.IsDraft {
		now := time.Now()
		email.SentAt = &now
	}

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&email).Error; err != nil {
			return err
		}
		for _, r := range input.To {
			if err := tx.Create(&database.Recipient{EmailID: email.ID, Address: r.Address, Name: r.Name, Kind: normalizeKind(r.Kind, "to")}).Error; err != nil {
				return err
			}
		}
		for _, r := range input.Cc {
			if err := tx.Create(&database.Recipient{EmailID: email.ID, Address: r.Address, Name: r.Name, Kind: normalizeKind(r.Kind, "cc")}).Error; err != nil {
				return err
			}
		}
		for _, r := range input.Bcc {
			if err := tx.Create(&database.Recipient{EmailID: email.ID, Address: r.Address, Name: r.Name, Kind: normalizeKind(r.Kind, "bcc")}).Error; err != nil {
				return err
			}
		}
		for _, attachmentID := range input.AttachmentIDs {
			attachmentID = strings.TrimSpace(attachmentID)
			if attachmentID == "" {
				return fmt.Errorf("attachment_ids cannot contain empty values")
			}
			if err := tx.Create(&database.Attachment{EmailID: email.ID, StorageKey: &attachmentID}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Notify the account via Solar Network when an email is sent.
	if s.solar != nil && s.solar.Enabled() && !input.IsDraft {
		if _, err := s.solar.SendDirectMessage(ctx, accountID.String(), fmt.Sprintf("New email: %s", input.Subject)); err != nil {
			logging.Log.Warn().Err(err).Str("account_id", accountID.String()).Msg("failed to send solar notification")
		}
	}

	return &email, nil
}

// ReceiveEmail stores a message received from another mail service. Raw
// attachments are uploaded under the destination mailbox owner and workspace,
// whereas outgoing messages retain the client-provided DysonFS IDs.
func (s *EmailService) ReceiveEmail(ctx context.Context, input ReceiveEmailInput) (*database.Email, error) {
	if strings.TrimSpace(input.MailboxID) == "" {
		return nil, fmt.Errorf("mailbox_id is required")
	}
	if strings.TrimSpace(input.FromAddress) == "" {
		return nil, fmt.Errorf("from_address is required")
	}
	var mailbox database.Mailbox
	if err := s.db.WithContext(ctx).Where("id = ?", input.MailboxID).First(&mailbox).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	attachments := make([]AttachmentInput, 0, len(input.Attachments))
	for _, attachment := range input.Attachments {
		stored, err := s.storeIncomingAttachment(ctx, mailbox, attachment)
		if err != nil {
			return nil, err
		}
		attachments = append(attachments, stored)
	}

	email := database.Email{
		AccountID:   mailbox.AccountID,
		MailboxID:   mailbox.ID,
		Subject:     input.Subject,
		Body:        input.Body,
		FromAddress: input.FromAddress,
		FromName:    input.FromName,
		SentAt:      input.SentAt,
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&email).Error; err != nil {
			return err
		}
		recipients := input.To
		if len(recipients) == 0 {
			recipients = []RecipientInput{{Address: mailbox.Address, Name: mailbox.Name, Kind: "to"}}
		}
		for _, recipient := range recipients {
			if err := tx.Create(&database.Recipient{EmailID: email.ID, Address: recipient.Address, Name: recipient.Name, Kind: normalizeKind(recipient.Kind, "to")}).Error; err != nil {
				return err
			}
		}
		for _, recipient := range input.Cc {
			if err := tx.Create(&database.Recipient{EmailID: email.ID, Address: recipient.Address, Name: recipient.Name, Kind: normalizeKind(recipient.Kind, "cc")}).Error; err != nil {
				return err
			}
		}
		for _, attachment := range attachments {
			if err := tx.Create(&database.Attachment{EmailID: email.ID, Filename: attachment.Filename, MimeType: attachment.MimeType, Size: attachment.Size, StorageKey: attachment.StorageKey}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &email, nil
}

func (s *EmailService) storeIncomingAttachment(ctx context.Context, mailbox database.Mailbox, attachment IncomingAttachment) (AttachmentInput, error) {
	if s.files == nil {
		return AttachmentInput{}, fmt.Errorf("attachment uploads are not configured")
	}
	fileID, err := s.files.UploadAttachment(ctx, filesystem.AttachmentUpload{
		AccountID:   mailbox.AccountID,
		WorkspaceID: mailbox.WorkspaceID,
		Filename:    attachment.Filename,
		MimeType:    attachment.MimeType,
		Size:        attachment.Size,
		Content:     attachment.Content,
	})
	if err != nil {
		return AttachmentInput{}, err
	}
	return AttachmentInput{Filename: attachment.Filename, MimeType: attachment.MimeType, Size: attachment.Size, StorageKey: &fileID}, nil
}

// DeleteEmail soft-deletes an email owned by the account.
func (s *EmailService) DeleteEmail(ctx context.Context, accountID uuid.UUID, id string) error {
	result := s.db.WithContext(ctx).Where("id = ? AND account_id = ?", id, accountID).Delete(&database.Email{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkRead toggles the read flag of an email.
func (s *EmailService) MarkRead(ctx context.Context, accountID uuid.UUID, id string, isRead bool) error {
	result := s.db.WithContext(ctx).Model(&database.Email{}).Where("id = ? AND account_id = ?", id, accountID).Update("is_read", isRead)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func normalizeKind(kind, fallback string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "to", "cc", "bcc":
		return strings.ToLower(strings.TrimSpace(kind))
	default:
		return fallback
	}
}
