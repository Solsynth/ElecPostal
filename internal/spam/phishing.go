package spam

import (
	"encoding/json"
	"strings"

	"gorm.io/datatypes"
)

// PhishingRejectsUnauthenticatedMail is the verdict recorded for mail whose
// From domain publishes a DMARC reject policy while the message aligned with
// neither SPF nor DKIM. The domain owner has declared that mail like this is
// forged, so the verdict is terminal instead of a score contribution. The
// string is ASCII only: it is also written into the stored message as a header.
const PhishingRejectsUnauthenticatedMail = "Sender domain rejects unauthenticated mail (DMARC p=reject)"

// PhishingReason returns the reason a message must be treated as a forged
// sender, or "" when it is not one. The DMARC field carries the policy outcome
// the verifier computed, and the verifier returns "reject" only for a message
// that failed to align with a domain publishing p=reject. A non-aligned SPF or
// DKIM pass therefore still lands here, which is the point: that is exactly the
// shape of an envelope-authorized relay forging the header From.
func PhishingReason(authentication datatypes.JSON) string {
	if len(authentication) == 0 {
		return ""
	}
	var auth authFields
	if err := json.Unmarshal(authentication, &auth); err != nil {
		return ""
	}
	if !strings.EqualFold(strings.TrimSpace(auth.DMARC), "reject") {
		return ""
	}
	return PhishingRejectsUnauthenticatedMail
}
