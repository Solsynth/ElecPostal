package spam

import (
	"encoding/json"
	"math"
	"regexp"
	"strings"
	"unicode"

	"src.solsynth.dev/sosys/elecpostal/internal/mailtext"
)

// Symbol names. EmailService derives the documented Authentication warnings
// from these exact strings.
const (
	SymbolBlockRule             = "BLOCK_RULE"
	SymbolSPFFail               = "R_SPF_FAIL"
	SymbolSPFSoftfail           = "R_SPF_SOFTFAIL"
	SymbolSPFAllow              = "R_SPF_ALLOW"
	SymbolDKIMReject            = "R_DKIM_REJECT"
	SymbolDKIMPermfail          = "R_DKIM_PERMFAIL"
	SymbolDKIMAllow             = "R_DKIM_ALLOW"
	SymbolDMARCPolicyReject     = "DMARC_POLICY_REJECT"
	SymbolDMARCPolicyQuarantine = "DMARC_POLICY_QUARANTINE"
	SymbolDMARCPolicyAllow      = "DMARC_POLICY_ALLOW"
	SymbolEnvelopeFromNeqFrom   = "ENVELOPE_FROM_NEQ_FROM"
	SymbolSpoofDisplayName      = "SPOOF_DISPLAY_NAME"
	SymbolMissingSubject        = "MISSING_SUBJECT"
	SymbolMoneyPhrases          = "MONEY_PHRASES"
	SymbolExcessiveUppercase    = "EXCESSIVE_UPPERCASE"
	SymbolExcessiveExclamation  = "EXCESSIVE_EXCLAMATION"
	SymbolManyURLs              = "MANY_URLS"
	SymbolZeroWidthObfuscation  = "ZERO_WIDTH_OBFUSCATION"
	SymbolBayesSpam             = "BAYES_SPAM"
	SymbolBayesHam              = "BAYES_HAM"
)

// Fixed rule weights, mirroring rspamd conf/scores.d defaults (adapted).
// Only the overall threshold is configurable in v1.
const (
	weightBlockRule             = 5.0
	weightSPFFail               = 1.5
	weightSPFSoftfail           = 0.5
	weightSPFAllow              = -0.2
	weightDKIMReject            = 1.5
	weightDKIMPermfail          = 1.0
	weightDKIMAllow             = -0.1
	weightDMARCPolicyReject     = 2.0
	weightDMARCPolicyQuarantine = 1.5
	weightDMARCPolicyAllow      = -0.5
	weightEnvelopeFromNeqFrom   = 1.0
	weightSpoofDisplayName      = 2.0
	weightMissingSubject        = 1.0
	weightMoneyPhrases          = 1.0
	weightExcessiveUppercase    = 0.5
	weightExcessiveExclamation  = 0.5
	weightManyURLsPerFive       = 0.5
	weightManyURLsCap           = 2.0
	weightZeroWidthObfuscation  = 1.0

	bayesSpamWeight = 5.1
	bayesHamWeight  = 3.0
)

// minScoreFloor keeps negative totals stable for display. There is no
// behavioral need for the clamp beyond that.
const minScoreFloor = -10

// authFields is the SMTP-produced Authentication JSON shape (senderauth.Result
// marshaled). Only the fields rules read are decoded.
type authFields struct {
	SPF   string `json:"spf"`
	DKIM  string `json:"dkim"`
	DMARC string `json:"dmarc"`
}

