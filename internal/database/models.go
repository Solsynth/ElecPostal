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
	WorkspaceID string         `gorm:"index:idx_mailboxes_workspace_id;size:36" json:"workspace_id"`
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
	ID          string         `gorm:"primaryKey;size:36" json:"id"`
	AccountID   uuid.UUID      `gorm:"index:idx_emails_account_id" json:"account_id"`
	MailboxID   string         `gorm:"index:idx_emails_mailbox_id;size:36" json:"mailbox_id"`
	ThreadID    *string        `gorm:"index:idx_emails_thread_id;size:36" json:"thread_id,omitempty"`
	Subject     string         `gorm:"size:512" json:"subject"`
	Body        string         `gorm:"type:text" json:"body"`
	FromAddress string         `gorm:"size:255" json:"from_address"`
	FromName    string         `gorm:"size:128" json:"from_name"`
	IsRead      bool           `json:"is_read"`
	IsStarred   bool           `json:"is_starred"`
	IsDraft     bool           `gorm:"index:idx_emails_is_draft" json:"is_draft"`
	SentAt      *time.Time     `json:"sent_at,omitempty"`
	DeletedAt   gorm.DeletedAt `gorm:"index" json:"deleted_at"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`

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
	ID          string    `gorm:"primaryKey;size:36" json:"id"`
	EmailID     string    `gorm:"index:idx_attachments_email_id;size:36" json:"email_id"`
	Filename    string    `gorm:"size:255" json:"filename"`
	MimeType    string    `gorm:"size:128" json:"mime_type"`
	Size        int64     `json:"size"`
	StorageKey  *string   `gorm:"size:128" json:"storage_key,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
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
