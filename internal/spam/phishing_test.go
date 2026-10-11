package spam

import (
	"testing"

	"gorm.io/datatypes"
)

func TestPhishingReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth string
		want string
	}{
		{"dmarc reject without alignment", `{"spf":"softfail","dkim":"none","dmarc":"reject"}`, PhishingRejectsUnauthenticatedMail},
		{"dmarc reject with a non-aligned spf pass", `{"spf":"pass","dkim":"none","dmarc":"reject"}`, PhishingRejectsUnauthenticatedMail},
		{"dmarc reject with the verifier's score and symbols present", `{"spf":"softfail","dkim":"none","dmarc":"reject","score":2.5,"symbols":[{"name":"DMARC_POLICY_REJECT","weight":2}],"warnings":["Domain policy rejects unauthenticated mail"]}`, PhishingRejectsUnauthenticatedMail},
		{"case and whitespace tolerated", `{"dmarc":" Reject "}`, PhishingRejectsUnauthenticatedMail},
		{"aligned pass", `{"spf":"pass","dkim":"pass","dmarc":"allow"}`, ""},
		{"quarantine stays a scored symbol", `{"spf":"softfail","dkim":"none","dmarc":"quarantine"}`, ""},
		{"domain without a dmarc record", `{"spf":"softfail","dkim":"none","dmarc":"none"}`, ""},
		{"temporary dns failure", `{"spf":"temperror","dkim":"temperror","dmarc":"temperror"}`, ""},
		{"missing document", ``, ""},
		{"unreadable document", `[]`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PhishingReason(datatypes.JSON(tc.auth)); got != tc.want {
				t.Fatalf("PhishingReason(%s) = %q, want %q", tc.auth, got, tc.want)
			}
		})
	}
}

func TestPhishingReasonIsHeaderSafe(t *testing.T) {
	for _, r := range PhishingRejectsUnauthenticatedMail {
		if r < 0x20 || r > 0x7e {
			t.Fatalf("reason contains non-ASCII byte %q: it is also written as a header value", r)
		}
	}
	if len(PhishingRejectsUnauthenticatedMail) > 200 {
		t.Fatalf("reason is %d bytes, too long for a header value", len(PhishingRejectsUnauthenticatedMail))
	}
}
