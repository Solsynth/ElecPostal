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
	"src.solsynth.dev/sosys/elecpostal/internal/relay"
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
	db       *database.DB
	notifier NotificationSender
	files    filesystem.Uploader
	relay    relay.Adapter
	domain   string
}

// SetRelay configures outbound delivery. A nil adapter retains the existing
// persistence-only behavior for deployments without delivery enabled.
func (s *EmailService) SetRelay(adapter relay.Adapter) {
	s.relay = adapter
}

// SetDomain configures the canonical mail domain used to complete local-only
// mailbox addresses (e.g. "alice" -> "alice@example.com") for outbound relay.
func (s *EmailService) SetDomain(domain string) {
	s.domain = strings.TrimSpace(strings.ToLower(domain))
}

// NewEmailService creates a new EmailService.
func NewEmailService(db *database.DB, notifier NotificationSender) *EmailService {
	return &EmailService{db: db, notifier: notifier}
}

// MailHost returns the configured canonical mail domain, if any.
func (s *EmailService) MailHost() string {
	return s.domain
}

// normalizeFromAddress returns a full email address for the mailbox. When the
// configured domain is present and the stored address lacks one, it appends it.
func (s *EmailService) normalizeFromAddress(address string) string {
	address = strings.TrimSpace(strings.ToLower(address))
	if address == "" || s.domain == "" || strings.Contains(address, "@") {
		return address
	}
	return address + "@" + s.domain
}

// NotificationSender is the gRPC notification capability required by the
// email domain without coupling it to a particular service implementation.
type NotificationSender interface {
	SendEmailNotification(context.Context, string, string) error
	Close() error
}

// SetAttachmentUploader enables streaming attachment uploads to FileSystem.
// It is optional so deployments can continue accepting attachment IDs created
// by another trusted service.
func (s *EmailService) SetAttachmentUploader(uploader filesystem.Uploader) {
	s.files = uploader
}

