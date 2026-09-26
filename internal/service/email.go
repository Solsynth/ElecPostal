package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"src.solsynth.dev/sosys/elecpostal/internal/account"
	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/dmarc"
	"src.solsynth.dev/sosys/elecpostal/internal/filesystem"
	"src.solsynth.dev/sosys/elecpostal/internal/logging"
	"src.solsynth.dev/sosys/elecpostal/internal/mailmime"
	"src.solsynth.dev/sosys/elecpostal/internal/mailtext"
	"src.solsynth.dev/sosys/elecpostal/internal/personality"
	"src.solsynth.dev/sosys/elecpostal/internal/realtime"
	"src.solsynth.dev/sosys/elecpostal/internal/relay"
	"src.solsynth.dev/sosys/elecpostal/internal/ring"
	"src.solsynth.dev/sosys/elecpostal/internal/workspace"
	gen "src.solsynth.dev/sosys/go/proto"
)

var (
	ErrNotFound                  = errors.New("not found")
	ErrForbidden                 = errors.New("forbidden")
	ErrWorkspaceUnavailable      = errors.New("workspace quota service is not configured")
	ErrMailboxLimitExceeded      = errors.New("workspace mailbox limit exceeded")
	ErrCustomDomainLimitExceeded = errors.New("workspace custom domain limit exceeded")
	ErrSendLimitExceeded         = errors.New("outbound email send limit exceeded")
	ErrOutboundRelayUnavailable  = errors.New("outbound relay is not configured")
)

var reservedMailboxLocalParts = map[string]struct{}{
	"admin": {}, "administrator": {}, "abuse": {}, "dmarc": {}, "hostmaster": {},
	"postmaster": {}, "security": {}, "webmaster": {},
}

const (
	archiveRetention   = 30 * 24 * time.Hour
	sendUsageRetention = 62 * 24 * time.Hour
)

// RecipientInput is a recipient for a new email.
type RecipientInput struct {
	Address string `json:"address" binding:"required"`
	Name    string `json:"name"`
	Kind    string `json:"kind"` // to, cc, bcc
}

// AttachmentInput is an attachment reference for a new email.
type AttachmentInput struct {
	Filename    string                             `json:"filename" binding:"required"`
	MimeType    string                             `json:"mime_type"`
	Size        int64                              `json:"size"`
	StorageKey  *string                            `json:"storage_key,omitempty"`
	File        *database.CloudFileReferenceObject `json:"file,omitempty"`
	ContentID   string                             `json:"content_id,omitempty"`
	Disposition string                             `json:"disposition,omitempty"`
	Position    int                                `json:"position"`
}

// AttachmentReference identifies an already-uploaded DysonFS object.
type AttachmentReference struct {
	Filename    string
	MimeType    string
	Size        int64
	StorageKey  string
	File        *database.CloudFileReferenceObject
	ContentID   string
	Disposition string
	Position    int
}

// SendEmailInput is the payload for sending an email.
type SendEmailInput struct {
	MailboxID     string           `json:"mailbox_id" binding:"required"`
	FromAliasID   string           `json:"from_alias_id,omitempty"`
	ThreadID      string           `json:"thread_id,omitempty"`
	ReplyToID     string           `json:"reply_to_id,omitempty"`
	To            []RecipientInput `json:"to" binding:"required,min=1"`
	Cc            []RecipientInput `json:"cc"`
	Bcc           []RecipientInput `json:"bcc"`
	Subject       string           `json:"subject"`
	Body          string           `json:"body"`
	ContentType   string           `json:"content_type"` // text/plain or text/html
	AttachmentIDs []string         `json:"attachment_ids"`
	IsDraft       bool             `json:"is_draft"`
	ScheduledAt   *time.Time       `json:"scheduled_at,omitempty"`
}

// IncomingAttachment contains the raw content delivered by another mail
// service. ElecPostal owns the subsequent FileSystem upload.
type IncomingAttachment struct {
	Filename    string
	MimeType    string
	Size        int64
	ContentID   string
	Disposition string
	Content     io.Reader
}

// ReceiveEmailInput is the trusted interservice payload for an inbound email.
// MailboxID identifies the local destination; ownership is never supplied by
// the caller and is always resolved from that mailbox.
type ReceiveEmailInput struct {
	MailboxID            string
	ThreadID             string
	MessageID            string
	FromAddress          string
	FromName             string
	Subject              string
	Body                 string
	ContentType          string
	OmitContentType      bool
	To                   []RecipientInput
	Cc                   []RecipientInput
	Attachments          []IncomingAttachment
	AttachmentReferences []AttachmentReference
	SentAt               *time.Time
	Authentication       datatypes.JSON
	EnvelopeFrom         string
	DeliveredTo          []string
	DmarcIntake          bool
}

// ListInput is pagination for list endpoints.
type ListInput struct {
	Offset         int
	Take           int
	MailboxID      string
	WorkspaceID    string
	Query          string
	From           string
	To             string
	IsRead         *bool
	IsStarred      *bool
	IsDraft        *bool
	HasAttachments *bool
	DeliveryStatus string
	LabelID        string
	Folder         string
}

// MailboxStats provides lightweight counts for mailbox navigation and filter badges.
type MailboxStats struct {
	Total          int64            `json:"total"`
	Unread         int64            `json:"unread"`
	Starred        int64            `json:"starred"`
	Drafts         int64            `json:"drafts"`
	DeliveryStatus map[string]int64 `json:"delivery_status"`
}

// ThreadSummary is one conversation row suitable for a mailbox list.
type ThreadSummary struct {
	ID            string       `json:"id"`
	MailboxID     string       `json:"mailbox_id"`
	Subject       string       `json:"subject"`
	LatestAt      time.Time    `json:"latest_at"`
	MessageCount  int64        `json:"message_count"`
	UnreadCount   int64        `json:"unread_count"`
	Participants  []string     `json:"participants"`
	LatestMessage EmailSummary `json:"latest_message"`
}

// CreateBlockRuleInput creates a sender or domain rule for a mailbox or workspace.
type CreateBlockRuleInput struct {
	Scope       string `json:"scope" binding:"required"`
	WorkspaceID string `json:"workspace_id"`
	MailboxID   string `json:"mailbox_id"`
	Pattern     string `json:"pattern" binding:"required"`
}

type CreateCustomDomainInput struct {
	WorkspaceID string `json:"workspace_id" binding:"required"`
	Domain      string `json:"domain" binding:"required"`
}

type CreateMailboxAliasInput struct {
	CustomDomainID string `json:"custom_domain_id" binding:"required"`
	LocalPart      string `json:"local_part" binding:"required"`
	Name           string `json:"name"`
}

type CreateMailForwardingInput struct {
	AliasID     string `json:"alias_id" binding:"required"`
	Destination string `json:"destination" binding:"required"`
}

const (
	folderInbox   = "inbox"
	folderSent    = "sent"
	folderDrafts  = "drafts"
	folderSpam    = "spam"
	folderTrash   = "trash"
	folderArchive = "archive"
)

// MailboxQuota reports the workspace's shared storage usage and limit.
// Attachment bytes are excluded because DysonFS reports them separately.
type MailboxQuota struct {
	WorkspaceID    string `json:"workspace_id"`
	UsedBytes      int64  `json:"used_bytes"`
	LimitBytes     int64  `json:"limit_bytes"`
	RemainingBytes int64  `json:"remaining_bytes"`
}

// WorkspaceMailboxUsage reports how many mailboxes a workspace has created
// against its plan's mailbox limit.
type WorkspaceMailboxUsage struct {
	WorkspaceID string `json:"workspace_id"`
	Used        int64  `json:"used"`
	Limit       int64  `json:"limit"`
	Remaining   int64  `json:"remaining"`
}

// SendUsage reports one calendar-period outbound send counter against its
// limit. A zero limit means the counter is disabled.
type SendUsage struct {
	Limit     int64 `json:"limit"`
	Used      int64 `json:"used"`
	Remaining int64 `json:"remaining"`
}

// WorkspaceSendUsage reports the workspace-scoped outbound send limits for the
// current day and month and their usage. Mailbox-scoped limits are not
// included.
type WorkspaceSendUsage struct {
	WorkspaceID string    `json:"workspace_id"`
	Daily       SendUsage `json:"daily"`
	Monthly     SendUsage `json:"monthly"`
}

// WorkspaceCustomDomainUsage reports how many SES custom domains a workspace
// has connected against its plan limit.
type WorkspaceCustomDomainUsage struct {
	WorkspaceID string `json:"workspace_id"`
	Used        int64  `json:"used"`
	Limit       int64  `json:"limit"`
	Remaining   int64  `json:"remaining"`
}

// EmailService handles email-related business logic.
type EmailService struct {
	db                *database.DB
	notifier          NotificationSender
	realtime          realtime.Publisher
	files             filesystem.ByteStore
	relay             relay.Adapter
	workspace         workspace.Provider
	sharedQuotaClient gen.DyQuotaServiceClient
	identities        relay.IdentityManager
	language          account.Provider
	summarizer        personality.Summarizer
	domain            string
	inbound           string
	dns               relay.DNSChecker
	dnsLabel          string
}

// SetRelay configures outbound delivery. A nil adapter retains the existing
// persistence-only behavior for deployments without delivery enabled.
func (s *EmailService) SetRelay(adapter relay.Adapter) {
	s.relay = adapter
}

// HasRelay reports whether an outbound relay adapter is configured.
func (s *EmailService) HasRelay() bool { return s.relay != nil }

// SetIdentityManager enables provider identity provisioning. It is normally
// the SES adapter configured as the outbound relay.
func (s *EmailService) SetIdentityManager(manager relay.IdentityManager) {
	s.identities = manager
}

// SetRealtimePublisher enables non-blocking mailbox change pushes.
func (s *EmailService) SetRealtimePublisher(publisher realtime.Publisher) { s.realtime = publisher }

// SetDomain configures the canonical mail domain used to complete local-only
// mailbox addresses (e.g. "alice" -> "alice@example.com") for outbound relay.
func (s *EmailService) SetDomain(domain string) {
	s.domain = strings.TrimSpace(strings.ToLower(domain))
}

// SetInboundHost configures the public MX hostname that receives mail for
// local domains. Custom domains advertise it as their inbound MX record.
func (s *EmailService) SetInboundHost(host string) {
	s.inbound = strings.TrimSpace(strings.ToLower(host))
}

