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
	ID                    string         `gorm:"primaryKey;size:36" json:"id"`
	AccountID             uuid.UUID      `gorm:"index:idx_emails_account_id" json:"account_id"`
	MailboxID             string         `gorm:"index:idx_emails_mailbox_id;size:36" json:"mailbox_id"`
	ThreadID              *string        `gorm:"index:idx_emails_thread_id;size:36" json:"thread_id,omitempty"`
	Subject               string         `gorm:"size:512" json:"subject"`
	Body                  string         `gorm:"type:text" json:"body"`
	FromAddress           string         `gorm:"size:255" json:"from_address"`
	FromName              string         `gorm:"size:128" json:"from_name"`
	IsRead                bool           `json:"is_read"`
	IsStarred             bool           `json:"is_starred"`
	IsDraft               bool           `gorm:"index:idx_emails_is_draft" json:"is_draft"`
	Folder                string         `gorm:"index:idx_emails_folder;size:16" json:"folder"`
	ContentType           string         `gorm:"size:32" json:"content_type"`
	ScheduledAt           *time.Time     `gorm:"index" json:"scheduled_at,omitempty"`
	TrashedAt             *time.Time     `gorm:"index" json:"trashed_at,omitempty"`
	SpamAt                *time.Time     `gorm:"index" json:"spam_at,omitempty"`
	SentAt                *time.Time     `json:"sent_at,omitempty"`
	DeliveryStatus        string         `gorm:"index;size:32" json:"delivery_status"`
	DeliveryAttempts      int            `json:"delivery_attempts"`
	LastDeliveryAttemptAt *time.Time     `json:"last_delivery_attempt_at,omitempty"`
	DeliveryError         *string        `gorm:"type:text" json:"delivery_error,omitempty"`
	ProviderMessageID     *string        `gorm:"size:255" json:"provider_message_id,omitempty"`
	Authentication        datatypes.JSON `gorm:"type:jsonb" json:"authentication,omitempty"`
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
	ID string `gorm:"primaryKey;size:36" json:"id"`
	// MailboxID is the protocol security boundary.  A credential is never
	// usable for another address owned by the same account.
	MailboxID string `gorm:"index:idx_mail_protocol_credentials_mailbox_id;size:36" json:"mailbox_id"`
	// AccountID is retained only to identify credentials created before the
	// mailbox-scoped migration.  Legacy credentials are deliberately disabled.
	AccountID *uuid.UUID     `gorm:"index:idx_mail_protocol_credentials_account_id" json:"account_id,omitempty"`
	Label     string         `gorm:"size:128" json:"label"`
	Hash      string         `gorm:"size:255" json:"-"`
	Protocols datatypes.JSON `gorm:"type:jsonb" json:"protocols"`
	Legacy    bool           `gorm:"not null;default:false" json:"legacy"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"deleted_at"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// MessageSource is the immutable RFC 5322 representation used by IMAP and
// POP3.  Parsed Email fields remain an index/view for the HTTP API.
type MessageSource struct {
	ID           string    `gorm:"primaryKey;size:36" json:"id"`
	EmailID      string    `gorm:"uniqueIndex;size:36" json:"email_id"`
	Raw          []byte    `gorm:"type:bytea;not null" json:"-"`
	SHA256       string    `gorm:"size:64;not null" json:"sha256"`
	EnvelopeFrom string    `gorm:"size:255" json:"envelope_from"`
	ReceivedAt   time.Time `gorm:"not null" json:"received_at"`
	CreatedAt    time.Time `json:"created_at"`
}

func (s *MessageSource) BeforeCreate(tx *gorm.DB) error {
	if s.ID == "" {
		s.ID = NewID()
	}
	return nil
}

// MailFolder is an IMAP mailbox belonging to one hosted address.  UID and
// mod-sequence counters are advanced under a row lock by protocol services.
type MailFolder struct {
	ID            string    `gorm:"primaryKey;size:36" json:"id"`
	MailboxID     string    `gorm:"uniqueIndex:idx_mail_folders_mailbox_name,priority:1;size:36" json:"mailbox_id"`
	Name          string    `gorm:"uniqueIndex:idx_mail_folders_mailbox_name,priority:2;size:255" json:"name"`
	UIDValidity   uint32    `gorm:"not null" json:"uid_validity"`
	NextUID       uint32    `gorm:"not null;default:1" json:"next_uid"`
	HighestModSeq uint64    `gorm:"not null;default:1" json:"highest_modseq"`
	SpecialUse    string    `gorm:"size:32" json:"special_use,omitempty"`
	Subscribed    bool      `gorm:"not null;default:true" json:"subscribed"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (f *MailFolder) BeforeCreate(tx *gorm.DB) error {
	if f.ID == "" {
		f.ID = NewID()
	}
	return nil
}

type FolderMessage struct {
	FolderID  string         `gorm:"primaryKey;uniqueIndex:idx_folder_messages_uid,priority:1;size:36" json:"folder_id"`
	EmailID   string         `gorm:"primaryKey;size:36" json:"email_id"`
	UID       uint32         `gorm:"uniqueIndex:idx_folder_messages_uid,priority:2;not null" json:"uid"`
	Flags     datatypes.JSON `gorm:"type:jsonb;not null" json:"flags"`
	ModSeq    uint64         `gorm:"not null" json:"modseq"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

type MailOutbox struct {
	ID          string         `gorm:"primaryKey;size:36" json:"id"`
	Kind        string         `gorm:"index;size:64" json:"kind"`
	Payload     datatypes.JSON `gorm:"type:jsonb;not null" json:"payload"`
	AvailableAt time.Time      `gorm:"index;not null" json:"available_at"`
	Attempts    int            `gorm:"not null;default:0" json:"attempts"`
	PublishedAt *time.Time     `gorm:"index" json:"published_at,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
}

func (o *MailOutbox) BeforeCreate(tx *gorm.DB) error {
	if o.ID == "" {
		o.ID = NewID()
	}
	return nil
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

// MailSendUsage holds an atomic counter for one mailbox or workspace and one
// calendar period. It is used to enforce outbound send limits.
type MailSendUsage struct {
	ID          string    `gorm:"primaryKey;size:36" json:"id"`
	WorkspaceID string    `gorm:"uniqueIndex:idx_mail_send_usage_scope_period,priority:1;size:36" json:"workspace_id"`
	Scope       string    `gorm:"uniqueIndex:idx_mail_send_usage_scope_period,priority:2;size:64" json:"scope"`
	PeriodStart time.Time `gorm:"uniqueIndex:idx_mail_send_usage_scope_period,priority:3" json:"period_start"`
	SentCount   int64     `json:"sent_count"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (u *MailSendUsage) BeforeCreate(tx *gorm.DB) error {
	if u.ID == "" {
		u.ID = NewID()
	}
	return nil
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
