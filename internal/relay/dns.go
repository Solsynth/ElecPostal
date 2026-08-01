package relay

import (
	"context"
	"net"
	"strings"
)

// DNSChecker resolves the records a customer must publish for a custom domain.
// *net.Resolver satisfies it; tests inject a fake.
type DNSChecker interface {
	LookupCNAME(context.Context, string) (string, error)
	LookupTXT(context.Context, string) ([]string, error)
	LookupMX(context.Context, string) ([]*net.MX, error)
}

// NewDNSResolver returns a resolver that queries the given DNS server on port
// 53 (for example "1.1.1.1"). An empty host defaults to the system resolver.
func NewDNSResolver(host string) (*net.Resolver, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return net.DefaultResolver, nil
	}
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "53")
	}
	dialer := &net.Dialer{}
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, host)
		},
	}, nil
}

// DNSRecordResult is the outcome of checking one published record.
type DNSRecordResult struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Want   string `json:"want"`
	Got    string `json:"got,omitempty"`
	Detail string `json:"detail,omitempty"`
	OK     bool   `json:"ok"`
}

// DNSValidation reports how a custom domain's publishing DNS state matches the
// records that must be published, queried through the configured resolver.
type DNSValidation struct {
	Resolver string            `json:"resolver"`
	Records  []DNSRecordResult `json:"records"`
	DKIM     bool              `json:"dkim"`
	SPF      bool              `json:"spf"`
	MX       bool              `json:"mx"`
}

// ValidateCustomDomainDNS checks DKIM CNAME records, the MAIL FROM SPF TXT
// record, and the inbound MX record against live DNS. inboundHost is the
// value the domain's MX record must point at; an empty value skips the MX
// check.
func ValidateCustomDomainDNS(ctx context.Context, checker DNSChecker, domain string, records []DNSRecord, inboundHost, resolverName string) DNSValidation {
	validation := DNSValidation{Resolver: resolverName}
	dkimTotal, dkimOK := 0, 0
	for _, record := range records {
		result := DNSRecordResult{Name: record.Name, Type: record.Type, Want: record.Value}
		switch record.Type {
		case "CNAME":
			got, err := checker.LookupCNAME(ctx, record.Name)
			if err != nil {
				result.Detail = err.Error()
			} else {
				result.Got = got
				result.OK = equalHost(got, record.Value)
				if !result.OK {
					result.Detail = "CNAME does not match"
				}
			}
			dkimTotal++
			if result.OK {
				dkimOK++
			}
		case "TXT":
			values, err := checker.LookupTXT(ctx, record.Name)
			if err != nil {
				result.Detail = err.Error()
			} else {
				result.Got = strings.Join(values, " ")
				result.OK = containsSPF(values)
				if !result.OK {
					result.Detail = "no matching SPF TXT record"
				}
			}
			if result.OK {
				validation.SPF = true
			}
		case "MX":
			if inboundHost == "" {
				result.Detail = "inbound MX not configured"
				break
			}
			mxRecords, err := checker.LookupMX(ctx, record.Name)
			if err != nil {
				result.Detail = err.Error()
			} else {
				hosts := make([]string, 0, len(mxRecords))
				for _, mx := range mxRecords {
					hosts = append(hosts, strings.TrimSuffix(mx.Host, "."))
					if equalHost(mx.Host, inboundHost) {
						result.OK = true
					}
				}
				result.Got = strings.Join(hosts, " ")
				if !result.OK {
					result.Detail = "inbound MX record missing"
				}
			}
			if result.OK {
				validation.MX = true
			}
		default:
			result.Detail = "unsupported record type"
		}
		validation.Records = append(validation.Records, result)
	}
	validation.DKIM = dkimTotal > 0 && dkimOK == dkimTotal
	return validation
}

func equalHost(a, b string) bool {
	normalize := func(value string) string {
		return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
	}
	return normalize(a) == normalize(b)
}

func containsSPF(values []string) bool {
	for _, value := range values {
		lower := strings.ToLower(strings.TrimSpace(value))
		if strings.HasPrefix(lower, "v=spf1") && strings.Contains(lower, "amazonses.com") {
			return true
		}
	}
	return false
}

// Custom-domain setup stages.
const (
	CustomDomainStageBasic     = "basic"
	CustomDomainStageFull      = "full"
	CustomDomainStageCompleted = "completed"
)

// StageReport derives the custom-domain setup stage from a validation result.
func StageReport(validation DNSValidation) string {
	switch {
	case validation.MX:
		return CustomDomainStageCompleted
	case validation.DKIM && validation.SPF:
		return CustomDomainStageFull
	default:
		return CustomDomainStageBasic
	}
}