// SetDNSResolver configures the DNS server used to validate custom-domain
// DKIM, SPF, and inbound MX records. An empty host falls back to 1.1.1.1.
func (s *EmailService) SetDNSResolver(host string) error {
	host = strings.TrimSpace(host)
	if host == "" {
		host = "1.1.1.1"
	}
	resolver, err := relay.NewDNSResolver(host)
	if err != nil {
		return err
	}
	s.dns = resolver
	s.dnsLabel = host
	return nil
}

// NewEmailService creates a new EmailService.
func NewEmailService(db *database.DB, notifier NotificationSender) *EmailService {
	return &EmailService{db: db, notifier: notifier}
}

// MailHost returns the configured canonical mail domain, if any.
func (s *EmailService) MailHost() string {
	return s.domain
}

// ResolveLocalMailbox finds a mailbox that may receive SMTP mail. postmaster
// is intentionally an alias only at delivery time; it can never be created as
// a normal mailbox address.
func (s *EmailService) ResolveLocalMailbox(ctx context.Context, address string) (*database.Mailbox, error) {
	address = strings.ToLower(strings.TrimSpace(address))
	var alias database.MailboxAlias
	if err := s.db.WithContext(ctx).Where("LOWER(address) = ?", address).First(&alias).Error; err == nil {
		var mailbox database.Mailbox
		if err := s.db.WithContext(ctx).Where("id = ?", alias.MailboxID).First(&mailbox).Error; err != nil {
			return nil, err
		}
		return &mailbox, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	at := strings.LastIndex(address, "@")
	if at <= 0 || at == len(address)-1 || s.domain == "" || address[at+1:] != s.domain {
		return nil, ErrNotFound
	}
	localPart := address[:at]
	var mailbox database.Mailbox
	query := s.db.WithContext(ctx)
	if localPart == "dmarc" {
		query = query.Where("is_default = ?", true).Order("created_at ASC")
	} else if localPart == "postmaster" {
		// Legacy mailboxes store only their local-part. Full-address rows must
		// still belong to the configured domain before becoming postmaster.
		query = query.Where("is_default = ? AND (LOWER(address) NOT LIKE ? OR LOWER(address) LIKE ?)", true, "%@%", "%@"+s.domain).Order("created_at ASC")
	} else {
		query = query.Where("LOWER(address) IN ?", []string{address, localPart})
	}
	if err := query.First(&mailbox).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &mailbox, nil
}

// IsMailboxSender reports whether address is the primary address or an alias
// assigned to mailboxID. SMTP submission uses it to prevent mailbox spoofing.
func (s *EmailService) IsMailboxSender(ctx context.Context, mailboxID, address string) (bool, error) {
	address = strings.ToLower(strings.TrimSpace(address))
	var mailbox database.Mailbox
	if err := s.db.WithContext(ctx).Where("id = ?", mailboxID).First(&mailbox).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	if address == s.normalizeFromAddress(mailbox.Address) {
		return true, nil
	}
	var count int64
	if err := s.db.WithContext(ctx).Model(&database.MailboxAlias{}).Where("mailbox_id = ? AND LOWER(address) = ?", mailboxID, address).Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// mailboxLoginCandidates supports legacy mailbox rows containing only a
// local-part while retaining the canonical full-address form for new rows.
func (s *EmailService) mailboxLoginCandidates(address string) []string {
	address = strings.ToLower(strings.TrimSpace(address))
	if at := strings.LastIndex(address, "@"); at > 0 && at < len(address)-1 && s.domain != "" && address[at+1:] == s.domain {
		return []string{address, address[:at]}
	}
	return []string{address}
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
	SendEmailNotification(context.Context, ring.EmailNotification) error
	Close() error
}

// SetAttachmentByteStore enables streaming attachment uploads and downloads
// through DysonFS.
func (s *EmailService) SetAttachmentByteStore(store filesystem.ByteStore) {
	s.files = store
}

// SetWorkspaceProvider enables workspace membership checks and derives the
// mail allowance from the workspace plan's storage quota.
func (s *EmailService) SetWorkspaceProvider(provider workspace.Provider) {
	s.workspace = provider
}

// SetSharedQuotaClient enables aggregate workspace storage usage checks.
func (s *EmailService) SetSharedQuotaClient(client gen.DyQuotaServiceClient) {
	s.sharedQuotaClient = client
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
	if s.realtime != nil {
		if err := s.realtime.Close(); err != nil {
			return err
		}
	}
	if s.language != nil {
		if err := s.language.Close(); err != nil {
			return err
		}
	}
	if s.summarizer != nil {
		if err := s.summarizer.Close(); err != nil {
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

// CreateCustomDomain provisions a workspace-owned SES domain. Credentials
// remain server-side in the AWS SDK provider chain and are never accepted from
// HTTP clients or written to this service's database.
func (s *EmailService) CreateCustomDomain(ctx context.Context, accountID uuid.UUID, input CreateCustomDomainInput) (*database.CustomDomain, error) {
	if s.identities == nil {
		return nil, relay.ErrIdentityManagementUnavailable
	}
	if err := s.authorizeWorkspaceMember(ctx, strings.TrimSpace(input.WorkspaceID), accountID); err != nil {
		return nil, err
	}
	domain, err := normalizeCustomDomain(input.Domain)
	if err != nil {
		return nil, err
	}
	var existing database.CustomDomain
	err = s.db.WithContext(ctx).Where("workspace_id = ? AND provider = ? AND domain = ?", input.WorkspaceID, "ses", domain).First(&existing).Error
	if err == nil {
		return s.refreshCustomDomain(ctx, &existing)
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	domainLimit, err := s.workspace.CustomDomainLimit(ctx, strings.TrimSpace(input.WorkspaceID))
	if err != nil {
		return nil, fmt.Errorf("get workspace custom domain limit: %w", err)
	}
	var domainCount int64
	if err := s.db.WithContext(ctx).Model(&database.CustomDomain{}).Where("workspace_id = ?", input.WorkspaceID).Count(&domainCount).Error; err != nil {
		return nil, err
	}
	if domainCount >= domainLimit {
		return nil, fmt.Errorf("%w: limit=%d", ErrCustomDomainLimitExceeded, domainLimit)
	}
	status, err := s.identities.CreateIdentity(ctx, domain)
	if err != nil {
		return nil, err
	}
	// Never let a workspace claim an already-verified account-wide SES domain
	// that was not created through its own record.
	if status.VerifiedForSendingStatus {
		return nil, fmt.Errorf("SES custom domain already exists; contact an administrator to associate it")
	}
	customDomain := database.CustomDomain{WorkspaceID: strings.TrimSpace(input.WorkspaceID), Provider: "ses", Domain: domain}
	s.applyCustomDomainStatusWithDNS(ctx, &customDomain, status)
	if err := s.db.WithContext(ctx).Create(&customDomain).Error; err != nil {
		return nil, err
	}
	return &customDomain, nil
}

func (s *EmailService) ListCustomDomains(ctx context.Context, accountID uuid.UUID, workspaceID string) ([]database.CustomDomain, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return nil, fmt.Errorf("workspace_id is required")
	}
	if err := s.authorizeWorkspaceMember(ctx, workspaceID, accountID); err != nil {
		return nil, err
	}
	var domains []database.CustomDomain
	if err := s.db.WithContext(ctx).Where("workspace_id = ?", workspaceID).Order("created_at DESC").Find(&domains).Error; err != nil {
		return nil, err
	}
	return domains, nil
}

func (s *EmailService) RefreshCustomDomain(ctx context.Context, accountID uuid.UUID, domainID string) (*database.CustomDomain, error) {
	if s.identities == nil {
		return nil, relay.ErrIdentityManagementUnavailable
	}
	var domain database.CustomDomain
	if err := s.db.WithContext(ctx).Where("id = ?", domainID).First(&domain).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if err := s.authorizeWorkspaceMember(ctx, domain.WorkspaceID, accountID); err != nil {
		return nil, err
	}
	return s.refreshCustomDomain(ctx, &domain)
}

// DeleteCustomDomain deletes the remote SES domain and then its local record.
func (s *EmailService) DeleteCustomDomain(ctx context.Context, accountID uuid.UUID, domainID string) error {
	if s.identities == nil {
		return relay.ErrIdentityManagementUnavailable
	}
	var domain database.CustomDomain
	if err := s.db.WithContext(ctx).Where("id = ?", domainID).First(&domain).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}
	if err := s.authorizeWorkspaceMember(ctx, domain.WorkspaceID, accountID); err != nil {
		return err
	}
	var otherCount int64
	if err := s.db.WithContext(ctx).Model(&database.CustomDomain{}).Where("provider = ? AND domain = ? AND id <> ?", domain.Provider, domain.Domain, domain.ID).Count(&otherCount).Error; err != nil {
		return err
	}
	if otherCount > 0 {
		return fmt.Errorf("custom domain is also connected to another workspace")
	}
	var aliasCount int64
	if err := s.db.WithContext(ctx).Model(&database.MailboxAlias{}).Where("custom_domain_id = ?", domain.ID).Count(&aliasCount).Error; err != nil {
		return err
	}
	if aliasCount > 0 {
		return fmt.Errorf("custom domain still has mailbox aliases")
	}
	if err := s.identities.DeleteIdentity(ctx, domain.Domain); err != nil {
		return err
	}
	return s.db.WithContext(ctx).Delete(&domain).Error
}

func (s *EmailService) refreshCustomDomain(ctx context.Context, domain *database.CustomDomain) (*database.CustomDomain, error) {
	status, err := s.identities.GetIdentity(ctx, domain.Domain)
	if err != nil {
		return nil, err
	}
	// Provision the custom MAIL FROM subdomain for domains created before it
	// was supported, so refresh upgrades them without a full re-create.
	if status.IdentityType == "DOMAIN" && status.MailFromDomain == "" {
		if err := s.identities.EnsureMailFrom(ctx, domain.Domain); err != nil {
			return nil, err
		}
		status, err = s.identities.GetIdentity(ctx, domain.Domain)
		if err != nil {
			return nil, err
		}
	}
	s.applyCustomDomainStatusWithDNS(ctx, domain, status)
	if err := s.db.WithContext(ctx).Save(domain).Error; err != nil {
		return nil, err
	}
	return domain, nil
}

// customDomainDNSRecords returns the records a customer must publish: the
// provider records plus the inbound MX record for receiving mail.
func customDomainDNSRecords(status relay.IdentityStatus, inboundHost string) []relay.DNSRecord {
	records := status.DNSRecords
	if inboundHost != "" && status.IdentityType == "DOMAIN" {
		records = append(records, relay.DNSRecord{Name: status.Identity, Type: "MX", Value: "10 " + inboundHost})
	}
	return records
}

// applyCustomDomainStatusWithDNS persists provider status, validates the
// published records through the configured DNS resolver, and derives the setup
// stage.
func (s *EmailService) applyCustomDomainStatusWithDNS(ctx context.Context, domain *database.CustomDomain, status relay.IdentityStatus) {
	records := customDomainDNSRecords(status, s.inbound)
	applyCustomDomainStatus(domain, status, records)
	if s.dns == nil {
		domain.Stage = relay.CustomDomainStageBasic
		domain.DNSValidation = nil
		return
	}
	validation := relay.ValidateCustomDomainDNS(ctx, s.dns, domain.Domain, records, s.inbound, s.dnsLabel)
	domain.Stage = relay.StageReport(validation)
	encoded, _ := json.Marshal(validation)
	domain.DNSValidation = datatypes.JSON(encoded)
}

func applyCustomDomainStatus(domain *database.CustomDomain, status relay.IdentityStatus, records []relay.DNSRecord) {
	domain.DomainType = status.IdentityType
	domain.VerificationStatus = status.VerificationStatus
	domain.VerifiedForSendingStatus = status.VerifiedForSendingStatus
	domain.DKIMStatus = status.DKIMStatus
	domain.MailFromDomain = status.MailFromDomain
	domain.MailFromStatus = status.MailFromStatus
	encoded, _ := json.Marshal(records)
	domain.DNSRecords = datatypes.JSON(encoded)
}

func normalizeCustomDomain(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if strings.Contains(value, "@") {
		return "", fmt.Errorf("domain must not include an email address")
	}
	parsed, err := url.Parse("https://" + value)
	if err != nil || parsed.Host != value || !strings.Contains(value, ".") {
		return "", fmt.Errorf("domain must be valid")
	}
	return value, nil
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
	localPart := strings.Split(address, "@")[0]
	if _, reserved := reservedMailboxLocalParts[localPart]; reserved {
		return nil, fmt.Errorf("mailbox local-part %q is reserved", localPart)
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
		if err := tx.Create(&mailbox).Error; err != nil {
			return err
		}
		return s.ensureProtocolFoldersTx(tx, mailbox.ID)
	})
	if err != nil {
		return nil, err
	}
	return &mailbox, nil
}

func (s *EmailService) ListMailboxAliases(ctx context.Context, accountID uuid.UUID, mailboxID string) ([]database.MailboxAlias, error) {
	mailbox, err := s.authorizedMailbox(ctx, accountID, mailboxID)
	if err != nil {
		return nil, err
	}
	var aliases []database.MailboxAlias
	if err := s.db.WithContext(ctx).Where("mailbox_id = ?", mailbox.ID).Order("address ASC").Find(&aliases).Error; err != nil {
		return nil, err
	}
	return aliases, nil
}

// CreateMailboxAlias assigns an address on a verified workspace custom domain
// to a mailbox. The alias can be selected as from_alias_id when sending.
func (s *EmailService) CreateMailboxAlias(ctx context.Context, accountID uuid.UUID, mailboxID string, input CreateMailboxAliasInput) (*database.MailboxAlias, error) {
	mailbox, err := s.authorizedMailbox(ctx, accountID, mailboxID)
	if err != nil {
		return nil, err
	}
	var domain database.CustomDomain
	if err := s.db.WithContext(ctx).Where("id = ? AND workspace_id = ? AND verified_for_sending_status = ?", input.CustomDomainID, mailbox.WorkspaceID, true).First(&domain).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("verified custom domain not found")
		}
		return nil, err
	}
	localPart := strings.TrimSpace(strings.ToLower(input.LocalPart))
	address := localPart + "@" + domain.Domain
	parsed, err := mail.ParseAddress(address)
	if err != nil || parsed.Address != address || strings.ContainsAny(localPart, "@<>") {
		return nil, fmt.Errorf("local_part must form a valid email address")
	}
	if _, reserved := reservedMailboxLocalParts[localPart]; reserved {
		return nil, fmt.Errorf("mailbox local-part %q is reserved", localPart)
	}
	if strings.EqualFold(address, s.normalizeFromAddress(mailbox.Address)) {
		return nil, fmt.Errorf("alias duplicates the mailbox address")
	}
	alias := database.MailboxAlias{MailboxID: mailbox.ID, CustomDomainID: domain.ID, Address: address, Name: strings.TrimSpace(input.Name)}
	if err := s.db.WithContext(ctx).Create(&alias).Error; err != nil {
		return nil, err
	}
	return &alias, nil
}

func (s *EmailService) DeleteMailboxAlias(ctx context.Context, accountID uuid.UUID, mailboxID, aliasID string) error {
	mailbox, err := s.authorizedMailbox(ctx, accountID, mailboxID)
	if err != nil {
		return err
	}
	var forwardingCount int64
	if err := s.db.WithContext(ctx).Model(&database.MailForwarding{}).Where("alias_id = ?", aliasID).Count(&forwardingCount).Error; err != nil {
		return err
	}
	if forwardingCount > 0 {
		return fmt.Errorf("alias still has forwarding rules")
	}
	result := s.db.WithContext(ctx).Where("id = ? AND mailbox_id = ?", aliasID, mailbox.ID).Delete(&database.MailboxAlias{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *EmailService) ListMailForwardings(ctx context.Context, accountID uuid.UUID, mailboxID string) ([]database.MailForwarding, error) {
	mailbox, err := s.authorizedMailbox(ctx, accountID, mailboxID)
	if err != nil {
		return nil, err
	}
	var forwardings []database.MailForwarding
	if err := s.db.WithContext(ctx).Where("mailbox_id = ?", mailbox.ID).Order("created_at ASC").Find(&forwardings).Error; err != nil {
		return nil, err
	}
	return forwardings, nil
}

// CreateMailForwarding sends copies of messages received through an alias to
// one external address. The original stays in the mailbox.
func (s *EmailService) CreateMailForwarding(ctx context.Context, accountID uuid.UUID, mailboxID string, input CreateMailForwardingInput) (*database.MailForwarding, error) {
	mailbox, err := s.authorizedMailbox(ctx, accountID, mailboxID)
	if err != nil {
		return nil, err
	}
	var alias database.MailboxAlias
	if err := s.db.WithContext(ctx).Where("id = ? AND mailbox_id = ?", input.AliasID, mailbox.ID).First(&alias).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	destination, err := normalizeForwardDestination(input.Destination)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(destination, alias.Address) {
		return nil, fmt.Errorf("forward destination cannot be the source alias")
	}
	forwarding := database.MailForwarding{MailboxID: mailbox.ID, AliasID: alias.ID, Destination: destination}
	if err := s.db.WithContext(ctx).Create(&forwarding).Error; err != nil {
		return nil, err
	}
	return &forwarding, nil
}

func (s *EmailService) DeleteMailForwarding(ctx context.Context, accountID uuid.UUID, mailboxID, forwardingID string) error {
	mailbox, err := s.authorizedMailbox(ctx, accountID, mailboxID)
	if err != nil {
		return err
	}
	result := s.db.WithContext(ctx).Where("id = ? AND mailbox_id = ?", forwardingID, mailbox.ID).Delete(&database.MailForwarding{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func normalizeForwardDestination(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Address != value {
		return "", fmt.Errorf("destination must be a valid email address")
	}
	return value, nil
}

func (s *EmailService) authorizedMailbox(ctx context.Context, accountID uuid.UUID, mailboxID string) (*database.Mailbox, error) {
	var mailbox database.Mailbox
	if err := s.db.WithContext(ctx).Where("id = ? AND account_id = ?", mailboxID, accountID).First(&mailbox).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if err := s.authorizeWorkspaceMember(ctx, mailbox.WorkspaceID, accountID); err != nil {
		return nil, err
	}
	return &mailbox, nil
}

// ListEmails returns email summaries for an account with optional mailbox and workspace
// filters. Workspace filters require active membership in that workspace.
func (s *EmailService) ListEmails(ctx context.Context, accountID uuid.UUID, mailboxID string, input ListInput) ([]EmailSummary, int64, error) {
	if input.Take <= 0 {
		input.Take = 20
	}
	if input.Take > 200 {
		input.Take = 200
	}
	if input.Offset < 0 {
		input.Offset = 0
	}

	if workspaceID := strings.TrimSpace(input.WorkspaceID); workspaceID != "" {
		if err := s.authorizeWorkspaceMember(ctx, workspaceID, accountID); err != nil {
			return nil, 0, err
		}
	}

	query := s.emailListQuery(ctx, accountID, input)
	if strings.TrimSpace(mailboxID) != "" {
		query = query.Where("mailbox_id = ?", mailboxID)
	} else if filterMailboxID := strings.TrimSpace(input.MailboxID); filterMailboxID != "" {
		query = query.Where("emails.mailbox_id = ?", filterMailboxID)
	}
	if workspaceID := strings.TrimSpace(input.WorkspaceID); workspaceID != "" {
		query = query.Joins("JOIN mailboxes ON mailboxes.id = emails.mailbox_id AND mailboxes.deleted_at IS NULL").
			Where("mailboxes.workspace_id = ?", workspaceID)
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
	summaries := make([]EmailSummary, len(items))
	for i := range items {
		summaries[i] = emailSummary(items[i])
	}
	return summaries, total, nil
}

// GetMailboxStats returns counts for an account's active messages. mailboxID
// may be empty to aggregate all mailboxes.
func (s *EmailService) GetMailboxStats(ctx context.Context, accountID uuid.UUID, mailboxID string) (MailboxStats, error) {
	base := s.db.WithContext(ctx).Model(&database.Email{}).Where("account_id = ? AND archived_at IS NULL AND is_dmarc_intake = ?", accountID, false)
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
	query := s.db.WithContext(ctx).Model(&database.Email{}).Where("emails.account_id = ? AND emails.archived_at IS NULL AND emails.is_dmarc_intake = ?", accountID, false)
	if term := strings.TrimSpace(input.Query); term != "" {
		like := "%" + strings.ToLower(term) + "%"
		query = query.Where("(LOWER(emails.subject) LIKE ? OR LOWER(emails.body) LIKE ? OR LOWER(emails.from_address) LIKE ? OR LOWER(emails.from_name) LIKE ? OR EXISTS (SELECT 1 FROM recipients WHERE recipients.email_id = emails.id AND (LOWER(recipients.address) LIKE ? OR LOWER(recipients.name) LIKE ?)))", like, like, like, like, like, like)
	}
	if sender := strings.TrimSpace(input.From); sender != "" {
		like := "%" + strings.ToLower(sender) + "%"
		query = query.Where("LOWER(emails.from_address) LIKE ? OR LOWER(emails.from_name) LIKE ?", like, like)
	}
	if recipient := strings.TrimSpace(input.To); recipient != "" {
		like := "%" + strings.ToLower(recipient) + "%"
		query = query.Where("EXISTS (SELECT 1 FROM recipients WHERE recipients.email_id = emails.id AND (LOWER(recipients.address) LIKE ? OR LOWER(recipients.name) LIKE ?))", like, like)
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
	if input.HasAttachments != nil {
		if *input.HasAttachments {
			query = query.Where("EXISTS (SELECT 1 FROM attachments WHERE attachments.email_id = emails.id)")
		} else {
			query = query.Where("NOT EXISTS (SELECT 1 FROM attachments WHERE attachments.email_id = emails.id)")
		}
	}
	if status := strings.TrimSpace(input.DeliveryStatus); status != "" {
		query = query.Where("emails.delivery_status = ?", status)
	}
	if labelID := strings.TrimSpace(input.LabelID); labelID != "" {
		query = query.Where("EXISTS (SELECT 1 FROM email_label_mappings WHERE email_label_mappings.email_id = emails.id AND email_label_mappings.label_id = ?)", labelID)
	}
	if folder := strings.TrimSpace(strings.ToLower(input.Folder)); folder != "" {
		query = query.Where("emails.folder = ?", folder)
	}
	return query
}

type ListDmarcReportsInput struct {
	Offset int
	Take   int
	Status string
	Domain string
}

func (s *EmailService) ListDmarcReports(ctx context.Context, accountID uuid.UUID, input ListDmarcReportsInput) ([]database.DmarcReport, int64, error) {
	if input.Take <= 0 {
		input.Take = 20
	}
	if input.Take > 200 {
		input.Take = 200
	}
	if input.Offset < 0 {
		input.Offset = 0
	}
	query := s.db.WithContext(ctx).Model(&database.DmarcReport{}).Where("account_id = ?", accountID)
	if status := strings.TrimSpace(input.Status); status != "" {
		query = query.Where("parse_status = ?", status)
	}
	if domain := strings.TrimSpace(strings.ToLower(input.Domain)); domain != "" {
		query = query.Where("LOWER(domain) = ?", domain)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var reports []database.DmarcReport
	if err := query.Order("created_at DESC").Offset(input.Offset).Limit(input.Take).
		Preload("Records").Preload("Email").Preload("Email.Attachments").Find(&reports).Error; err != nil {
		return nil, 0, err
	}
	return reports, total, nil
}

func (s *EmailService) GetDmarcReport(ctx context.Context, accountID uuid.UUID, id string) (*database.DmarcReport, error) {
	var report database.DmarcReport
	err := s.db.WithContext(ctx).Where("id = ? AND account_id = ?", id, accountID).
		Preload("Records").Preload("Email").Preload("Email.Attachments").First(&report).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &report, nil
}

// ListBlockRules returns block rules owned by the account.
func (s *EmailService) ListBlockRules(ctx context.Context, accountID uuid.UUID) ([]database.MailBlockRule, error) {
	var rules []database.MailBlockRule
	err := s.db.WithContext(ctx).Where("account_id = ?", accountID).Order("created_at DESC").Find(&rules).Error
	return rules, err
}

// CreateBlockRule validates scope ownership before persisting a sender/domain rule.
func (s *EmailService) CreateBlockRule(ctx context.Context, accountID uuid.UUID, input CreateBlockRuleInput) (*database.MailBlockRule, error) {
	pattern := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(input.Pattern, "@")))
	if pattern == "" {
		return nil, fmt.Errorf("block pattern is required")
	}
	rule := database.MailBlockRule{AccountID: accountID, Pattern: pattern}
	switch strings.ToLower(strings.TrimSpace(input.Scope)) {
	case "mailbox":
		var mailbox database.Mailbox
		if err := s.db.WithContext(ctx).Where("id = ? AND account_id = ?", input.MailboxID, accountID).First(&mailbox).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, ErrNotFound
			}
			return nil, err
		}
		rule.MailboxID = &mailbox.ID
	case "workspace":
		if strings.TrimSpace(input.WorkspaceID) == "" {
			return nil, fmt.Errorf("workspace_id is required for workspace rules")
		}
		if err := s.authorizeWorkspaceMember(ctx, input.WorkspaceID, accountID); err != nil {
			return nil, err
		}
		rule.WorkspaceID = &input.WorkspaceID
	default:
		return nil, fmt.Errorf("scope must be mailbox or workspace")
	}
	if strings.Contains(pattern, "@") {
		rule.MatchType = "address"
	} else {
		rule.MatchType = "domain"
	}
	if err := s.db.WithContext(ctx).Create(&rule).Error; err != nil {
		return nil, err
	}
	return &rule, nil
}

func (s *EmailService) DeleteBlockRule(ctx context.Context, accountID uuid.UUID, id string) error {
	result := s.db.WithContext(ctx).Where("id = ? AND account_id = ?", id, accountID).Delete(&database.MailBlockRule{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
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

// OpenEmailEML returns a stream that serializes the account-owned message as RFC 5322 bytes.
func (s *EmailService) OpenEmailEML(ctx context.Context, accountID uuid.UUID, emailID string) (io.ReadCloser, error) {
	if _, err := s.GetEmail(ctx, accountID, emailID); err != nil {
		return nil, err
	}
	source, err := s.OpenProtocolMessage(ctx, emailID)
	if err != nil {
		return nil, err
	}
	reader, writer := io.Pipe()
	go func() {
		writer.CloseWithError(mailmime.Render(ctx, source, writer))
	}()
	return reader, nil
}

// ListThreads returns one summary per conversation, newest activity first.
func (s *EmailService) ListThreads(ctx context.Context, accountID uuid.UUID, mailboxID string, input ListInput) ([]ThreadSummary, int64, error) {
	query := s.emailListQuery(ctx, accountID, input)
	if strings.TrimSpace(mailboxID) != "" {
		query = query.Where("emails.mailbox_id = ?", mailboxID)
	}
	var messages []database.Email
	if err := query.Order("emails.created_at DESC").Preload("Recipients").Preload("Mailbox").Preload("Labels").Find(&messages).Error; err != nil {
		return nil, 0, err
	}
	groups := make(map[string]*ThreadSummary)
	ordered := make([]string, 0)
	for _, message := range messages {
		threadID := message.ID
		if message.ThreadID != nil && *message.ThreadID != "" {
			threadID = *message.ThreadID
		}
		group := groups[threadID]
		if group == nil {
			group = &ThreadSummary{ID: threadID, MailboxID: message.MailboxID, Subject: message.Subject, LatestAt: message.CreatedAt, LatestMessage: emailSummary(message)}
			groups[threadID] = group
			ordered = append(ordered, threadID)
		}
		group.MessageCount++
		if !message.IsRead {
			group.UnreadCount++
		}
		for _, recipient := range message.Recipients {
			group.Participants = appendUnique(group.Participants, recipient.Address)
		}
		if message.FromAddress != "" {
			group.Participants = appendUnique(group.Participants, message.FromAddress)
		}
	}
	total := int64(len(ordered))
	take := input.Take
	if take <= 0 {
		take = 20
	}
	start, end := input.Offset, input.Offset+take
	if start > len(ordered) {
		start = len(ordered)
	}
	if end > len(ordered) {
		end = len(ordered)
	}
	items := make([]ThreadSummary, 0, end-start)
	for _, id := range ordered[start:end] {
		items = append(items, *groups[id])
	}
	return items, total, nil

}
func (s *EmailService) GetThread(ctx context.Context, accountID uuid.UUID, threadID string) ([]database.Email, error) {
	var messages []database.Email
	if err := s.db.WithContext(ctx).Where("account_id = ? AND thread_id = ?", accountID, threadID).Order("created_at ASC").Preload("Recipients").Preload("Attachments").Preload("Mailbox").Preload("Labels").Find(&messages).Error; err != nil {
		return nil, err
	}
	if len(messages) == 0 {
		return nil, ErrNotFound
	}
	return messages, nil
}

func appendUnique(values []string, value string) []string {
	for _, item := range values {
		if item == value {
			return values
		}
	}
	return append(values, value)
}

// GetMailboxQuota returns the shared storage usage and limit for the
// mailbox's workspace.
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

// GetWorkspaceMailboxUsage returns the current mailbox count and its plan
// limit for a workspace.
func (s *EmailService) GetWorkspaceMailboxUsage(ctx context.Context, accountID uuid.UUID, workspaceID string) (WorkspaceMailboxUsage, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return WorkspaceMailboxUsage{}, fmt.Errorf("workspace_id is required")
	}
	if err := s.authorizeWorkspaceMember(ctx, workspaceID, accountID); err != nil {
		return WorkspaceMailboxUsage{}, err
	}
	if s.workspace == nil {
		return WorkspaceMailboxUsage{}, ErrWorkspaceUnavailable
	}
	limit, err := s.workspace.MailboxLimit(ctx, workspaceID)
	if err != nil {
		return WorkspaceMailboxUsage{}, fmt.Errorf("get workspace mailbox limit: %w", err)
	}
	var used int64
	if err := s.db.WithContext(ctx).Model(&database.Mailbox{}).Where("workspace_id = ?", workspaceID).Count(&used).Error; err != nil {
		return WorkspaceMailboxUsage{}, fmt.Errorf("count workspace mailboxes: %w", err)
	}
	remaining := limit - used
	if remaining < 0 {
		remaining = 0
	}
	return WorkspaceMailboxUsage{WorkspaceID: workspaceID, Used: used, Limit: limit, Remaining: remaining}, nil
}

// GetWorkspaceCustomDomainUsage returns the current custom domain count and
// its plan limit for a workspace.
func (s *EmailService) GetWorkspaceCustomDomainUsage(ctx context.Context, accountID uuid.UUID, workspaceID string) (WorkspaceCustomDomainUsage, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return WorkspaceCustomDomainUsage{}, fmt.Errorf("workspace_id is required")
	}
	if err := s.authorizeWorkspaceMember(ctx, workspaceID, accountID); err != nil {
		return WorkspaceCustomDomainUsage{}, err
	}
	if s.workspace == nil {
		return WorkspaceCustomDomainUsage{}, ErrWorkspaceUnavailable
	}
	limit, err := s.workspace.CustomDomainLimit(ctx, workspaceID)
	if err != nil {
		return WorkspaceCustomDomainUsage{}, fmt.Errorf("get workspace custom domain limit: %w", err)
	}
	var used int64
	if err := s.db.WithContext(ctx).Model(&database.CustomDomain{}).Where("workspace_id = ?", workspaceID).Count(&used).Error; err != nil {
		return WorkspaceCustomDomainUsage{}, fmt.Errorf("count workspace custom domains: %w", err)
	}
	remaining := limit - used
	if remaining < 0 {
		remaining = 0
	}
	return WorkspaceCustomDomainUsage{WorkspaceID: workspaceID, Used: used, Limit: limit, Remaining: remaining}, nil
}

// GetWorkspaceSendUsage returns the workspace-scoped outbound send usage for
// the current day and calendar month.
func (s *EmailService) GetWorkspaceSendUsage(ctx context.Context, accountID uuid.UUID, workspaceID string) (WorkspaceSendUsage, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return WorkspaceSendUsage{}, fmt.Errorf("workspace_id is required")
	}
	if err := s.authorizeWorkspaceMember(ctx, workspaceID, accountID); err != nil {
		return WorkspaceSendUsage{}, err
	}
	limits, err := s.workspaceSendLimits(ctx, workspaceID)
	if err != nil {
		return WorkspaceSendUsage{}, err
	}
	now := time.Now().UTC()
	dayStart := now.Truncate(24 * time.Hour)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	var usages []database.MailSendUsage
	if err := s.db.WithContext(ctx).Where(
		"workspace_id = ? AND scope IN ? AND period_start IN ?",
		workspaceID, []string{"workspace:day", "workspace:month"}, []time.Time{dayStart, monthStart},
	).Find(&usages).Error; err != nil {
		return WorkspaceSendUsage{}, fmt.Errorf("load workspace send usage: %w", err)
	}

	daily := SendUsage{Limit: limits.WorkspaceDaily}
	monthly := SendUsage{Limit: limits.WorkspaceMonthly}
	for _, usage := range usages {
		switch {
		case usage.Scope == "workspace:day" && usage.PeriodStart.Equal(dayStart):
			daily.Used = usage.SentCount
		case usage.Scope == "workspace:month" && usage.PeriodStart.Equal(monthStart):
			monthly.Used = usage.SentCount
		}
	}
	daily.Remaining = daily.Limit - daily.Used
	if daily.Remaining < 0 {
		daily.Remaining = 0
	}
	monthly.Remaining = monthly.Limit - monthly.Used
	if monthly.Remaining < 0 {
		monthly.Remaining = 0
	}
	return WorkspaceSendUsage{WorkspaceID: workspaceID, Daily: daily, Monthly: monthly}, nil
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
	threadID, err := s.resolveThreadID(ctx, accountID, input.ThreadID, input.ReplyToID)
	if err != nil {
		return nil, err
	}
	input.ThreadID = threadID
	mailboxLimit, err := s.workspaceMailboxLimit(ctx, mailbox.WorkspaceID)
	if err != nil {
		return nil, err
	}
	attachmentReferences, err := s.resolveAttachmentReferences(ctx, mailbox, input.AttachmentIDs)
	if err != nil {
		return nil, err
	}
	var sendLimits workspace.SendLimits
	if !input.IsDraft && input.ScheduledAt == nil {
		sendLimits, err = s.workspaceSendLimits(ctx, mailbox.WorkspaceID)
		if err != nil {
			return nil, err
		}
	}

	fromAddress := s.normalizeFromAddress(mailbox.Address)
	if input.FromAliasID != "" {
		var alias database.MailboxAlias
		if err := s.db.WithContext(ctx).Where("id = ? AND mailbox_id = ?", input.FromAliasID, mailbox.ID).First(&alias).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, ErrNotFound
			}
			return nil, err
		}
		fromAddress = alias.Address
		if alias.Name != "" {
			mailbox.Name = alias.Name
		}
	}
	if !input.IsDraft {
		if err := s.ensureMailboxSendingIdentity(ctx, mailbox, fromAddress); err != nil {
			return nil, err
		}
	}
	email := database.Email{
		AccountID:      accountID,
		MailboxID:      input.MailboxID,
		ThreadID:       &threadID,
		Subject:        input.Subject,
		Body:           input.Body,
		FromAddress:    fromAddress,
		FromName:       mailbox.Name,
		IsDraft:        input.IsDraft,
		DeliveryStatus: "draft",
		Folder:         folderSent,
		ContentType:    normalizeContentType(input.ContentType),
	}
	if input.IsDraft {
		email.Folder = folderDrafts
	}
	if input.ScheduledAt != nil && !input.IsDraft {
		if !input.ScheduledAt.After(time.Now()) {
			return nil, fmt.Errorf("scheduled_at must be in the future")
		}
		email.ScheduledAt = input.ScheduledAt
		email.DeliveryStatus = "scheduled"
	}
	email.RawSizeBytes = outgoingRawSize(email, input)
	if !input.IsDraft && email.ScheduledAt == nil {
		email.DeliveryStatus = "pending"
		now := time.Now()
		email.LastDeliveryAttemptAt = &now
		email.DeliveryAttempts = 1
	}

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if !input.IsDraft && input.ScheduledAt == nil {
			if err := reserveOutboundSend(tx, mailbox, sendLimits, time.Now()); err != nil {
				return err
			}
		}
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
		for _, reference := range attachmentReferences {
			id := strings.TrimSpace(reference.StorageKey)
			if err := tx.Create(&database.Attachment{EmailID: email.ID, Position: reference.Position, Filename: reference.Filename, MimeType: reference.MimeType, Size: reference.Size, StorageKey: &id, File: reference.File, ContentID: reference.ContentID, Disposition: reference.Disposition}).Error; err != nil {
				return err
			}
		}
		if err := s.storeProtocolSourceTx(tx, &email, nil, fromAddress); err != nil {
			return err
		}
		folder := "Sent"
		if input.IsDraft {
			folder = "Drafts"
		}
		return s.addFolderMembershipTx(tx, mailbox.ID, folder, email.ID)
	})
	if err != nil {
		return nil, err
	}
	if err := s.enforceWorkspaceMailboxQuota(ctx, mailbox.WorkspaceID, mailboxLimit); err != nil {
		return nil, err
	}
	s.publishMailEvent(ctx, email.AccountID.String(), "mail.created", &email)
	if input.IsDraft || email.ScheduledAt != nil {
		return &email, nil
	}
	message := outgoingRelayMessage(mailbox, fromAddress, input)
	message.Attachments = relayAttachmentMetadata(attachmentReferences)
	if err := s.deliverStoredEmail(ctx, &email, message); err != nil {
		return nil, err
	}

	return &email, nil
}

// ensureMailboxSendingIdentity keeps SES's account-wide identity namespace
// from bypassing workspace ownership. The service's canonical mail host stays
// available for its existing operator-managed identity; other addresses need a
// verified workspace custom domain for its domain.
func (s *EmailService) ensureMailboxSendingIdentity(ctx context.Context, mailbox database.Mailbox, fromAddress string) error {
	if s.identities == nil {
		return nil
	}
	parts := strings.Split(fromAddress, "@")
	if len(parts) != 2 || parts[1] == "" {
		return fmt.Errorf("mailbox address must be a full email address when SES is enabled")
	}
	if strings.EqualFold(parts[1], s.domain) {
		return nil
	}
	var count int64
	if err := s.db.WithContext(ctx).Model(&database.CustomDomain{}).
		Where("workspace_id = ? AND provider = ? AND verified_for_sending_status = ? AND domain = ?", mailbox.WorkspaceID, "ses", true, strings.ToLower(parts[1])).
		Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("a verified SES custom domain is required to send from %s", fromAddress)
	}
	return nil
}

