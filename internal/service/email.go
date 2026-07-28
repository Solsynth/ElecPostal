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
	"src.solsynth.dev/sosys/elecpostal/internal/workspace"
)

var (
	ErrNotFound             = errors.New("not found")
	ErrForbidden            = errors.New("forbidden")
	ErrWorkspaceUnavailable = errors.New("workspace quota service is not configured")
	ErrMailboxLimitExceeded = errors.New("workspace mailbox limit exceeded")
)

const (
	mailStorageFractionDivisor int64 = 10
	archiveRetention                 = 30 * 24 * time.Hour
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
	Offset         int
	Take           int
	Query          string
	IsRead         *bool
	IsStarred      *bool
	IsDraft        *bool
	DeliveryStatus string
	LabelID        string
}

// MailboxStats provides lightweight counts for mailbox navigation and filter badges.
type MailboxStats struct {
	Total          int64            `json:"total"`
	Unread         int64            `json:"unread"`
	Starred        int64            `json:"starred"`
	Drafts         int64            `json:"drafts"`
	DeliveryStatus map[string]int64 `json:"delivery_status"`
}

// MailboxQuota reports the active raw-email storage reserved from a workspace
// plan. Attachments are excluded because DysonFS already charges them.
type MailboxQuota struct {
	WorkspaceID    string `json:"workspace_id"`
	UsedBytes      int64  `json:"used_bytes"`
	LimitBytes     int64  `json:"limit_bytes"`
	RemainingBytes int64  `json:"remaining_bytes"`
}

