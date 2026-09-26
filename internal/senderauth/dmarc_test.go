package senderauth

import (
	"context"
	"errors"
	"testing"
)

func TestDMARC(t *testing.T) {
	cases := []struct {
		name     string
		record   string
		txtErr   error
		from     string
		envelope string
		spf      string
		dkim     string
		dkimD    string
		want     string
	}{
		{
			name: "policy reject, unaligned", record: "v=DMARC1; p=reject",
			from: "example.com", envelope: "other.test", spf: "none", dkim: "none", want: "reject",
		},
		{
			name: "policy quarantine, unaligned", record: "v=DMARC1; p=quarantine",
			from: "example.com", envelope: "other.test", spf: "none", dkim: "none", want: "quarantine",
		},
		{
			name: "policy none, unaligned", record: "v=DMARC1; p=none",
			from: "example.com", envelope: "other.test", spf: "none", dkim: "none", want: "none",
		},
		{
			name: "spf aligned allows", record: "v=DMARC1; p=reject",
			from: "example.com", envelope: "example.com", spf: "pass", dkim: "none", want: "allow",
		},
		{
			name: "spf pass but unaligned domain", record: "v=DMARC1; p=reject",
			from: "example.com", envelope: "other.test", spf: "pass", dkim: "none", want: "reject",
		},
		{
			name: "spf subdomain aligned relaxed", record: "v=DMARC1; p=quarantine",
			from: "example.com", envelope: "mail.example.com", spf: "pass", dkim: "none", want: "allow",
		},
		{
			name: "spf subdomain unaligned strict", record: "v=DMARC1; p=quarantine; aspf=strict",
			from: "example.com", envelope: "mail.example.com", spf: "pass", dkim: "none", want: "quarantine",
		},
		{
			name: "dkim aligned allows", record: "v=DMARC1; p=reject; adkim=strict",
			from: "example.com", envelope: "other.test", spf: "none", dkim: "pass", dkimD: "example.com", want: "allow",
		},
		{
			name: "dkim subdomain unaligned strict", record: "v=DMARC1; p=reject; adkim=strict",
			from: "example.com", envelope: "other.test", spf: "none", dkim: "pass", dkimD: "mail.example.com", want: "reject",
		},
		{
			name: "dkim subdomain aligned relaxed", record: "v=DMARC1; p=reject",
			from: "example.com", envelope: "other.test", spf: "none", dkim: "pass", dkimD: "mail.example.com", want: "allow",
		},
		{
			name: "missing p tag", record: "v=DMARC1; aspf=s",
			from: "example.com", envelope: "other.test", spf: "none", dkim: "none", want: "none",
		},
		{
			name: "invalid p tag", record: "v=DMARC1; p=bogus",
			from: "example.com", envelope: "other.test", spf: "none", dkim: "none", want: "none",
		},
		{
			name: "no from domain", record: "v=DMARC1; p=reject",
			from: "", envelope: "other.test", spf: "pass", dkim: "none", want: "none",
		},
		{
			name: "dns failure", txtErr: errors.New("boom"),
			from: "example.com", envelope: "example.com", spf: "pass", dkim: "none", want: "none",
		},
		{
			name: "temperror spf does not align", record: "v=DMARC1; p=reject",
			from: "example.com", envelope: "example.com", spf: "temperror", dkim: "none", want: "reject",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeResolver()
			if tc.txtErr != nil {
				f.txtErr["_dmarc.example.com"] = tc.txtErr
			} else if tc.record != "" {
				f.txt["_dmarc.example.com"] = []string{tc.record}
			}
			v := newTestVerifier(t, f, 0)
			got := v.verifyDMARC(context.Background(), tc.from, tc.envelope, tc.spf, tc.dkim, tc.dkimD)
			if got != tc.want {
				t.Fatalf("verifyDMARC = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDMARCEndToEndDkimAlignment: a valid DKIM signature whose d= matches the
// From domain yields DMARC allow under a reject policy.
func TestDMARCEndToEndDkimAlignment(t *testing.T) {
	fix := signMessage(t, "Body for DMARC alignment.", false, "sha256")
	f := dkimResolver(newFakeResolver(), fix.pubB64)
	f.txt["_dmarc."+dkimDomain] = []string{"v=DMARC1; p=reject; adkim=strict"}
	v := newTestVerifier(t, f, 0)

	res := v.Verify(context.Background(), fix.raw, "bounce@other.test", "192.0.2.1")
	if res.DKIM != "pass" {
		t.Fatalf("DKIM = %q, want pass", res.DKIM)
	}
	if res.DMARC != "allow" {
		t.Fatalf("DMARC = %q, want allow (dkim aligned)", res.DMARC)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("warnings = %v, want none for aligned mail", res.Warnings)
	}
}