func (s *EmailService) resolveThreadID(ctx context.Context, accountID uuid.UUID, requestedThreadID, replyToID string) (string, error) {
	requestedThreadID, replyToID = strings.TrimSpace(requestedThreadID), strings.TrimSpace(replyToID)
	if requestedThreadID != "" && replyToID != "" {
		return "", fmt.Errorf("provide thread_id or reply_to_id, not both")
	}
	if replyToID != "" {
		var parent database.Email
		if err := s.db.WithContext(ctx).Where("id = ? AND account_id = ?", replyToID, accountID).First(&parent).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return "", ErrNotFound
			}
			return "", err
		}
		if parent.ThreadID != nil && *parent.ThreadID != "" {
			return *parent.ThreadID, nil
		}
		return parent.ID, nil
	}
	if requestedThreadID != "" {
		var count int64
		if err := s.db.WithContext(ctx).Model(&database.Email{}).Where("thread_id = ? AND account_id = ?", requestedThreadID, accountID).Count(&count).Error; err != nil {
			return "", err
		}
		if count == 0 {
			return "", ErrNotFound
		}
		return requestedThreadID, nil
	}
	return database.NewID(), nil
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
		ContentType:   normalizeContentType(input.ContentType),
		ThreadID:      input.ThreadID,
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