// EmailService handles email-related business logic.
type EmailService struct {
	db        *database.DB
	notifier  NotificationSender
	files     filesystem.Uploader
	relay     relay.Adapter
	workspace workspace.Provider
	domain    string
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

// SetWorkspaceProvider enables workspace membership checks and derives the
// mail allowance from the workspace plan's storage quota.
func (s *EmailService) SetWorkspaceProvider(provider workspace.Provider) {
	s.workspace = provider
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
	if s.workspace != nil {
		if err := s.workspace.Close(); err != nil {
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
		if err := s.authorizeWorkspaceMember(ctx, workspaceID, accountID); err != nil {
			return nil, err
		}
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
	if err := s.authorizeWorkspaceMember(ctx, workspaceID, accountID); err != nil {
		return nil, err
	}
	if s.workspace == nil {
		return nil, ErrWorkspaceUnavailable
	}
	mailboxLimit, err := s.workspace.MailboxLimit(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("get workspace mailbox limit: %w", err)
	}

	mailbox := database.Mailbox{
		AccountID:   accountID,
		WorkspaceID: workspaceID,
		Address:     address,
		Name:        strings.TrimSpace(name),
		IsDefault:   isDefault,
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&database.Mailbox{}).Where("workspace_id = ?", workspaceID).Count(&count).Error; err != nil {
			return err
		}
		if count >= mailboxLimit {
			return fmt.Errorf("%w: limit=%d", ErrMailboxLimitExceeded, mailboxLimit)
		}
		return tx.Create(&mailbox).Error
	})
	if err != nil {
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

	query := s.emailListQuery(ctx, accountID, input)
	if strings.TrimSpace(mailboxID) != "" {
		query = query.Where("mailbox_id = ?", mailboxID)
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var items []database.Email
	if err := query.Order("created_at desc").Offset(input.Offset).Limit(input.Take).
		Preload("Recipients").Preload("Attachments").Preload("Mailbox").Preload("Labels").Find(&items).Error; err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// GetMailboxStats returns counts for an account's active messages. mailboxID
// may be empty to aggregate all mailboxes.
func (s *EmailService) GetMailboxStats(ctx context.Context, accountID uuid.UUID, mailboxID string) (MailboxStats, error) {
	base := s.db.WithContext(ctx).Model(&database.Email{}).Where("account_id = ? AND archived_at IS NULL", accountID)
	if strings.TrimSpace(mailboxID) != "" {
		base = base.Where("mailbox_id = ?", mailboxID)
	}
	stats := MailboxStats{DeliveryStatus: make(map[string]int64)}
	if err := base.Count(&stats.Total).Error; err != nil {
		return stats, err
	}
	if err := base.Where("is_read = ?", false).Count(&stats.Unread).Error; err != nil {
		return stats, err
	}
	if err := base.Where("is_starred = ?", true).Count(&stats.Starred).Error; err != nil {
		return stats, err
	}
	if err := base.Where("is_draft = ?", true).Count(&stats.Drafts).Error; err != nil {
		return stats, err
	}
	var statuses []struct {
		Status string
		Count  int64
	}
	if err := base.Select("delivery_status AS status, COUNT(*) AS count").Group("delivery_status").Scan(&statuses).Error; err != nil {
		return stats, err
	}
	for _, status := range statuses {
		stats.DeliveryStatus[status.Status] = status.Count
	}
	return stats, nil
}

func (s *EmailService) emailListQuery(ctx context.Context, accountID uuid.UUID, input ListInput) *gorm.DB {
	query := s.db.WithContext(ctx).Model(&database.Email{}).Where("emails.account_id = ? AND emails.archived_at IS NULL", accountID)
	if term := strings.TrimSpace(input.Query); term != "" {
		like := "%" + strings.ToLower(term) + "%"
		query = query.Where("(LOWER(emails.subject) LIKE ? OR LOWER(emails.body) LIKE ? OR LOWER(emails.from_address) LIKE ? OR LOWER(emails.from_name) LIKE ? OR EXISTS (SELECT 1 FROM recipients WHERE recipients.email_id = emails.id AND (LOWER(recipients.address) LIKE ? OR LOWER(recipients.name) LIKE ?)))", like, like, like, like, like, like)
	}
	if input.IsRead != nil {
		query = query.Where("emails.is_read = ?", *input.IsRead)
	}
	if input.IsStarred != nil {
		query = query.Where("emails.is_starred = ?", *input.IsStarred)
	}
	if input.IsDraft != nil {
		query = query.Where("emails.is_draft = ?", *input.IsDraft)
	}
	if status := strings.TrimSpace(input.DeliveryStatus); status != "" {
		query = query.Where("emails.delivery_status = ?", status)
	}
	if labelID := strings.TrimSpace(input.LabelID); labelID != "" {
		query = query.Where("EXISTS (SELECT 1 FROM email_label_mappings WHERE email_label_mappings.email_id = emails.id AND email_label_mappings.label_id = ?)", labelID)
	}
	return query
}

// ListLabels returns the account's tags in a stable name order.
func (s *EmailService) ListLabels(ctx context.Context, accountID uuid.UUID) ([]database.EmailLabel, error) {
	var labels []database.EmailLabel
	err := s.db.WithContext(ctx).Where("account_id = ?", accountID).Order("name ASC").Find(&labels).Error
	return labels, err
}

// CreateLabel creates an account-owned tag.
func (s *EmailService) CreateLabel(ctx context.Context, accountID uuid.UUID, name, color string) (*database.EmailLabel, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("label name is required")
	}
	if len(name) > 128 {
		return nil, fmt.Errorf("label name must be at most 128 characters")
	}
	label := database.EmailLabel{AccountID: accountID, Name: name, Color: strings.TrimSpace(color)}
	if err := s.db.WithContext(ctx).Create(&label).Error; err != nil {
		return nil, err
	}
	return &label, nil
}

// DeleteLabel removes an account-owned tag and all of its mappings.
func (s *EmailService) DeleteLabel(ctx context.Context, accountID uuid.UUID, labelID string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var label database.EmailLabel
		if err := tx.Where("id = ? AND account_id = ?", labelID, accountID).First(&label).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if err := tx.Where("label_id = ?", label.ID).Delete(&database.EmailLabelMapping{}).Error; err != nil {
			return err
		}
		return tx.Delete(&label).Error
	})
}

// SetEmailLabel adds or removes a tag from an account-owned email.
func (s *EmailService) SetEmailLabel(ctx context.Context, accountID uuid.UUID, emailID, labelID string, assigned bool) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&database.Email{}).Where("id = ? AND account_id = ?", emailID, accountID).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return ErrNotFound
		}
		if err := tx.Model(&database.EmailLabel{}).Where("id = ? AND account_id = ?", labelID, accountID).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return ErrNotFound
		}
		if assigned {
			return tx.Where("email_id = ? AND label_id = ?", emailID, labelID).FirstOrCreate(&database.EmailLabelMapping{EmailID: emailID, LabelID: labelID}).Error
		}
		return tx.Where("email_id = ? AND label_id = ?", emailID, labelID).Delete(&database.EmailLabelMapping{}).Error
	})
}

// GetEmail returns a single email belonging to the account.
func (s *EmailService) GetEmail(ctx context.Context, accountID uuid.UUID, id string) (*database.Email, error) {
	var email database.Email
	if err := s.db.WithContext(ctx).Where("id = ? AND account_id = ?", id, accountID).
		Preload("Recipients").Preload("Attachments").Preload("Mailbox").Preload("Labels").First(&email).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &email, nil
}

