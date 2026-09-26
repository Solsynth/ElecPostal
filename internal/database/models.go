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

// CustomDomain records a workspace-owned provider sending domain. Secrets
// are never persisted: the configured AWS SDK credential chain owns access to
// SES, while this row stores only public verification metadata.
type CustomDomain struct {
	ID                       string         `gorm:"primaryKey;size:36" json:"id"`
	WorkspaceID              string         `gorm:"uniqueIndex:idx_custom_domains_workspace_provider_domain,priority:1;size:36" json:"workspace_id"`
	Provider                 string         `gorm:"uniqueIndex:idx_custom_domains_workspace_provider_domain,priority:2;size:32" json:"provider"`
	Domain                   string         `gorm:"uniqueIndex:idx_custom_domains_workspace_provider_domain,priority:3;size:255" json:"domain"`
	DomainType               string         `gorm:"size:32" json:"domain_type"`
	VerificationStatus       string         `gorm:"size:32" json:"verification_status"`
	VerifiedForSendingStatus bool           `json:"verified_for_sending_status"`
	DKIMStatus               string         `gorm:"size:32" json:"dkim_status"`
	MailFromDomain           string         `gorm:"size:255" json:"mail_from_domain"`
	MailFromStatus           string         `gorm:"size:32" json:"mail_from_status"`
	Stage                    string         `gorm:"size:16" json:"stage"` // basic, full, or completed
	DNSValidation            datatypes.JSON `gorm:"type:jsonb" json:"dns_validation,omitempty"`
	DNSRecords               datatypes.JSON `gorm:"type:jsonb" json:"dns_records"`
	CreatedAt                time.Time      `json:"created_at"`
	UpdatedAt                time.Time      `json:"updated_at"`
}

func (c *CustomDomain) BeforeCreate(tx *gorm.DB) error {
	if c.ID == "" {
		c.ID = NewID()
	}
	return nil
}

