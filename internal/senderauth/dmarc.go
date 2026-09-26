package senderauth

import (
	"context"
	"strings"
)

// verifyDMARC applies the From domain's DMARC policy. It returns "allow" when
// SPF or DKIM is aligned, otherwise the published policy
// ("reject"/"quarantine"/"none"). A missing record, a missing p= tag, or a DNS
// failure yields "none" (no warning, no score contribution).
func (v *verifier) verifyDMARC(ctx context.Context, fromDomain, envelopeDomain, spf, dkim, dkimD string) string {
	if fromDomain == "" {
		return "none"
	}
	records, err := v.lookupTXT(ctx, "_dmarc."+fromDomain)
	if err != nil {
		return "none"
	}
	record := dmarcRecord(records)
	if record == "" {
		return "none"
	}
	policy, aspf, adkim := parseDMARC(record)
	if policy == "" {
		return "none"
	}

	aligned := false
	if spf == "pass" && alignedDomain(envelopeDomain, fromDomain, aspf) {
		aligned = true
	}
	if !aligned && dkim == "pass" && alignedDomain(dkimD, fromDomain, adkim) {
		aligned = true
	}
	if aligned {
		return "allow"
	}
	return policy
}

// dmarcRecord returns the first v=DMARC1 record, or "".
func dmarcRecord(records []string) string {
	for _, r := range records {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(r)), "v=dmarc1") {
			return strings.TrimSpace(r)
		}
	}
	return ""
}

// parseDMARC returns the validated policy (none|quarantine|reject) and the
// SPF/DKIM alignment modes (default relaxed). The sp= subdomain policy is
// accepted but not applied in v1.
func parseDMARC(record string) (policy, aspf, adkim string) {
	for _, part := range strings.Split(record, ";") {
		part = strings.TrimSpace(part)
		eq := strings.Index(part, "=")
		if eq < 0 {
			continue
		}
		tag := strings.ToLower(strings.TrimSpace(part[:eq]))
		val := strings.ToLower(strings.TrimSpace(part[eq+1:]))
		switch tag {
		case "p":
			switch val {
			case "none", "quarantine", "reject":
				policy = val
			}
		case "aspf":
			aspf = val
		case "adkim":
			adkim = val
		}
	}
	return policy, aspf, adkim
}

// alignedDomain reports whether d matches from exactly, or — unless the mode
// is strict — as a subdomain.
func alignedDomain(d, from, mode string) bool {
	d = strings.ToLower(strings.TrimSpace(d))
	from = strings.ToLower(strings.TrimSpace(from))
	if d == "" || from == "" {
		return false
	}
	if d == from {
		return true
	}
	if mode == "strict" {
		return false
	}
	return strings.HasSuffix(d, "."+from)
}