// GetMailboxQuota returns the 10% raw-email allocation and its current use
// for the mailbox's workspace.
func (s *EmailService) GetMailboxQuota(ctx context.Context, accountID uuid.UUID, mailboxID string) (MailboxQuota, error) {
	var mailbox database.Mailbox
	if err := s.db.WithContext(ctx).Where("id = ? AND account_id = ?", mailboxID, accountID).First(&mailbox).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return MailboxQuota{}, ErrNotFound
		}
		return MailboxQuota{}, err
	}
	if err := s.authorizeWorkspaceMember(ctx, mailbox.WorkspaceID, accountID); err != nil {
		return MailboxQuota{}, err
	}
	limit, used, err := s.workspaceMailboxUsage(ctx, mailbox.WorkspaceID)
	if err != nil {
		return MailboxQuota{}, err
	}
	remaining := limit - used
	if remaining < 0 {
		remaining = 0
	}
	return MailboxQuota{WorkspaceID: mailbox.WorkspaceID, UsedBytes: used, LimitBytes: limit, RemainingBytes: remaining}, nil
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
	if err := s.authorizeWorkspaceMember(ctx, mailbox.WorkspaceID, accountID); err != nil {
		return nil, err
	}
	mailboxLimit, err := s.workspaceMailboxLimit(ctx, mailbox.WorkspaceID)
	if err != nil {
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
	email.RawSizeBytes = outgoingRawSize(email, input)
	if !input.IsDraft {
		email.DeliveryStatus = "pending"
		now := time.Now()
		email.LastDeliveryAttemptAt = &now
		email.DeliveryAttempts = 1
	}

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
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
	if err := s.enforceWorkspaceMailboxQuota(ctx, mailbox.WorkspaceID, mailboxLimit); err != nil {
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
	logging.Log.Info().
		Str("email_id", email.ID).
		Int("attempt", email.DeliveryAttempts).
		Int("recipient_count", len(message.To)+len(message.Cc)+len(message.Bcc)).
		Msg("delivering email")
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
	logging.Log.Info().
		Str("email_id", email.ID).
		Int("attempt", email.DeliveryAttempts).
		Str("provider_message_id", result.ProviderMessageID).
		Msg("email delivered")
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
	logging.Log.Warn().
		Str("email_id", email.ID).
		Int("attempt", email.DeliveryAttempts).
		Str("status", status).
		Str("error", message).
		Msg("email delivery failed")
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
	mailboxLimit, err := s.workspaceMailboxLimit(ctx, mailbox.WorkspaceID)
	if err != nil {
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
	email.RawSizeBytes = incomingRawSize(email, input)
	if len(input.To) == 0 {
		email.RawSizeBytes += rawStringSize(mailbox.Address, mailbox.Name, "to")
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
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
	if err := s.enforceWorkspaceMailboxQuota(ctx, mailbox.WorkspaceID, mailboxLimit); err != nil {
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

// PurgeArchivedEmails permanently removes raw email records whose 30-day
// archive retention window has elapsed. Attachment contents remain managed by
// DysonFS and are intentionally not included in mailbox storage accounting.
func (s *EmailService) PurgeArchivedEmails(ctx context.Context) (int64, error) {
	var emails []database.Email
	if err := s.db.WithContext(ctx).Where("archive_delete_at IS NOT NULL AND archive_delete_at <= ?", time.Now()).Find(&emails).Error; err != nil {
		return 0, err
	}
	var purged int64
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, email := range emails {
			if err := tx.Where("email_id = ?", email.ID).Delete(&database.Recipient{}).Error; err != nil {
				return err
			}
			if err := tx.Where("email_id = ?", email.ID).Delete(&database.Attachment{}).Error; err != nil {
				return err
			}
			if err := tx.Where("email_id = ?", email.ID).Delete(&database.EmailLabelMapping{}).Error; err != nil {
				return err
			}
			result := tx.Unscoped().Delete(&database.Email{}, "id = ?", email.ID)
			if result.Error != nil {
				return result.Error
			}
			purged += result.RowsAffected
		}
		return nil
	})
	return purged, err
}

func (s *EmailService) authorizeWorkspaceMember(ctx context.Context, workspaceID string, accountID uuid.UUID) error {
	if s.workspace == nil {
		return ErrWorkspaceUnavailable
	}
	if err := s.workspace.AuthorizeMember(ctx, workspaceID, accountID.String()); err != nil {
		return fmt.Errorf("authorize workspace mailbox: %w", err)
	}
	return nil
}

// enforceWorkspaceMailboxQuota retains the newest messages in a workspace.
// Archived messages are excluded from the active mailbox budget and receive a
// fixed 30-day deletion deadline.
func (s *EmailService) enforceWorkspaceMailboxQuota(ctx context.Context, workspaceID string, limit int64) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var used int64
		if err := tx.Model(&database.Email{}).
			Select("COALESCE(SUM(emails.raw_size_bytes), 0)").
			Joins("JOIN mailboxes ON mailboxes.id = emails.mailbox_id AND mailboxes.deleted_at IS NULL").
			Where("mailboxes.workspace_id = ? AND emails.archived_at IS NULL", workspaceID).
			Scan(&used).Error; err != nil {
			return fmt.Errorf("calculate workspace mail usage: %w", err)
		}
		deadline := time.Now().Add(archiveRetention)
		for used > limit {
			var oldest database.Email
			if err := tx.Joins("JOIN mailboxes ON mailboxes.id = emails.mailbox_id AND mailboxes.deleted_at IS NULL").
				Where("mailboxes.workspace_id = ? AND emails.archived_at IS NULL", workspaceID).
				Order("emails.created_at ASC, emails.id ASC").First(&oldest).Error; err != nil {
				return fmt.Errorf("find email to archive: %w", err)
			}
			now := time.Now()
			if err := tx.Model(&oldest).Updates(map[string]any{"archived_at": now, "archive_delete_at": deadline}).Error; err != nil {
				return fmt.Errorf("archive email: %w", err)
			}
			used -= oldest.RawSizeBytes
		}
		return nil
	})
}

