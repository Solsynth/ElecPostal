package relay

import (
	"context"
	"errors"
)

// DNSRecord is a record a customer must publish before SES can verify a domain.
type DNSRecord struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

// IdentityStatus is the provider-neutral state of a sending identity.
type IdentityStatus struct {
	Identity                 string      `json:"identity"`
	IdentityType             string      `json:"identity_type"`
	VerificationStatus       string      `json:"verification_status"`
	VerifiedForSendingStatus bool        `json:"verified_for_sending_status"`
	DKIMStatus               string      `json:"dkim_status,omitempty"`
	MailFromDomain           string      `json:"mail_from_domain,omitempty"`
	MailFromStatus           string      `json:"mail_from_status,omitempty"`
	DNSRecords               []DNSRecord `json:"dns_records,omitempty"`
}

// IdentityManager provisions and refreshes provider sending identities.
// It is intentionally separate from Adapter so a non-SES relay does not gain
// an identity-management API by accident.
type IdentityManager interface {
	CreateIdentity(context.Context, string) (IdentityStatus, error)
	GetIdentity(context.Context, string) (IdentityStatus, error)
	DeleteIdentity(context.Context, string) error
	EnsureMailFrom(context.Context, string) error
}

var ErrIdentityManagementUnavailable = errors.New("mail relay identity management is not configured")
