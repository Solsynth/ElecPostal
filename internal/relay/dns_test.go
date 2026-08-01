package relay

import (
	"context"
	"net"
	"testing"
)

type fakeDNSChecker struct {
	cname    map[string]string
	cnameErr map[string]error
	txt      map[string][]string
	txtErr   map[string]error
	mx       map[string][]*net.MX
	mxErr    map[string]error
}

func (f *fakeDNSChecker) LookupCNAME(_ context.Context, name string) (string, error) {
	if err := f.cnameErr[name]; err != nil {
		return "", err
	}
	return f.cname[name], nil
}

func (f *fakeDNSChecker) LookupTXT(_ context.Context, name string) ([]string, error) {
	if err := f.txtErr[name]; err != nil {
		return nil, err
	}
	return f.txt[name], nil
}

func (f *fakeDNSChecker) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	if err := f.mxErr[name]; err != nil {
		return nil, err
	}
	return f.mx[name], nil
}

func sampleDNSRecords() []DNSRecord {
	return []DNSRecord{
		{Name: "one._domainkey.example.com", Type: "CNAME", Value: "one.dkim.amazonses.com"},
		{Name: "two._domainkey.example.com", Type: "CNAME", Value: "two.dkim.amazonses.com"},
		{Name: "bounce.example.com", Type: "TXT", Value: "v=spf1 include:amazonses.com ~all"},
		{Name: "example.com", Type: "MX", Value: "10 mail.example.com"},
	}
}

func TestValidateCustomDomainDNSAllPublished(t *testing.T) {
	checker := &fakeDNSChecker{
		cname: map[string]string{
			"one._domainkey.example.com": "one.dkim.amazonses.com.",
			"two._domainkey.example.com": "two.dkim.amazonses.com",
		},
		txt: map[string][]string{
			"bounce.example.com": {"v=spf1 include:amazonses.com ~all"},
		},
		mx: map[string][]*net.MX{
			"example.com": {{Host: "mail.example.com.", Pref: 10}},
		},
	}

	validation := ValidateCustomDomainDNS(context.Background(), checker, "example.com", sampleDNSRecords(), "mail.example.com", "1.1.1.1")
	if !validation.DKIM || !validation.SPF || !validation.MX {
		t.Fatalf("validation = %#v", validation)
	}
	if stage := StageReport(validation); stage != CustomDomainStageCompleted {
		t.Fatalf("stage = %q, want completed", stage)
	}
	for _, result := range validation.Records {
		if !result.OK {
			t.Fatalf("record not ok: %#v", result)
		}
	}
}

func TestValidateCustomDomainDNSMissingMXFallsBackToFull(t *testing.T) {
	checker := &fakeDNSChecker{
		cname: map[string]string{
			"one._domainkey.example.com": "one.dkim.amazonses.com",
			"two._domainkey.example.com": "two.dkim.amazonses.com",
		},
		txt: map[string][]string{
			"bounce.example.com": {"v=spf1 include:amazonses.com ~all"},
		},
		mx: map[string][]*net.MX{
			"example.com": {{Host: "aspmx.l.google.com.", Pref: 10}},
		},
	}

	validation := ValidateCustomDomainDNS(context.Background(), checker, "example.com", sampleDNSRecords(), "mail.example.com", "1.1.1.1")
	if !validation.DKIM || !validation.SPF || validation.MX {
		t.Fatalf("validation = %#v", validation)
	}
	if stage := StageReport(validation); stage != CustomDomainStageFull {
		t.Fatalf("stage = %q, want full", stage)
	}
}

func TestValidateCustomDomainDNSNothingPublishedIsBasic(t *testing.T) {
	checker := &fakeDNSChecker{mxErr: map[string]error{"example.com": &net.DNSError{IsNotFound: true}}}

	validation := ValidateCustomDomainDNS(context.Background(), checker, "example.com", sampleDNSRecords(), "mail.example.com", "1.1.1.1")
	if validation.DKIM || validation.SPF || validation.MX {
		t.Fatalf("validation = %#v", validation)
	}
	if stage := StageReport(validation); stage != CustomDomainStageBasic {
		t.Fatalf("stage = %q, want basic", stage)
	}
}