func relayAttachmentMetadata(references []AttachmentReference) []relay.AttachmentMetadata {
	metadata := make([]relay.AttachmentMetadata, 0, len(references))
	for _, reference := range references {
		metadata = append(metadata, relay.AttachmentMetadata{ID: reference.StorageKey, Filename: reference.Filename, MimeType: reference.MimeType, Size: reference.Size, ContentID: reference.ContentID, Disposition: reference.Disposition})
	}
	return metadata
}

func relayMessageFromEmail(email database.Email) relay.Message {
	message := relay.Message{
		FromAddress: email.FromAddress,
		FromName:    email.FromName,
		Subject:     email.Subject,
		Body:        email.Body,
		ContentType: email.ContentType,
		ThreadID:    dereferenceString(email.ThreadID),
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
		if id := attachmentFileID(attachment); id != "" {
			message.AttachmentIDs = append(message.AttachmentIDs, id)
			message.Attachments = append(message.Attachments, relay.AttachmentMetadata{ID: id, Filename: attachment.Filename, MimeType: attachment.MimeType, Size: attachment.Size, ContentID: attachment.ContentID, Disposition: attachment.Disposition})
		}
	}
	return message
}

// DeliverLocal stores copies of an outbound message in the mailboxes selected
// by the direct SMTP adapter. The adapter calls this only after DNS confirms
// that the recipient domain's MX points at this server.
func (s *EmailService) DeliverLocal(ctx context.Context, message relay.Message, recipients []string) error {
	attachmentReferences, err := s.resolveAttachmentReferences(ctx, database.Mailbox{}, message.AttachmentIDs)
	if err != nil {
		return err
	}
	if len(message.Attachments) > 0 {
		metadata := make(map[string]relay.AttachmentMetadata, len(message.Attachments))
		for _, item := range message.Attachments {
			metadata[item.ID] = item
		}
		for index := range attachmentReferences {
			item := metadata[attachmentReferences[index].StorageKey]
			if item.Filename != "" {
				attachmentReferences[index].Filename = item.Filename
			}
			if item.MimeType != "" {
				attachmentReferences[index].MimeType = item.MimeType
			}
			attachmentReferences[index].ContentID = item.ContentID
			attachmentReferences[index].Disposition = item.Disposition
		}
	}
	now := time.Now()
	delivered := make(map[string]struct{}, len(recipients))
	for _, recipient := range recipients {
		address := strings.ToLower(strings.TrimSpace(recipient))
		if _, seen := delivered[address]; seen {
			continue
		}
		delivered[address] = struct{}{}

		mailbox, err := s.ResolveLocalMailbox(ctx, address)
		if err != nil {
			return fmt.Errorf("local recipient %q: %w", recipient, err)
		}
		if _, err := s.ReceiveEmail(ctx, ReceiveEmailInput{
			MailboxID: mailbox.ID, ThreadID: message.ThreadID, FromAddress: message.FromAddress,
			FromName: message.FromName, Subject: message.Subject, Body: message.Body,
			ContentType: message.ContentType, To: localRecipientInputs(message.To, "to"),
			Cc: localRecipientInputs(message.Cc, "cc"), AttachmentReferences: attachmentReferences,
			SentAt: &now, DeliveredTo: []string{recipient},
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

// ReserveOutboundSend atomically reserves one outbound send against the
// authenticated mailbox and its workspace limits.
func (s *EmailService) ReserveOutboundSend(ctx context.Context, mailboxID string) error {
	var mailbox database.Mailbox
	if err := s.db.WithContext(ctx).Where("id = ?", mailboxID).First(&mailbox).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}
	limits, err := s.workspaceSendLimits(ctx, mailbox.WorkspaceID)
	if err != nil {
		return err
	}
	now := time.Now()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return reserveOutboundSend(tx, mailbox, limits, now)
	})
}

// SendOutbound relays the external leg of an SMTP submission through the
// configured provider. Local recipients are delivered directly by the SMTP
// session, so the message passed here only contains recipients that are not
// served by this instance. The submitting client owns its Sent-folder copy via
// IMAP APPEND and no Sent record is created here.
func (s *EmailService) SendOutbound(ctx context.Context, message relay.Message) error {
	if s.relay == nil {
		return ErrOutboundRelayUnavailable
	}
	if _, err := s.relay.Send(ctx, message); err != nil {
		return err
	}
	return nil
}

// ReceiveEmail stores a message received from another mail service. Raw
// attachments are uploaded under the destination mailbox owner and workspace,
// whereas outgoing messages retain the client-provided DysonFS IDs.
func (s *EmailService) ReceiveEmail(ctx context.Context, input ReceiveEmailInput) (*database.Email, error) {
	if strings.TrimSpace(input.MailboxID) == "" {
		return nil, fmt.Errorf("mailbox_id is required")
	}
	// Incoming message bytes are not guaranteed to be UTF-8; PostgreSQL text
	// columns reject invalid sequences with SQLSTATE 22021, so every string
	// that reaches a column is normalized here regardless of producer.
	input.Subject = mailtext.ToValidUTF8(input.Subject)
	input.Body = mailtext.ToValidUTF8(input.Body)
	input.FromAddress = mailtext.ToValidUTF8(input.FromAddress)
	input.FromName = mailtext.ToValidUTF8(input.FromName)
	input.EnvelopeFrom = mailtext.ToValidUTF8(input.EnvelopeFrom)
	var messageID *string
	if mid := strings.TrimSpace(input.MessageID); mid != "" {
		messageID = &mid
	}
	for i := range input.To {
		input.To[i].Address = mailtext.ToValidUTF8(input.To[i].Address)
		input.To[i].Name = mailtext.ToValidUTF8(input.To[i].Name)
	}
	for i := range input.Cc {
		input.Cc[i].Address = mailtext.ToValidUTF8(input.Cc[i].Address)
		input.Cc[i].Name = mailtext.ToValidUTF8(input.Cc[i].Name)
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
	isDmarcIntake := input.DmarcIntake
	for _, recipient := range input.DeliveredTo {
		if isDmarcAddress(recipient) {
			isDmarcIntake = true
			break
		}
	}
	if !isDmarcIntake {
		for _, recipient := range append(append([]RecipientInput{}, input.To...), input.Cc...) {
			if isDmarcAddress(recipient.Address) {
				isDmarcIntake = true
				break
			}
		}
	}
	logging.Log.Info().Str("mailbox_id", mailbox.ID).Int("attachment_count", len(input.Attachments)+len(input.AttachmentReferences)).Msg("receiving email")
	mailboxLimit, err := s.workspaceMailboxLimit(ctx, mailbox.WorkspaceID)
	if err != nil {
		return nil, err
	}
	attachments := make([]AttachmentInput, 0, len(input.AttachmentReferences)+len(input.Attachments))
	stagedReferences := make([]AttachmentReference, 0, len(input.Attachments))
	for _, reference := range input.AttachmentReferences {
		if strings.TrimSpace(reference.StorageKey) == "" && (reference.File == nil || strings.TrimSpace(reference.File.ID) == "") {
			return nil, fmt.Errorf("attachment DysonFS file ID is required")
		}
		if reference.StorageKey == "" && reference.File != nil {
			reference.StorageKey = reference.File.ID
		}
		attachments = append(attachments, attachmentInputFromReference(reference))
	}
	if len(input.Attachments) > 0 {
		staged, err := s.StageIncomingAttachments(ctx, mailbox.ID, input.Attachments)
		if err != nil {
			return nil, err
		}
		stagedReferences = staged
		for _, reference := range staged {
			attachments = append(attachments, attachmentInputFromReference(reference))
		}
	}

	email := database.Email{
		AccountID:       mailbox.AccountID,
		MailboxID:       mailbox.ID,
		MessageID:       messageID,
		Subject:         input.Subject,
		Body:            input.Body,
		FromAddress:     input.FromAddress,
		FromName:        input.FromName,
		SentAt:          input.SentAt,
		Folder:          folderInbox,
		ContentType:     normalizeContentType(input.ContentType),
		OmitContentType: input.OmitContentType,
		IsDmarcIntake:   isDmarcIntake,
		Authentication:  input.Authentication,
	}
	threadID := strings.TrimSpace(input.ThreadID)
	if threadID == "" {
		threadID = database.NewID()
	}
	email.ThreadID = &threadID
	if s.shouldRouteToSpam(ctx, mailbox, input.FromAddress, input.Subject, input.Body) {
		email.Folder = folderSpam
		now := time.Now()
		email.SpamAt = &now
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
		for position, attachment := range attachments {
			if err := tx.Create(&database.Attachment{EmailID: email.ID, Position: position, Filename: attachment.Filename, MimeType: attachment.MimeType, Size: attachment.Size, StorageKey: attachment.StorageKey, File: attachment.File, ContentID: attachment.ContentID, Disposition: attachment.Disposition}).Error; err != nil {
				return err
			}
		}
		if err := s.storeProtocolSourceTx(tx, &email, nil, input.EnvelopeFrom); err != nil {
			return err
		}
		if isDmarcIntake {
			return nil
		}
		return s.addInboxMembershipTx(tx, mailbox.ID, email.ID)
	})
	if err != nil {
		for _, reference := range stagedReferences {
			if id := strings.TrimSpace(reference.StorageKey); id != "" && s.files != nil {
				_ = s.files.DeleteAttachment(context.Background(), id)
			}
		}
		logging.Log.Error().Err(err).Str("mailbox_id", mailbox.ID).Msg("failed to persist incoming email")
		return nil, err
	}
	logging.Log.Info().Str("email_id", email.ID).Str("mailbox_id", mailbox.ID).Msg("email received")
	if err := s.enforceWorkspaceMailboxQuota(ctx, mailbox.WorkspaceID, mailboxLimit); err != nil {
		return nil, err
	}
	if email.Folder == folderInbox {
		// Inbox mail is inspected even without a notifier: the insight also
		// stores the summary that listings show as the message preview.
		insight := s.inspectInboxEmail(ctx, &email)
		if s.notifier != nil {
			if err := s.notifier.SendEmailNotification(ctx, emailNotificationPayload(&email, insight)); err != nil {
				logging.Log.Warn().Err(err).Str("account_id", mailbox.AccountID.String()).Msg("failed to send incoming email notification")
			}
		}
	}
	s.publishMailEvent(ctx, email.AccountID.String(), "mail.created", &email)
	if isDmarcIntake {
		if err := s.persistDmarcReports(ctx, &email, nil); err != nil {
			logging.Log.Warn().Err(err).Str("email_id", email.ID).Msg("failed to parse DMARC report")
		}
	}
	forwardRecipients := input.DeliveredTo
	if len(forwardRecipients) == 0 {
		for _, recipient := range input.To {
			forwardRecipients = append(forwardRecipients, recipient.Address)
		}
	}
	s.forwardIncomingEmail(ctx, mailbox, email, forwardRecipients, len(attachments) > 0)
	return &email, nil
}

func isDmarcAddress(address string) bool {
	address = strings.ToLower(strings.TrimSpace(address))
	at := strings.LastIndex(address, "@")
	return at > 0 && address[:at] == "dmarc"
}

func (s *EmailService) persistDmarcReports(ctx context.Context, email *database.Email, raw []byte) error {
	var reports []dmarc.Report
	var parseErr error
	if len(raw) > 0 {
		reports, parseErr = dmarc.Parse(raw)
	} else if s.files == nil {
		parseErr = fmt.Errorf("attachment byte store is not configured")
	} else {
		var attachments []database.Attachment
		if err := s.db.WithContext(ctx).Where("email_id = ?", email.ID).Order("position ASC").Find(&attachments).Error; err != nil {
			parseErr = err
		} else {
			for _, attachment := range attachments {
				id := attachmentFileID(attachment)
				if id == "" {
					continue
				}
				reader, err := s.files.OpenAttachment(ctx, id)
				if err != nil {
					parseErr = err
					break
				}
				partReports, reportErr := dmarc.ParseAttachment(attachment.Filename, reader.Content)
				_ = reader.Content.Close()
				if reportErr == nil {
					reports = append(reports, partReports...)
				} else if strings.HasSuffix(strings.ToLower(attachment.Filename), ".xml") || strings.HasSuffix(strings.ToLower(attachment.Filename), ".gz") || strings.HasSuffix(strings.ToLower(attachment.Filename), ".zip") {
					parseErr = reportErr
					break
				}
			}
			if parseErr == nil && len(reports) == 0 {
				parseErr = errors.New("no DMARC report attachment found")
			}
		}
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if parseErr != nil {
			return tx.Create(&database.DmarcReport{
				EmailID:     email.ID,
				AccountID:   email.AccountID,
				MailboxID:   email.MailboxID,
				ParseStatus: "failed",
				ParseError:  parseErr.Error(),
			}).Error
		}
		for _, report := range reports {
			row := database.DmarcReport{
				EmailID:         email.ID,
				AccountID:       email.AccountID,
				MailboxID:       email.MailboxID,
				AttachmentName:  report.AttachmentName,
				ReporterOrg:     report.ReporterOrg,
				ReporterEmail:   report.ReporterEmail,
				ReportID:        report.ReportID,
				DateBegin:       report.DateBegin,
				DateEnd:         report.DateEnd,
				Domain:          report.Domain,
				ADKIM:           report.ADKIM,
				ASPF:            report.ASPF,
				Policy:          report.Policy,
				SubdomainPolicy: report.SubdomainPolicy,
				Percentage:      report.Percentage,
				ParseStatus:     "parsed",
			}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
			for _, record := range report.Records {
				if err := tx.Create(&database.DmarcReportRecord{
					ReportID:     row.ID,
					SourceIP:     record.SourceIP,
					Count:        record.Count,
					Disposition:  record.Disposition,
					DKIM:         record.DKIM,
					SPF:          record.SPF,
					HeaderFrom:   record.HeaderFrom,
					EnvelopeFrom: record.EnvelopeFrom,
					EnvelopeTo:   record.EnvelopeTo,
				}).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// forwardIncomingEmail is deliberately best-effort: a forwarding failure never
// rejects the original SMTP delivery. Attachments are not forwarded until a
// relay attachment byte-source is configured, preventing silent data loss.
func (s *EmailService) forwardIncomingEmail(ctx context.Context, mailbox database.Mailbox, email database.Email, recipients []string, hasAttachments bool) {
	if s.relay == nil || len(recipients) == 0 || strings.HasPrefix(strings.ToLower(strings.TrimSpace(email.Subject)), "fwd:") {
		return
	}
	addresses := make([]string, 0, len(recipients))
	for _, recipient := range recipients {
		addresses = append(addresses, strings.ToLower(strings.TrimSpace(recipient)))
	}
	var aliases []database.MailboxAlias
	if err := s.db.WithContext(ctx).Where("mailbox_id = ? AND LOWER(address) IN ?", mailbox.ID, addresses).Find(&aliases).Error; err != nil || len(aliases) == 0 {
		if err != nil {
			logging.Log.Warn().Err(err).Str("email_id", email.ID).Msg("look up forwarding aliases")
		}
		return
	}
	aliasByID := make(map[string]database.MailboxAlias, len(aliases))
	aliasIDs := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		aliasByID[alias.ID], aliasIDs = alias, append(aliasIDs, alias.ID)
	}
	var rules []database.MailForwarding
	if err := s.db.WithContext(ctx).Where("alias_id IN ?", aliasIDs).Find(&rules).Error; err != nil {
		logging.Log.Warn().Err(err).Str("email_id", email.ID).Msg("look up mail forwarding rules")
		return
	}
	var attachmentIDs []string
	var attachmentMetadata []relay.AttachmentMetadata
	if hasAttachments {
		var attachments []database.Attachment
		if err := s.db.WithContext(ctx).Where("email_id = ?", email.ID).Order("position ASC").Find(&attachments).Error; err != nil {
			logging.Log.Warn().Err(err).Str("email_id", email.ID).Msg("failed to load forwarding attachments")
			return
		}
		for _, attachment := range attachments {
			if id := attachmentFileID(attachment); id != "" {
				attachmentIDs = append(attachmentIDs, id)
				attachmentMetadata = append(attachmentMetadata, relay.AttachmentMetadata{ID: id, Filename: attachment.Filename, MimeType: attachment.MimeType, Size: attachment.Size, ContentID: attachment.ContentID, Disposition: attachment.Disposition})
			}
		}
		if len(attachmentIDs) != len(attachments) {
			logging.Log.Warn().Str("email_id", email.ID).Msg("failed to forward email with missing attachment source")
			return
		}
	}
	sent := map[string]struct{}{}
	for _, rule := range rules {
		alias := aliasByID[rule.AliasID]
		key := alias.Address + "\x00" + rule.Destination
		if _, ok := sent[key]; ok {
			continue
		}
		sent[key] = struct{}{}
		if err := s.ensureMailboxSendingIdentity(ctx, mailbox, alias.Address); err != nil {
			logging.Log.Warn().Err(err).Str("email_id", email.ID).Str("alias", alias.Address).Msg("skipped forwarding from unverified custom domain")
			continue
		}
		name := alias.Name
		if name == "" {
			name = mailbox.Name
		}
		message := relay.Message{FromAddress: alias.Address, FromName: name, To: []string{rule.Destination}, Subject: "Fwd: " + email.Subject, Body: forwardedBody(email), ContentType: "text/plain", ThreadID: dereferenceString(email.ThreadID), AttachmentIDs: attachmentIDs, Attachments: attachmentMetadata}
		if _, err := s.relay.Send(ctx, message); err != nil {
			logging.Log.Warn().Err(err).Str("email_id", email.ID).Str("destination", rule.Destination).Msg("failed to forward email")
		}
	}
}

func forwardedBody(email database.Email) string {
	return fmt.Sprintf("Forwarded message from %s\nSubject: %s\n\n%s", email.FromAddress, email.Subject, email.Body)
}

func (s *EmailService) storeIncomingAttachment(ctx context.Context, mailbox database.Mailbox, attachment IncomingAttachment) (AttachmentInput, error) {
	if s.files == nil {
		return AttachmentInput{}, fmt.Errorf("attachment uploads are not configured")
	}
	file, err := s.files.UploadAttachment(ctx, filesystem.AttachmentUpload{
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
	return AttachmentInput{Filename: attachment.Filename, MimeType: attachment.MimeType, Size: attachment.Size, StorageKey: &file.ID, File: &file, ContentID: attachment.ContentID, Disposition: attachment.Disposition}, nil
}

// StageIncomingAttachments uploads transient attachment readers before message
// persistence. The returned references contain no attachment bytes.
func (s *EmailService) StageIncomingAttachments(ctx context.Context, mailboxID string, incoming []IncomingAttachment) ([]AttachmentReference, error) {
	var mailbox database.Mailbox
	if err := s.db.WithContext(ctx).Where("id = ?", mailboxID).First(&mailbox).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	staged := make([]AttachmentReference, 0, len(incoming))
	cleanup := func() {
		if s.files == nil {
			return
		}
		for _, reference := range staged {
			if id := strings.TrimSpace(reference.StorageKey); id != "" {
				_ = s.files.DeleteAttachment(context.Background(), id)
			}
		}
	}
	for _, attachment := range incoming {
		if attachment.Content == nil {
			cleanup()
			return nil, fmt.Errorf("attachment content is required")
		}
		stored, err := s.storeIncomingAttachment(ctx, mailbox, attachment)
		if err != nil {
			cleanup()
			return nil, err
		}
		staged = append(staged, AttachmentReference{
			Filename: stored.Filename, MimeType: stored.MimeType, Size: stored.Size,
			StorageKey: dereferenceString(stored.StorageKey), File: stored.File,
			ContentID: stored.ContentID, Disposition: stored.Disposition, Position: len(staged),
		})
	}
	return staged, nil
}

func attachmentInputFromReference(reference AttachmentReference) AttachmentInput {
	id := strings.TrimSpace(reference.StorageKey)
	return AttachmentInput{
		Filename: reference.Filename, MimeType: reference.MimeType, Size: reference.Size,
		StorageKey: &id, File: reference.File, ContentID: reference.ContentID,
		Disposition: reference.Disposition, Position: reference.Position,
	}
}

func (s *EmailService) resolveAttachmentReferences(ctx context.Context, mailbox database.Mailbox, ids []string) ([]AttachmentReference, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if s.files == nil {
		return nil, fmt.Errorf("attachment byte store is not configured")
	}
	references := make([]AttachmentReference, 0, len(ids))
	for position, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, fmt.Errorf("attachment_ids cannot contain empty values")
		}
		reader, err := s.files.OpenAttachment(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("resolve attachment %q: %w", id, err)
		}
		_ = reader.Content.Close()
		if reader.File.ID == "" || reader.File.Name == "" || reader.File.MimeType == "" || reader.File.Size < 0 {
			return nil, fmt.Errorf("attachment %q has invalid metadata", id)
		}
		references = append(references, AttachmentReference{
			Filename: reader.File.Name, MimeType: reader.File.MimeType, Size: reader.File.Size,
			StorageKey: reader.File.ID, File: &reader.File, Position: position,
		})
	}
	return references, nil
}

// DeleteEmail moves a message to Trash. Permanent deletion remains reserved for
// the retention worker so users can recover accidental deletes.
func (s *EmailService) DeleteEmail(ctx context.Context, accountID uuid.UUID, id string) error {
	now := time.Now()
	result := s.db.WithContext(ctx).Model(&database.Email{}).Where("id = ? AND account_id = ?", id, accountID).Updates(map[string]any{"folder": folderTrash, "trashed_at": now})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// MoveEmail changes a message's mailbox folder and records the relevant state.
func (s *EmailService) MoveEmail(ctx context.Context, accountID uuid.UUID, id, folder string) error {
	folder = strings.ToLower(strings.TrimSpace(folder))
	if !validFolder(folder) {
		return fmt.Errorf("invalid folder")
	}
	updates := map[string]any{"folder": folder}
	now := time.Now()
	switch folder {
	case folderTrash:
		updates["trashed_at"] = now
	case folderSpam:
		updates["spam_at"] = now
	default:
		updates["trashed_at"] = nil
	}
	result := s.db.WithContext(ctx).Model(&database.Email{}).Where("id = ? AND account_id = ?", id, accountID).Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	if s.realtime != nil {
		if err := s.realtime.Publish(ctx, accountID.String(), "mail.moved", map[string]string{"id": id, "folder": folder}); err != nil {
			logging.Log.Warn().Err(err).Msg("publish mail move")
		}
	}
	return nil
}

func (s *EmailService) publishMailEvent(ctx context.Context, accountID, eventType string, email *database.Email) {
	if s.realtime == nil {
		return
	}
	// Realtime is a cache-invalidation signal, not an email transport. Sending
	// only identifiers keeps private body and attachment data on the HTTP API.
	notice := map[string]string{
		"mailbox_id": email.MailboxID,
		"email_id":   email.ID,
		"reason":     eventType,
	}
	if err := s.realtime.Publish(ctx, accountID, "mail.changed", notice); err != nil {
		logging.Log.Warn().Err(err).Str("event", eventType).Msg("publish mail websocket notice")
	}
}

// DeliverScheduledEmails attempts all due scheduled messages. It is safe to
// invoke periodically; claiming messages in a transaction avoids duplicate sends.
func (s *EmailService) DeliverScheduledEmails(ctx context.Context) (int64, error) {
	var emails []database.Email
	if err := s.db.WithContext(ctx).Where("scheduled_at <= ? AND delivery_status = ?", time.Now(), "scheduled").Preload("Mailbox").Preload("Recipients").Preload("Attachments").Find(&emails).Error; err != nil {
		return 0, err
	}
	var delivered int64
	for i := range emails {
		email := &emails[i]
		if email.Mailbox == nil {
			logging.Log.Warn().Str("email_id", email.ID).Msg("scheduled email mailbox no longer exists")
			continue
		}
		now := time.Now()
		limits, err := s.workspaceSendLimits(ctx, email.Mailbox.WorkspaceID)
		if err != nil {
			logging.Log.Warn().Err(err).Str("email_id", email.ID).Msg("load scheduled email send limits")
			continue
		}
		claimed := false
		err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			result := tx.Model(&database.Email{}).Where("id = ? AND delivery_status = ?", email.ID, "scheduled").Updates(map[string]any{"scheduled_at": nil, "delivery_status": "pending", "last_delivery_attempt_at": now, "delivery_attempts": email.DeliveryAttempts + 1})
			if result.Error != nil {
				return result.Error
			}
			claimed = result.RowsAffected > 0
			if !claimed {
				return nil
			}
			if err := reserveOutboundSend(tx, *email.Mailbox, limits, now); err != nil {
				return err
			}
			return nil
		})
		if err != nil {
			logging.Log.Warn().Err(err).Str("email_id", email.ID).Msg("scheduled email is over its send limit")
			continue
		}
		if !claimed {
			continue
		}
		email.ScheduledAt = nil
		email.DeliveryStatus = "pending"
		email.LastDeliveryAttemptAt = &now
		email.DeliveryAttempts++
		if err := s.deliverStoredEmail(ctx, email, relayMessageFromEmail(*email)); err != nil {
			logging.Log.Warn().Err(err).Str("email_id", email.ID).Msg("scheduled delivery failed")
		}
		delivered++
	}
	return delivered, nil
}

func validFolder(folder string) bool {
	switch folder {
	case folderInbox, folderSent, folderDrafts, folderSpam, folderTrash, folderArchive:
		return true
	default:
		return false
	}
}
func normalizeContentType(contentType string) string {
	if strings.EqualFold(strings.TrimSpace(contentType), "text/html") {
		return "text/html"
	}
	return "text/plain"
}

func dereferenceString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (s *EmailService) shouldRouteToSpam(ctx context.Context, mailbox database.Mailbox, fromAddress, subject, body string) bool {
	if s.isBlocked(ctx, mailbox, fromAddress) {
		return true
	}
	text := strings.ToLower(subject + " " + body)
	for _, phrase := range []string{"viagra", "bitcoin giveaway", "urgent wire transfer", "click here to claim"} {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func (s *EmailService) isBlocked(ctx context.Context, mailbox database.Mailbox, fromAddress string) bool {
	address := strings.ToLower(strings.TrimSpace(fromAddress))
	at := strings.LastIndex(address, "@")
	domain := address
	if at >= 0 {
		domain = address[at+1:]
	}
	var count int64
	if err := s.db.WithContext(ctx).Model(&database.MailBlockRule{}).Where("account_id = ? AND ((mailbox_id = ? AND match_type = 'address' AND pattern = ?) OR (mailbox_id = ? AND match_type = 'domain' AND pattern = ?) OR (workspace_id = ? AND match_type = 'address' AND pattern = ?) OR (workspace_id = ? AND match_type = 'domain' AND pattern = ?))", mailbox.AccountID, mailbox.ID, address, mailbox.ID, domain, mailbox.WorkspaceID, address, mailbox.WorkspaceID, domain).Count(&count).Error; err != nil {
		logging.Log.Warn().Err(err).Msg("evaluate mail block rules")
	}
	return count > 0
}

// PurgeArchivedEmails permanently removes message metadata whose 30-day
// archive retention window has elapsed. Unreferenced DysonFS attachment files
// are deleted only after database references are removed and rechecked.
func (s *EmailService) PurgeArchivedEmails(ctx context.Context) (int64, error) {
	var emails []database.Email
	if err := s.db.WithContext(ctx).Where("archive_delete_at IS NOT NULL AND archive_delete_at <= ?", time.Now()).Find(&emails).Error; err != nil {
		return 0, err
	}
	var purged int64
	var purgedIDs []string
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, email := range emails {
			var attachments []database.Attachment
			if err := tx.Where("email_id = ?", email.ID).Find(&attachments).Error; err != nil {
				return err
			}
			for _, attachment := range attachments {
				if id := attachmentFileID(attachment); id != "" {
					purgedIDs = append(purgedIDs, id)
				}
			}
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
	if err != nil {
		return purged, err
	}
	seen := map[string]struct{}{}
	for _, id := range purgedIDs {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		var references []database.Attachment
		if queryErr := s.db.WithContext(ctx).Find(&references).Error; queryErr != nil {
			logging.Log.Warn().Err(queryErr).Str("file_id", id).Msg("check shared attachment reference")
			continue
		}
		count := int64(0)
		for _, reference := range references {
			if attachmentFileID(reference) == id {
				count++
			}
		}
		if count == 0 && s.files != nil {
			if deleteErr := s.files.DeleteAttachment(ctx, id); deleteErr != nil {
				logging.Log.Warn().Err(deleteErr).Str("file_id", id).Msg("failed to delete purged attachment")
			}
		}
	}
	return purged, nil
}

// PurgeExpiredSendUsage removes old daily and monthly counters. Keeping a
// little over two months supports a full calendar month while bounding table
// growth.
func (s *EmailService) PurgeExpiredSendUsage(ctx context.Context) (int64, error) {
	result := s.db.WithContext(ctx).Where("period_start < ?", time.Now().UTC().Add(-sendUsageRetention)).Delete(&database.MailSendUsage{})
	return result.RowsAffected, result.Error
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
	var sharedUsed int64
	if s.sharedQuotaClient != nil {
		usage, err := s.sharedQuotaClient.GetUsedQuota(ctx, &gen.DyGetUsedQuotaRequest{WorkspaceId: workspaceID})
		if err != nil {
			return fmt.Errorf("get shared workspace storage usage: %w", err)
		}
		sharedUsed = usage.GetUsedBytes()
	}

	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		used := sharedUsed
		if s.sharedQuotaClient == nil {
			if err := tx.Model(&database.Email{}).
				Select("COALESCE(SUM(emails.raw_size_bytes), 0)").
				Joins("JOIN mailboxes ON mailboxes.id = emails.mailbox_id AND mailboxes.deleted_at IS NULL").
				Where("mailboxes.workspace_id = ? AND emails.archived_at IS NULL", workspaceID).
				Scan(&used).Error; err != nil {
				return fmt.Errorf("calculate workspace mail usage: %w", err)
			}
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
	if s.sharedQuotaClient != nil {
		usage, err := s.sharedQuotaClient.GetUsedQuota(ctx, &gen.DyGetUsedQuotaRequest{WorkspaceId: workspaceID})
		if err != nil {
			return 0, 0, fmt.Errorf("get shared workspace storage usage: %w", err)
		}
		return limit, usage.GetUsedBytes(), nil
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
	if totalBytes <= 0 {
		return 0, fmt.Errorf("workspace mail quota is zero")
	}
	return totalBytes, nil
}

func (s *EmailService) workspaceSendLimits(ctx context.Context, workspaceID string) (workspace.SendLimits, error) {
	if s.workspace == nil {
		return workspace.SendLimits{}, ErrWorkspaceUnavailable
	}
	limits, err := s.workspace.SendLimits(ctx, workspaceID)
	if err != nil {
		return workspace.SendLimits{}, fmt.Errorf("get workspace send limits: %w", err)
	}
	return limits, nil
}

func reserveOutboundSend(tx *gorm.DB, mailbox database.Mailbox, limits workspace.SendLimits, now time.Time) error {
	dayStart := now.UTC().Truncate(24 * time.Hour)
	monthStart := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	for _, reservation := range []struct {
		scope       string
		periodStart time.Time
		limit       int64
	}{
		{"workspace:day", dayStart, limits.WorkspaceDaily},
		{"workspace:month", monthStart, limits.WorkspaceMonthly},
		{"mailbox:" + mailbox.ID + ":day", dayStart, limits.MailboxDaily},
		{"mailbox:" + mailbox.ID + ":month", monthStart, limits.MailboxMonthly},
	} {
		if err := reserveSendUsage(tx, mailbox.WorkspaceID, reservation.scope, reservation.periodStart, reservation.limit, now); err != nil {
			return err
		}
	}
	return nil
}

func reserveSendUsage(tx *gorm.DB, workspaceID, scope string, periodStart time.Time, limit int64, now time.Time) error {
	if limit <= 0 {
		return nil
	}
	var result database.MailSendUsage
	query := tx.Raw(`
		INSERT INTO mail_send_usages (id, workspace_id, scope, period_start, sent_count, created_at, updated_at)
		VALUES (?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT (workspace_id, scope, period_start) DO UPDATE
		SET sent_count = mail_send_usages.sent_count + 1, updated_at = EXCLUDED.updated_at
		WHERE mail_send_usages.sent_count < ?
		RETURNING id, sent_count`, database.NewID(), workspaceID, scope, periodStart, now, now, limit).Scan(&result)
	if query.Error != nil {
		return query.Error
	}
	if query.RowsAffected == 0 {
		return fmt.Errorf("%w: %s limit=%d", ErrSendLimitExceeded, scope, limit)
	}
	return nil
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