func (s *EmailService) workspaceMailboxUsage(ctx context.Context, workspaceID string) (limit, used int64, err error) {
	limit, err = s.workspaceMailboxLimit(ctx, workspaceID)
	if err != nil {
		return 0, 0, err
	}
	err = s.db.WithContext(ctx).Model(&database.Email{}).
		Select("COALESCE(SUM(emails.raw_size_bytes), 0)").
		Joins("JOIN mailboxes ON mailboxes.id = emails.mailbox_id AND mailboxes.deleted_at IS NULL").
		Where("mailboxes.workspace_id = ? AND emails.archived_at IS NULL", workspaceID).
		Scan(&used).Error
	if err != nil {
		return 0, 0, fmt.Errorf("calculate workspace mail usage: %w", err)
	}
	return limit, used, nil
}

func (s *EmailService) workspaceMailboxLimit(ctx context.Context, workspaceID string) (int64, error) {
	if s.workspace == nil {
		return 0, ErrWorkspaceUnavailable
	}
	totalBytes, err := s.workspace.PlanStorageBytes(ctx, workspaceID)
	if err != nil {
		return 0, fmt.Errorf("get workspace mail quota: %w", err)
	}
	limit := totalBytes / mailStorageFractionDivisor
	if limit <= 0 {
		return 0, fmt.Errorf("workspace mail quota is zero")
	}
	return limit, nil
}

func outgoingRawSize(email database.Email, input SendEmailInput) int64 {
	size := rawStringSize(email.Subject, email.Body, email.FromAddress, email.FromName)
	for _, recipients := range [][]RecipientInput{input.To, input.Cc, input.Bcc} {
		for _, recipient := range recipients {
			size += rawStringSize(recipient.Address, recipient.Name, normalizeKind(recipient.Kind, "to"))
		}
	}
	return size
}

func incomingRawSize(email database.Email, input ReceiveEmailInput) int64 {
	size := rawStringSize(email.Subject, email.Body, email.FromAddress, email.FromName)
	for _, recipients := range [][]RecipientInput{input.To, input.Cc} {
		for _, recipient := range recipients {
			size += rawStringSize(recipient.Address, recipient.Name, normalizeKind(recipient.Kind, "to"))
		}
	}
	return size
}

func rawStringSize(values ...string) int64 {
	var size int64
	for _, value := range values {
		size += int64(len(value))
	}
	return size
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

// MarkStarred toggles the flag of an email.
func (s *EmailService) MarkStarred(ctx context.Context, accountID uuid.UUID, id string, isStarred bool) error {
	result := s.db.WithContext(ctx).Model(&database.Email{}).Where("id = ? AND account_id = ?", id, accountID).Update("is_starred", isStarred)
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
