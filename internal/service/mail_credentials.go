package service

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"src.solsynth.dev/sosys/elecpostal/internal/database"
)

const mailAppPasswordBytes = 32

var validMailProtocols = map[string]struct{}{
	"smtp": {},
	"imap": {},
	"pop3": {},
}

// CreateMailProtocolCredentialInput defines the label and protocol scopes for
// a new account-owned mail app password.
type CreateMailProtocolCredentialInput struct {
	Label     string   `json:"label" binding:"required"`
	Protocols []string `json:"protocols" binding:"required,min=1"`
}

// CreatedMailProtocolCredential is returned only at creation time. Secret must
// be shown once; the database stores a bcrypt hash instead.
type CreatedMailProtocolCredential struct {
	Credential database.MailProtocolCredential `json:"credential"`
	Secret     string                          `json:"secret"`
}

// ProtocolPrincipal is the account authenticated for a mail protocol session.
type ProtocolPrincipal struct {
	AccountID    uuid.UUID
	CredentialID string
}

func (s *EmailService) CreateMailProtocolCredential(ctx context.Context, accountID uuid.UUID, input CreateMailProtocolCredentialInput) (*CreatedMailProtocolCredential, error) {
	label := strings.TrimSpace(input.Label)
	if label == "" {
		return nil, fmt.Errorf("label is required")
	}
	protocols, err := normalizeMailProtocols(input.Protocols)
	if err != nil {
		return nil, err
	}
	encodedProtocols, err := json.Marshal(protocols)
	if err != nil {
		return nil, fmt.Errorf("encode credential protocols: %w", err)
	}

	secretBytes := make([]byte, mailAppPasswordBytes)
	if _, err := rand.Read(secretBytes); err != nil {
		return nil, fmt.Errorf("generate app password: %w", err)
	}
	secret := base64.RawURLEncoding.EncodeToString(secretBytes)
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash app password: %w", err)
	}
	credential := database.MailProtocolCredential{
		AccountID: accountID,
		Label:     label,
		Hash:      string(hash),
		Protocols: encodedProtocols,
	}
	if err := s.db.WithContext(ctx).Create(&credential).Error; err != nil {
		return nil, err
	}
	return &CreatedMailProtocolCredential{Credential: credential, Secret: secret}, nil
}

func (s *EmailService) ListMailProtocolCredentials(ctx context.Context, accountID uuid.UUID) ([]database.MailProtocolCredential, error) {
	var credentials []database.MailProtocolCredential
	if err := s.db.WithContext(ctx).Where("account_id = ?", accountID).Order("created_at desc").Find(&credentials).Error; err != nil {
		return nil, err
	}
	return credentials, nil
}

func (s *EmailService) DeleteMailProtocolCredential(ctx context.Context, accountID uuid.UUID, id string) error {
	result := s.db.WithContext(ctx).Where("id = ? AND account_id = ?", id, accountID).Delete(&database.MailProtocolCredential{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// AuthenticateMailProtocol verifies a scoped app password. This is intended
// solely for SMTP/IMAP/POP3 listeners; it does not authenticate HTTP requests.
func (s *EmailService) AuthenticateMailProtocol(ctx context.Context, accountID uuid.UUID, secret, protocol string) (*ProtocolPrincipal, error) {
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	if _, ok := validMailProtocols[protocol]; !ok {
		return nil, fmt.Errorf("unsupported mail protocol %q", protocol)
	}
	if strings.TrimSpace(secret) == "" {
		return nil, ErrForbidden
	}
	var credentials []database.MailProtocolCredential
	if err := s.db.WithContext(ctx).Where("account_id = ?", accountID).Find(&credentials).Error; err != nil {
		return nil, err
	}
	for _, credential := range credentials {
		protocols := []string{}
		if err := json.Unmarshal(credential.Protocols, &protocols); err != nil {
			continue
		}
		if !containsProtocol(protocols, protocol) {
			continue
		}
		if bcrypt.CompareHashAndPassword([]byte(credential.Hash), []byte(secret)) == nil {
			return &ProtocolPrincipal{AccountID: accountID, CredentialID: credential.ID}, nil
		}
	}
	return nil, ErrForbidden
}

// AuthenticateMailProtocolAddress resolves a mailbox login address before
// checking its owner's protocol-scoped app passwords.
func (s *EmailService) AuthenticateMailProtocolAddress(ctx context.Context, address, secret, protocol string) (*ProtocolPrincipal, error) {
	var mailbox database.Mailbox
	if err := s.db.WithContext(ctx).Where("address = ?", strings.ToLower(strings.TrimSpace(address))).First(&mailbox).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrForbidden
		}
		return nil, err
	}
	return s.AuthenticateMailProtocol(ctx, mailbox.AccountID, secret, protocol)
}

func normalizeMailProtocols(input []string) ([]string, error) {
	seen := make(map[string]struct{}, len(input))
	protocols := make([]string, 0, len(input))
	for _, protocol := range input {
		protocol = strings.ToLower(strings.TrimSpace(protocol))
		if _, ok := validMailProtocols[protocol]; !ok {
			return nil, fmt.Errorf("unsupported mail protocol %q", protocol)
		}
		if _, exists := seen[protocol]; !exists {
			seen[protocol] = struct{}{}
			protocols = append(protocols, protocol)
		}
	}
	if len(protocols) == 0 {
		return nil, fmt.Errorf("at least one mail protocol is required")
	}
	return protocols, nil
}

func containsProtocol(protocols []string, target string) bool {
	for _, protocol := range protocols {
		if protocol == target {
			return true
		}
	}
	return false
}