// MailboxAlias is an additional sender and inbound-recipient address for a
// mailbox. It always belongs to a verified workspace custom domain.
type MailboxAlias struct {
	ID             string    `gorm:"primaryKey;size:36" json:"id"`
	MailboxID      string    `gorm:"index:idx_mailbox_aliases_mailbox_id;size:36" json:"mailbox_id"`
	CustomDomainID string    `gorm:"index:idx_mailbox_aliases_custom_domain_id;size:36" json:"custom_domain_id"`
	Address        string    `gorm:"uniqueIndex;size:255" json:"address"`
	Name           string    `gorm:"size:128" json:"name"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// MailForwarding forwards mail received through a specific alias. Keeping the
// source explicit avoids accidentally forwarding every message in a mailbox.
type MailForwarding struct {
	ID          string    `gorm:"primaryKey;size:36" json:"id"`
	MailboxID   string    `gorm:"index:idx_mail_forwardings_mailbox_id;size:36" json:"mailbox_id"`
	AliasID     string    `gorm:"uniqueIndex:idx_mail_forwardings_alias_destination,priority:1;size:36" json:"alias_id"`
	Destination string    `gorm:"uniqueIndex:idx_mail_forwardings_alias_destination,priority:2;size:255" json:"destination"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (f *MailForwarding) BeforeCreate(tx *gorm.DB) error {
	if f.ID == "" {
		f.ID = NewID()
	}
	return nil
}

func (a *MailboxAlias) BeforeCreate(tx *gorm.DB) error {
	if a.ID == "" {
		a.ID = NewID()
	}
	return nil
}

func (m *Mailbox) BeforeCreate(tx *gorm.DB) error {
	if m.ID == "" {
		m.ID = NewID()
	}
	return nil
}

// Email stores a single email message.
type Email struct {
	ID          string    `gorm:"primaryKey;size:36" json:"id"`
	AccountID   uuid.UUID `gorm:"index:idx_emails_account_id" json:"account_id"`
	MailboxID   string    `gorm:"index:idx_emails_mailbox_id;size:36" json:"mailbox_id"`
	ThreadID    *string   `gorm:"index:idx_emails_thread_id;size:36" json:"thread_id,omitempty"`
	Subject     string    `gorm:"size:512" json:"subject"`
	Body        string    `gorm:"type:text" json:"body"`
	FromAddress string    `gorm:"size:255" json:"from_address"`
	FromName    string    `gorm:"size:128" json:"from_name"`
	// MessageID is the RFC 5322 Message-ID header value when present. It is the
	// per-mailbox import dedupe key; NULL when the message had no Message-ID.
	MessageID             *string        `gorm:"size:255" json:"message_id,omitempty"`
	// InReplyTo and References are the RFC 5322 reply chain of the message
	// (message-ids without angle brackets, space separated). They let the
	// service chain replies into their conversation and re-emit the chain on
	// outbound replies so the recipient's client threads them the same way.
	InReplyTo             string         `gorm:"type:text" json:"in_reply_to,omitempty"`
	References            string         `gorm:"type:text" json:"references,omitempty"`
	IsRead                bool           `json:"is_read"`
	IsStarred             bool           `json:"is_starred"`
	IsDraft               bool           `gorm:"index:idx_emails_is_draft" json:"is_draft"`
	Folder                string         `gorm:"index:idx_emails_folder;size:16" json:"folder"`
	ContentType           string         `gorm:"size:32" json:"content_type"`
	OmitContentType       bool           `gorm:"not null;default:false" json:"-"`
	IsDmarcIntake         bool           `gorm:"index:idx_emails_is_dmarc_intake" json:"is_dmarc_intake"`
	ScheduledAt           *time.Time     `json:"scheduled_at,omitempty"`
	TrashedAt             *time.Time     `json:"trashed_at,omitempty"`
	SpamAt                *time.Time     `json:"spam_at,omitempty"`
	SentAt                *time.Time     `json:"sent_at,omitempty"`
	DeliveryStatus        string         `gorm:"index;size:32" json:"delivery_status"`
	DeliveryAttempts      int            `json:"delivery_attempts"`
	LastDeliveryAttemptAt *time.Time     `json:"last_delivery_attempt_at,omitempty"`
	DeliveryError         *string        `gorm:"type:text" json:"delivery_error,omitempty"`
	ProviderMessageID     *string        `gorm:"size:255" json:"provider_message_id,omitempty"`
	Authentication        datatypes.JSON `gorm:"type:jsonb" json:"authentication,omitempty"`
	// Summary is the personality-service summary generated for notifications
	// when the account enabled AI summaries. It is empty for every other
	// message, including those whose content looked secret-bearing.
	Summary string `gorm:"type:text" json:"summary,omitempty"`
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
	ID          string                    `gorm:"primaryKey;size:36" json:"id"`
	EmailID     string                    `gorm:"index:idx_attachments_email_id;size:36" json:"email_id"`
	Position    int                       `gorm:"index:idx_attachments_email_position" json:"position"`
	Filename    string                    `gorm:"size:255" json:"filename"`
	MimeType    string                    `gorm:"size:128" json:"mime_type"`
	Size        int64                     `json:"size"`
	StorageKey  *string                   `gorm:"size:128" json:"storage_key,omitempty"`
	File        *CloudFileReferenceObject `gorm:"serializer:json;type:jsonb" json:"file,omitempty"`
	ContentID   string                    `gorm:"size:512" json:"content_id,omitempty"`
	Disposition string                    `gorm:"size:32" json:"disposition,omitempty"`
	CreatedAt   time.Time                 `json:"created_at"`
	UpdatedAt   time.Time                 `json:"updated_at"`
}

// CloudFileReferenceObject is the denormalized DysonFS file snapshot stored
// with an email attachment. It mirrors the reference-object pattern used by
// DysonNetwork services, so clients can render files without treating the
// email attachment row ID as a Drive file ID.
type CloudFileReferenceObject struct {
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	FileMeta        map[string]any `json:"file_meta"`
	UserMeta        map[string]any `json:"user_meta"`
	SensitiveMarks  []int          `json:"sensitive_marks"`
	MimeType        string         `json:"mime_type"`
	Hash            string         `json:"hash"`
	Size            int64          `json:"size"`
	HasCompression  bool           `json:"has_compression"`
	URL             string         `json:"url,omitempty"`
	Width           *int           `json:"width,omitempty"`
	Height          *int           `json:"height,omitempty"`
	Blurhash        string         `json:"blurhash,omitempty"`
	Usage           string         `json:"usage,omitempty"`
	ApplicationType string         `json:"application_type,omitempty"`
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

// MessageSource is an attachment-free protocol manifest used by IMAP and
// POP3. Attachment bytes remain in DysonFS and are streamed on demand.
type MessageSource struct {
	ID            string         `gorm:"primaryKey;size:36" json:"id"`
	EmailID       string         `gorm:"uniqueIndex;size:36" json:"email_id"`
	Manifest      datatypes.JSON `gorm:"type:jsonb;default:'{}';not null" json:"manifest"`
	WireSizeBytes int64          `gorm:"not null;default:0" json:"wire_size_bytes"`
	SHA256        string         `gorm:"size:64;not null" json:"sha256"`
	EnvelopeFrom  string         `gorm:"size:255" json:"envelope_from"`
	ReceivedAt    time.Time      `gorm:"not null" json:"received_at"`
	CreatedAt     time.Time      `json:"created_at"`
}

func (s *MessageSource) BeforeCreate(tx *gorm.DB) error {
	if s.ID == "" {
		s.ID = NewID()
	}
	return nil
}

// DmarcReport stores a parsed DMARC aggregate report linked to its original
// received email. The raw email and attachments remain in the normal message
// store for audit and reprocessing.
type DmarcReport struct {
	ID              string     `gorm:"primaryKey;size:36" json:"id"`
	EmailID         string     `gorm:"index:idx_dmarc_reports_email_id;size:36" json:"email_id"`
	AccountID       uuid.UUID  `gorm:"index:idx_dmarc_reports_account_id" json:"account_id"`
	MailboxID       string     `gorm:"index:idx_dmarc_reports_mailbox_id;size:36" json:"mailbox_id"`
	AttachmentName  string     `gorm:"size:255" json:"attachment_name"`
	ReporterOrg     string     `gorm:"size:255" json:"reporter_org"`
	ReporterEmail   string     `gorm:"size:255" json:"reporter_email"`
	ReportID        string     `gorm:"size:255" json:"report_id"`
	DateBegin       *time.Time `json:"date_begin,omitempty"`
	DateEnd         *time.Time `json:"date_end,omitempty"`
	Domain          string     `gorm:"size:255" json:"domain"`
	ADKIM           string     `gorm:"size:16" json:"adkim"`
	ASPF            string     `gorm:"size:16" json:"aspf"`
	Policy          string     `gorm:"size:16" json:"policy"`
	SubdomainPolicy string     `gorm:"size:16" json:"subdomain_policy"`
	Percentage      int        `json:"percentage"`
	ParseStatus     string     `gorm:"index;size:16" json:"parse_status"`
	ParseError      string     `gorm:"type:text" json:"parse_error,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`

	Email   *Email              `gorm:"foreignKey:EmailID;references:ID" json:"email,omitempty"`
	Records []DmarcReportRecord `gorm:"foreignKey:ReportID;references:ID" json:"records,omitempty"`
}

func (r *DmarcReport) BeforeCreate(tx *gorm.DB) error {
	if r.ID == "" {
		r.ID = NewID()
	}
	return nil
}

// DmarcReportRecord stores one source-IP evaluation row from a DMARC report.
type DmarcReportRecord struct {
	ID           string    `gorm:"primaryKey;size:36" json:"id"`
	ReportID     string    `gorm:"index:idx_dmarc_report_records_report_id;size:36" json:"report_id"`
	SourceIP     string    `gorm:"size:64" json:"source_ip"`
	Count        int64     `json:"count"`
	Disposition  string    `gorm:"size:32" json:"disposition"`
	DKIM         string    `gorm:"size:32" json:"dkim"`
	SPF          string    `gorm:"size:32" json:"spf"`
	HeaderFrom   string    `gorm:"size:255" json:"header_from"`
	EnvelopeFrom string    `gorm:"size:255" json:"envelope_from"`
	EnvelopeTo   string    `gorm:"size:255" json:"envelope_to"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (r *DmarcReportRecord) BeforeCreate(tx *gorm.DB) error {
	if r.ID == "" {
		r.ID = NewID()
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

// AccountNotificationSettings holds one account's incoming-mail notification
// preferences. Accounts without a row fall back to DefaultNotificationSettings,
// so enabling a flag for every existing account needs no backfill.
type AccountNotificationSettings struct {
	AccountID uuid.UUID `gorm:"primaryKey;size:36" json:"account_id"`
	// Highlight replaces the notification subtitle with a verification code,
	// security event, or action request extracted from the message.
	Highlight bool `gorm:"not null" json:"highlight"`
	// Summarize asks the personality service for a one-line summary when the
	// message carries no code, security event, or action request.
	Summarize bool      `gorm:"not null" json:"summarize"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// DefaultNotificationSettings returns the preferences applied to an account
// that never changed them. Highlighting is on because it only picks a different
// part of the same message; AI summaries are off because they send message
// content to the personality service.
func DefaultNotificationSettings(accountID uuid.UUID) AccountNotificationSettings {
	return AccountNotificationSettings{AccountID: accountID, Highlight: true}
}