// evaluateRules runs the deterministic symbol table in fixed order and sums
// the weights. text is the mailtext.Text view of the body, bounded to 16K
// runes.
func (s *Service) evaluateRules(input Input) Result {
	var auth authFields
	if len(input.Authentication) > 0 {
		_ = json.Unmarshal(input.Authentication, &auth)
	}
	text := mailtext.Text(input.Body, input.ContentType)

	var symbols []Symbol
	score := 0.0
	add := func(name string, weight float64) {
		symbols = append(symbols, Symbol{Name: name, Weight: weight})
		score += weight
	}

	if input.IsBlocked {
		add(SymbolBlockRule, weightBlockRule)
	}
	switch auth.SPF {
	case "fail":
		add(SymbolSPFFail, weightSPFFail)
	case "softfail":
		add(SymbolSPFSoftfail, weightSPFSoftfail)
	case "pass":
		add(SymbolSPFAllow, weightSPFAllow)
	}
	switch auth.DKIM {
	case "fail":
		add(SymbolDKIMReject, weightDKIMReject)
	case "permerror":
		add(SymbolDKIMPermfail, weightDKIMPermfail)
	case "pass":
		add(SymbolDKIMAllow, weightDKIMAllow)
	}
	switch auth.DMARC {
	case "reject":
		add(SymbolDMARCPolicyReject, weightDMARCPolicyReject)
	case "quarantine":
		add(SymbolDMARCPolicyQuarantine, weightDMARCPolicyQuarantine)
	case "allow":
		add(SymbolDMARCPolicyAllow, weightDMARCPolicyAllow)
	}
	if envDomain, fromDomain := domainOf(input.EnvelopeFrom), domainOf(input.FromAddress); envDomain != "" && fromDomain != "" && !strings.EqualFold(envDomain, fromDomain) {
		add(SymbolEnvelopeFromNeqFrom, weightEnvelopeFromNeqFrom)
	}
	if auth.DKIM != "pass" && spoofDisplayName(input.FromName, input.FromAddress) {
		add(SymbolSpoofDisplayName, weightSpoofDisplayName)
	}
	if strings.TrimSpace(input.Subject) == "" {
		add(SymbolMissingSubject, weightMissingSubject)
	}
	if moneyPhrases(input.Subject, text) {
		add(SymbolMoneyPhrases, weightMoneyPhrases)
	}
	if excessiveUppercase(text) {
		add(SymbolExcessiveUppercase, weightExcessiveUppercase)
	}
	if strings.Count(text, "!") > 4 {
		add(SymbolExcessiveExclamation, weightExcessiveExclamation)
	}
	if n := urlCount(text); n > 5 {
		w := weightManyURLsPerFive * math.Ceil(float64(n-5)/5)
		if w > weightManyURLsCap {
			w = weightManyURLsCap
		}
		add(SymbolManyURLs, w)
	}
	if zeroWidth(text) {
		add(SymbolZeroWidthObfuscation, weightZeroWidthObfuscation)
	}

	return Result{Score: score, Symbols: symbols}
}

var spoofNames = []string{
	"admin", "support", "security", "account", "billing",
	"paypal", "apple", "google", "microsoft", "amazon", "bank",
}

// spoofDisplayName fires when the display name advertises a trusted persona
// but is not the actual local part of the From address. A DKIM pass (checked
// by the caller) suppresses it because the sender is authenticated.
func spoofDisplayName(fromName, fromAddress string) bool {
	name := strings.ToLower(strings.TrimSpace(fromName))
	if name == "" {
		return false
	}
	contains := false
	for _, s := range spoofNames {
		if strings.Contains(name, s) {
			contains = true
			break
		}
	}
	if !contains {
		return false
	}
	local := localPart(fromAddress)
	return local != "" && name != local
}

var moneyPhraseList = []string{
	"viagra",
	"bitcoin giveaway",
	"urgent wire transfer",
	"click here to claim",
}

func moneyPhrases(subject, text string) bool {
	hay := strings.ToLower(subject + " " + text)
	for _, p := range moneyPhraseList {
		if strings.Contains(hay, p) {
			return true
		}
	}
	return false
}

func excessiveUppercase(text string) bool {
	letters, upper := 0, 0
	for _, r := range text {
		if unicode.IsLetter(r) {
			letters++
			if unicode.IsUpper(r) {
				upper++
			}
		}
	}
	return letters > 100 && float64(upper)/float64(letters) > 0.5
}

var urlRe = regexp.MustCompile(`https?://[^\s]+`)

func urlCount(text string) int {
	return len(urlRe.FindAllString(text, -1))
}

func zeroWidth(text string) bool {
	return strings.ContainsAny(text, "\u200b\u200c\u200d\ufeff")
}

// domainOf returns the lowercased domain part of an address ("<"…">" stripped
// for display-name forms), or "" when there is no "@".
func domainOf(addr string) string {
	addr = strings.TrimSpace(addr)
	if i := strings.LastIndex(addr, "<"); i >= 0 {
		addr = addr[i+1:]
	}
	addr = strings.TrimSuffix(addr, ">")
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		return strings.ToLower(strings.TrimSpace(addr[i+1:]))
	}
	return ""
}

// localPart returns the lowercased local part of an address, "" when absent.
func localPart(addr string) string {
	addr = strings.TrimSpace(addr)
	if i := strings.LastIndex(addr, "<"); i >= 0 {
		addr = addr[i+1:]
	}
	addr = strings.TrimSuffix(addr, ">")
	if i := strings.Index(addr, "@"); i >= 0 {
		return strings.ToLower(strings.TrimSpace(addr[:i]))
	}
	return strings.ToLower(addr)
}