// Close releases resources held by optional downstream clients.
func (s *EmailService) Close() error {
	if s.relay != nil {
		if err := s.relay.Close(); err != nil {
			return err
		}
	}
	if s.files != nil {
		if err := s.files.Close(); err != nil {
			return err
		}
	}
	if s.notifier != nil {
		if err := s.notifier.Close(); err != nil {
			return err
		}
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

	fromAddress := s.normalizeFromAddress(mailbox.Address)
	email := database.Email{
		AccountID:      accountID,
		MailboxID:      input.MailboxID,
		Subject:        input.Subject,
		Body:           input.Body,
		FromAddress:    fromAddress,
		FromName:       mailbox.Name,
		IsDraft:        input.IsDraft,
		DeliveryStatus: "draft",
	}
	if !input.IsDraft {
		email.DeliveryStatus = "pending"
		now := time.Now()
		email.LastDeliveryAttemptAt = &now
		email.DeliveryAttempts = 1
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
	if input.IsDraft {
		return &email, nil
	}
	if err := s.deliverStoredEmail(ctx, &email, outgoingRelayMessage(mailbox, fromAddress, input)); err != nil {
		return nil, err
	}

	return &email, nil
}

// ResendEmail retries an existing non-draft outbound message. The delivery
// fields on that message record are updated for each attempt.
func (s *EmailService) ResendEmail(ctx context.Context, accountID uuid.UUID, id string) (*database.Email, error) {
	var email database.Email
	if err := s.db.WithContext(ctx).Where("id = ? AND account_id = ?", id, accountID).
		Preload("Mailbox").Preload("Recipients").Preload("Attachments").First(&email).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if email.IsDraft {
		return nil, fmt.Errorf("draft emails cannot be resent")
	}
	now := time.Now()
	email.DeliveryStatus = "pending"
	email.DeliveryError = nil
	email.LastDeliveryAttemptAt = &now
	email.DeliveryAttempts++
	if err := s.db.WithContext(ctx).Model(&email).Updates(map[string]any{
		"delivery_status":          email.DeliveryStatus,
		"delivery_error":           nil,
		"last_delivery_attempt_at": now,
		"delivery_attempts":        email.DeliveryAttempts,
	}).Error; err != nil {
		return nil, err
	}
	if err := s.deliverStoredEmail(ctx, &email, relayMessageFromEmail(email)); err != nil {
		return nil, err
	}
	return &email, nil
}

func (s *EmailService) deliverStoredEmail(ctx context.Context, email *database.Email, message relay.Message) error {
	if s.relay == nil {
		return s.recordDeliveryFailure(ctx, email, "no outbound relay is configured", "not_configured")
	}
	result, err := s.relay.Send(ctx, message)
	if err != nil {
		return s.recordDeliveryFailure(ctx, email, err.Error(), "failed")
	}
	now := time.Now()
	email.DeliveryStatus = "sent"
	email.DeliveryError = nil
	email.SentAt = &now
	if result.ProviderMessageID != "" {
		email.ProviderMessageID = &result.ProviderMessageID
	}
	if err := s.db.WithContext(ctx).Model(email).Updates(map[string]any{
		"delivery_status":     email.DeliveryStatus,
		"delivery_error":      nil,
		"sent_at":             now,
		"provider_message_id": email.ProviderMessageID,
	}).Error; err != nil {
		return err
	}
	return nil
}

func (s *EmailService) recordDeliveryFailure(ctx context.Context, email *database.Email, message, status string) error {
	email.DeliveryStatus = status
	email.DeliveryError = &message
	if err := s.db.WithContext(ctx).Model(email).Updates(map[string]any{
		"delivery_status": status,
		"delivery_error":  message,
	}).Error; err != nil {
		return err
	}
	return fmt.Errorf("deliver email: %s", message)
}

func outgoingRelayMessage(mailbox database.Mailbox, fromAddress string, input SendEmailInput) relay.Message {
	message := relay.Message{
		FromAddress:   fromAddress,
		FromName:      mailbox.Name,
		Subject:       input.Subject,
		Body:          input.Body,
		AttachmentIDs: input.AttachmentIDs,
	}
	for _, recipient := range input.To {
		message.To = append(message.To, recipient.Address)
	}
	for _, recipient := range input.Cc {
		message.Cc = append(message.Cc, recipient.Address)
	}
	for _, recipient := range input.Bcc {
		message.Bcc = append(message.Bcc, recipient.Address)
	}
	return message
}

func relayMessageFromEmail(email database.Email) relay.Message {
	message := relay.Message{
		FromAddress: email.FromAddress,
		FromName:    email.FromName,
		Subject:     email.Subject,
		Body:        email.Body,
	}
	for _, recipient := range email.Recipients {
		switch recipient.Kind {
		case "cc":
			message.Cc = append(message.Cc, recipient.Address)
		case "bcc":
			message.Bcc = append(message.Bcc, recipient.Address)
		default:
			message.To = append(message.To, recipient.Address)
		}
	}
	for _, attachment := range email.Attachments {
		if attachment.StorageKey != nil {
			message.AttachmentIDs = append(message.AttachmentIDs, *attachment.StorageKey)
		}
	}
	return message
}

// DeliverLocal stores copies of an outbound message in the mailboxes selected
// by the direct SMTP adapter. The adapter calls this only after DNS confirms
// that the recipient domain's MX points at this server.
func (s *EmailService) DeliverLocal(ctx context.Context, message relay.Message, recipients []string) error {
	if len(message.AttachmentIDs) > 0 {
		return relay.ErrAttachmentSourceRequired
	}
	now := time.Now()
	delivered := make(map[string]struct{}, len(recipients))
	for _, recipient := range recipients {
		address := strings.ToLower(strings.TrimSpace(recipient))
		if _, seen := delivered[address]; seen {
			continue
		}
		delivered[address] = struct{}{}

		var mailbox database.Mailbox
		if err := s.db.WithContext(ctx).Where("LOWER(address) = ?", address).First(&mailbox).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("local recipient %q: %w", recipient, ErrNotFound)
			}
			return err
		}
		if _, err := s.ReceiveEmail(ctx, ReceiveEmailInput{
			MailboxID:   mailbox.ID,
			FromAddress: message.FromAddress,
			FromName:    message.FromName,
			Subject:     message.Subject,
			Body:        message.Body,
			To:          localRecipientInputs(message.To, "to"),
			Cc:          localRecipientInputs(message.Cc, "cc"),
			SentAt:      &now,
		}); err != nil {
			return err
		}
	}
	return nil
}

func localRecipientInputs(addresses []string, kind string) []RecipientInput {
	result := make([]RecipientInput, 0, len(addresses))
	for _, address := range addresses {
		result = append(result, RecipientInput{Address: address, Kind: kind})
	}
	return result
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
	if s.notifier != nil {
		if err := s.notifier.SendEmailNotification(ctx, mailbox.AccountID.String(), input.Subject); err != nil {
			logging.Log.Warn().Err(err).Str("account_id", mailbox.AccountID.String()).Msg("failed to send incoming email notification")
		}
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
