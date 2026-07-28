package database

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// Mailbox represents a user's email address within ElecPostal.
// Each mailbox is linked to a DysonNetwork workspace. For personal mailboxes
// this is the user's individual workspace.
type Mailbox struct {
	ID          string         `gorm:"primaryKey;size:36" json:"id"`
	AccountID   uuid.UUID      `gorm:"index:idx_mailboxes_account_id" json:"account_id"`
	WorkspaceID string         `gorm:"not null;check:chk_mailboxes_workspace_id,workspace_id <> '';index:idx_mailboxes_workspace_id;size:36" json:"workspace_id"`
	Address     string         `gorm:"uniqueIndex;size:255" json:"address"`
	Name        string         `gorm:"size:128" json:"name"`
	IsDefault   bool           `gorm:"index:idx_mailboxes_account_default" json:"is_default"`
	IsVerified  bool           `json:"is_verified"`
	Config      datatypes.JSON `gorm:"type:jsonb" json:"config"`
	DeletedAt   gorm.DeletedAt `gorm:"index" json:"deleted_at"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

func (m *Mailbox) BeforeCreate(tx *gorm.DB) error {
	if m.ID == "" {
		m.ID = NewID()
	}
	return nil
}

// Email stores a single email message.
type Email struct {
	ID                    string     `gorm:"primaryKey;size:36" json:"id"`
	AccountID             uuid.UUID  `gorm:"index:idx_emails_account_id" json:"account_id"`
	MailboxID             string     `gorm:"index:idx_emails_mailbox_id;size:36" json:"mailbox_id"`
	ThreadID              *string    `gorm:"index:idx_emails_thread_id;size:36" json:"thread_id,omitempty"`
	Subject               string     `gorm:"size:512" json:"subject"`
	Body                  string     `gorm:"type:text" json:"body"`
	FromAddress           string     `gorm:"size:255" json:"from_address"`
	FromName              string     `gorm:"size:128" json:"from_name"`
	IsRead                bool       `json:"is_read"`
	IsStarred             bool       `json:"is_starred"`
	IsDraft               bool       `gorm:"index:idx_emails_is_draft" json:"is_draft"`
	Folder                string     `gorm:"index:idx_emails_folder;size:16" json:"folder"`
	ContentType           string     `gorm:"size:32" json:"content_type"`
	ScheduledAt           *time.Time `gorm:"index" json:"scheduled_at,omitempty"`
	TrashedAt             *time.Time `gorm:"index" json:"trashed_at,omitempty"`
	SpamAt                *time.Time `gorm:"index" json:"spam_at,omitempty"`
	SentAt                *time.Time `json:"sent_at,omitempty"`
	DeliveryStatus        string     `gorm:"index;size:32" json:"delivery_status"`
	DeliveryAttempts      int        `json:"delivery_attempts"`
	LastDeliveryAttemptAt *time.Time `json:"last_delivery_attempt_at,omitempty"`
	DeliveryError         *string    `gorm:"type:text" json:"delivery_error,omitempty"`
	ProviderMessageID     *string    `gorm:"size:255" json:"provider_message_id,omitempty"`
	// RawSizeBytes is the byte size of the message data kept in ElecPostal's
	// database. Attachment content is stored and accounted for by DysonFS, so
	// it is deliberately excluded from this value.
	RawSizeBytes    int64          `gorm:"index" json:"raw_size_bytes"`
	ArchivedAt      *time.Time     `gorm:"index" json:"archived_at,omitempty"`
	ArchiveDeleteAt *time.Time     `gorm:"index" json:"archive_delete_at,omitempty"`
	DeletedAt       gorm.DeletedAt `gorm:"index" json:"deleted_at"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`

	Mailbox     *Mailbox     `gorm:"foreignKey:MailboxID;references:ID" json:"mailbox,omitempty"`
	Recipients  []Recipient  `gorm:"foreignKey:EmailID;references:ID" json:"recipients,omitempty"`
	Attachments []Attachment `gorm:"foreignKey:EmailID;references:ID" json:"attachments,omitempty"`
	Labels      []EmailLabel `gorm:"many2many:email_label_mappings;" json:"labels,omitempty"`
}

func (e *Email) BeforeCreate(tx *gorm.DB) error {
	if e.ID == "" {
		e.ID = NewID()
	}
	return nil
}

// Recipient stores one recipient of an email.
type Recipient struct {
	ID        string    `gorm:"primaryKey;size:36" json:"id"`
	EmailID   string    `gorm:"index:idx_recipients_email_id;size:36" json:"email_id"`
	Address   string    `gorm:"size:255" json:"address"`
	Name      string    `gorm:"size:128" json:"name"`
	Kind      string    `gorm:"size:16" json:"kind"` // to, cc, bcc
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (r *Recipient) BeforeCreate(tx *gorm.DB) error {
	if r.ID == "" {
		r.ID = NewID()
	}
	return nil
}

// Attachment stores metadata for an email attachment.
type Attachment struct {
	ID         string    `gorm:"primaryKey;size:36" json:"id"`
	EmailID    string    `gorm:"index:idx_attachments_email_id;size:36" json:"email_id"`
	Filename   string    `gorm:"size:255" json:"filename"`
	MimeType   string    `gorm:"size:128" json:"mime_type"`
	Size       int64     `json:"size"`
	StorageKey *string   `gorm:"size:128" json:"storage_key,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// MailProtocolCredential is a revocable app password for mail protocols. Its
// secret is stored only as a bcrypt hash and is never usable for HTTP APIs.
type MailProtocolCredential struct {
	ID        string         `gorm:"primaryKey;size:36" json:"id"`
	AccountID uuid.UUID      `gorm:"index:idx_mail_protocol_credentials_account_id" json:"account_id"`
	Label     string         `gorm:"size:128" json:"label"`
	Hash      string         `gorm:"size:255" json:"-"`
	Protocols datatypes.JSON `gorm:"type:jsonb" json:"protocols"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"deleted_at"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

func (c *MailProtocolCredential) BeforeCreate(tx *gorm.DB) error {
	if c.ID == "" {
		c.ID = NewID()
	}
	return nil
}

func (a *Attachment) BeforeCreate(tx *gorm.DB) error {
	if a.ID == "" {
		a.ID = NewID()
	}
	return nil
}

// EmailLabel is a user-defined label for organizing emails.
type EmailLabel struct {
	ID        string         `gorm:"primaryKey;size:36" json:"id"`
	AccountID uuid.UUID      `gorm:"index:idx_labels_account_id" json:"account_id"`
	Name      string         `gorm:"size:128" json:"name"`
	Color     string         `gorm:"size:32" json:"color"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"deleted_at"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

func (l *EmailLabel) BeforeCreate(tx *gorm.DB) error {
	if l.ID == "" {
		l.ID = NewID()
	}
	return nil
}

// EmailLabelMapping is the join table between emails and labels.
type EmailLabelMapping struct {
	EmailID string `gorm:"primaryKey;size:36" json:"email_id"`
	LabelID string `gorm:"primaryKey;size:36" json:"label_id"`
}

// MailBlockRule prevents a sender or domain from reaching a mailbox or every
// mailbox in a workspace. Matching mail is retained in Spam for review.
type MailBlockRule struct {
	ID          string         `gorm:"primaryKey;size:36" json:"id"`
	AccountID   uuid.UUID      `gorm:"index:idx_mail_block_rules_account_id" json:"account_id"`
	WorkspaceID *string        `gorm:"index;size:36" json:"workspace_id,omitempty"`
	MailboxID   *string        `gorm:"index;size:36" json:"mailbox_id,omitempty"`
	Pattern     string         `gorm:"size:255" json:"pattern"`
	MatchType   string         `gorm:"size:16" json:"match_type"` // address or domain
	DeletedAt   gorm.DeletedAt `gorm:"index" json:"deleted_at"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

func (r *MailBlockRule) BeforeCreate(tx *gorm.DB) error {
	if r.ID == "" {
		r.ID = NewID()
	}
	return nil
}
