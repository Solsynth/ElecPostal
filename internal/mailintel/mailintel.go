// Package mailintel finds the notification-worthy part of an incoming email so
// notifications can surface a verification code, a security alert, or an
// action request instead of the leading excerpt of the message.
package mailintel

import (
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"src.solsynth.dev/sosys/elecpostal/internal/mailtext"
)

// Kind classifies what an incoming message wants from its recipient.
type Kind string

const (
	// KindNone means nothing in the message stood out.
	KindNone Kind = ""
	// KindCode marks a one-time verification code.
	KindCode Kind = "code"
	// KindSecurity marks an account-security event report.
	KindSecurity Kind = "security"
	// KindAction marks a request the recipient has to act on.
	KindAction Kind = "action"
)

// Highlight is the notification-worthy part of an incoming email.
type Highlight struct {
	Kind Kind
	// Text is the excerpt a notification should show: the isolated code for
	// KindCode, otherwise the matching sentence.
	Text string
	// Code is the normalized code when one was found, including promo codes
	// surfaced through KindAction.
	Code string
}

const (
	// anchorWindow is how many runes may sit between a candidate code and its
	// anchor keyword. Codes are introduced ("Your code is …") or followed
	// ("123456 is your code") on the same line or the next one.
	anchorWindow = 48
	// excerptRuneLimit bounds the notification excerpt so a long sentence
	// cannot overflow a push payload.
	excerptRuneLimit = 120
	// segmentRuneLimit bounds the text matched per segment.
	segmentRuneLimit = 240
)

var (
	// digitCode matches a 4-8 digit code, including the "123 456" and
	// "123-456" groupings senders use to make codes readable.
	digitCode = regexp.MustCompile(`\b\d{2,4}[ -]?\d{2,4}\b`)
	// alnumCode matches a 5-12 character code mixing letters and digits.
	alnumCode = regexp.MustCompile(`\b[A-Za-z0-9]{5,12}\b`)
)

// strongVerificationAnchors introduce a one-time credential unambiguously.
var strongVerificationAnchors = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(verification|verify|authentication|confirmation|security|login|log[- ]?in|sign[- ]?in|access|one[- ]time)\s+(code|passcode|password|pin)\b|\b(otp|passcode)\b|\byour code\b|\bcode (sent|below|is|we sent)\b`),
	regexp.MustCompile(`验证码|校验码|动态码|动态密码|一次性密码|驗證碼`),
}

// weakVerificationAnchors introduce a credential through context alone ("Code:
// 123456"). Modifier-led phrases are filtered out before they are trusted.
var weakVerificationAnchors = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(code|otp|pin|2fa|verify|verification)\b`),
	regexp.MustCompile(`验证码|校验码|动态码|动态密码|一次性密码|驗證碼`),
}

// promoAnchors introduce a discount or referral code.
var promoAnchors = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(promo|coupon|discount|voucher|referral|invitation)\s*code\b|\buse code\b`),
	regexp.MustCompile(`优惠码|折扣码|促销码|抵扣码|邀请码`),
}

// securityKeywords report an account-security event.
var securityKeywords = regexp.MustCompile(`(?i)\b(new sign[- ]?in|new login|sign[- ]?in attempt|login attempt|password (was )?(changed|updated)|two[- ]factor|2fa|security alert|security notice|unauthorized|suspicious|account (locked|suspended|disabled))\b|密码(已|被)?(修改|更改)|安全提醒|异常(登录|活动)|未授权`)

// actionKeywords ask the recipient to do something.
var actionKeywords = regexp.MustCompile(`(?i)\b(confirm|verification|verify|activate|activation|reset|expires?|expiring|expiration|deadline|action required|overdue|past due|payment|invoice|due)\b|确认|激活|重置|过期|到期|截止|支付|账单|发票|待处理|请尽快`)

// codeModifier matches the word introducing an anchor hit that never carries a
// one-time credential ("zip code", "tracking code").
var codeModifier = regexp.MustCompile(`(?i)\b(zip|postal|qr|bar|product|promo|coupon|discount|voucher|referral|invitation|order|tracking|item|country|region|use|using|redeem|apply)\s*$`)

// boilerplateKeywords mark automated footers, which never carry the important
// part of a message.
var boilerplateKeywords = regexp.MustCompile(`(?i)\b(unsubscribe|you are receiving this|this email was sent|privacy policy|update your preferences|manage your notifications)\b|退订|此邮件|隐私政策`)

// Analyze reports the most important thing an incoming message wants from its
// recipient. HTML bodies are reduced to text first. The zero value means the
// message carried nothing worth surfacing, and notifications should fall back
// to the subject.
func Analyze(subject, body, contentType string) Highlight {
	subject = strings.TrimSpace(subject)
	bodyText := mailtext.Text(body, contentType)
	haystack := subject + "\n" + bodyText
	parts := excerpts{subject: splitSegments(subject), body: splitSegments(bodyText)}

	// A one-time code is the most actionable thing an email can carry, and it
	// is the part a leading excerpt would never reach.
	if code, _, ok := nearestCode(haystack, anchorPositions(haystack, strongVerificationAnchors, false)); ok {
		return Highlight{Kind: KindCode, Text: code, Code: code}
	}
	if text, ok := parts.match(securityKeywords); ok {
		return Highlight{Kind: KindSecurity, Text: text}
	}
	// Promo codes outrank a bare "code" mention and are surfaced as their
	// surrounding sentence: "Use code SAVE20" only means something together
	// with the offer.
	if code, at, ok := nearestCode(haystack, anchorPositions(haystack, promoAnchors, false)); ok {
		return Highlight{Kind: KindAction, Text: parts.at(len(subject)+1, at), Code: code}
	}
	if code, _, ok := nearestCode(haystack, anchorPositions(haystack, weakVerificationAnchors, true)); ok {
		return Highlight{Kind: KindCode, Text: code, Code: code}
	}
	if text, ok := parts.match(actionKeywords); ok {
		return Highlight{Kind: KindAction, Text: text}
	}
	return Highlight{}
}

// excerpts holds the subject and body split into segments, so the body can be
// searched before falling back to the subject.
type excerpts struct {
	subject []segment
	body    []segment
}

// match returns the first body segment matching pattern, falling back to the
// subject. Body segments win: they carry the details a leading excerpt and the
// subject both hide.
func (e excerpts) match(pattern *regexp.Regexp) (string, bool) {
	if text, ok := matchingSegment(e.body, pattern); ok {
		return text, true
	}
	return matchingSegment(e.subject, pattern)
}

// at returns the segment containing a haystack byte offset. bodyOffset is where
// the body starts in the haystack.
func (e excerpts) at(bodyOffset, at int) string {
	if at >= bodyOffset {
		if text := segmentAt(e.body, at-bodyOffset); text != "" {
			return text
		}
	}
	return segmentAt(e.subject, at)
}

// matchingSegment returns the first segment matching pattern, skipping
// automated footers.
func matchingSegment(segments []segment, pattern *regexp.Regexp) (string, bool) {
	for _, seg := range segments {
		if boilerplateKeywords.MatchString(seg.text) {
			continue
		}
		if pattern.MatchString(seg.text) {
			return seg.text, true
		}
	}
	return "", false
}

// segmentAt returns the segment containing the byte offset.
func segmentAt(segments []segment, at int) string {
	for _, seg := range segments {
		if at >= seg.start && at < seg.end {
			return seg.text
		}
	}
	return ""
}

type codeCandidate struct {
	code string
	at   int // byte offset of the raw match
}

// nearestCode returns the candidate code whose nearest anchor keyword is
// within anchorWindow runes. The candidate closest to an anchor wins; earlier
// candidates win ties.
func nearestCode(haystack string, positions []int) (string, int, bool) {
	candidates := codeCandidates(haystack)
	if len(candidates) == 0 || len(positions) == 0 {
		return "", 0, false
	}
	code, at, best := "", 0, 0
	for _, candidate := range candidates {
		distance := nearestDistance(utf8.RuneCountInString(haystack[:candidate.at]), positions)
		if distance > anchorWindow {
			continue
		}
		if code == "" || distance < best {
			code, at, best = candidate.code, candidate.at, distance
		}
	}
	if code == "" {
		return "", 0, false
	}
	return code, at, true
}

// codeCandidates collects plausible codes in document order. Codes mix letters
// with digits, or are a run of four to eight digits.
func codeCandidates(haystack string) []codeCandidate {
	var candidates []codeCandidate
	seen := make(map[string]bool)
	for _, pattern := range []*regexp.Regexp{digitCode, alnumCode} {
		for _, loc := range pattern.FindAllStringIndex(haystack, -1) {
			if looksLikeDate(haystack, loc[0], loc[1]) {
				continue
			}
			code, ok := normalizeCode(haystack[loc[0]:loc[1]])
			if !ok || seen[code] {
				continue
			}
			seen[code] = true
			candidates = append(candidates, codeCandidate{code: code, at: loc[0]})
		}
	}
	return candidates
}

// normalizeCode strips the separators senders add for readability and rejects
// matches that cannot be a code.
func normalizeCode(raw string) (string, bool) {
	code := strings.NewReplacer("-", "", " ", "").Replace(raw)
	if len(code) < 4 || len(code) > 12 {
		return "", false
	}
	digits, letters := 0, 0
	for _, r := range code {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case unicode.IsLetter(r):
			letters++
		default:
			return "", false
		}
	}
	switch {
	case digits == 0:
		return "", false
	// A long digit run is an order or account number, not a code.
	case letters == 0 && len(code) > 8:
		return "", false
	// Mixed codes are at least five characters; shorter runs are words with a
	// trailing digit.
	case letters > 0 && len(code) < 5:
		return "", false
	case isCalendarDate(code):
		return "", false
	}
	return code, true
}

// isCalendarDate reports whether a digit run reads as a YYYYMMDD date.
func isCalendarDate(code string) bool {
	if len(code) != 8 || code[:4] < "1900" || code[:4] > "2099" {
		return false
	}
	_, err := time.Parse("20060102", code)
	return err == nil
}

// looksLikeDate reports whether a match continues a date or time literal
// ("2026-09-26", "12:30:45"), which must never surface as a code. A period only
// counts when digits follow it, so the sentence period after a code does not.
func looksLikeDate(haystack string, start, end int) bool {
	if end < len(haystack) && isDateSeparator(haystack[end]) {
		if end+1 < len(haystack) && isDigit(haystack[end+1]) {
			return true
		}
	}
	if start > 0 && isDateSeparator(haystack[start-1]) {
		return true
	}
	return false
}

func isDateSeparator(b byte) bool {
	switch b {
	case '-', '/', '.', ':':
		return true
	default:
		return false
	}
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// anchorPositions returns the rune offsets of every anchor match. With
// skipModifiers, anchors introduced by a modifier word ("zip code",
// "tracking code") are left out because they never carry a one-time code.
func anchorPositions(haystack string, anchors []*regexp.Regexp, skipModifiers bool) []int {
	var positions []int
	for _, pattern := range anchors {
		for _, loc := range pattern.FindAllStringIndex(haystack, -1) {
			if skipModifiers && codeModifier.MatchString(haystack[:loc[0]]) {
				continue
			}
			positions = append(positions, utf8.RuneCountInString(haystack[:loc[0]]))
		}
	}
	return positions
}

func nearestDistance(at int, positions []int) int {
	best := -1
	for _, position := range positions {
		distance := position - at
		if distance < 0 {
			distance = -distance
		}
		if best < 0 || distance < best {
			best = distance
		}
	}
	return best
}

// segment is a sentence or line of the analyzed message. start and end are byte
// offsets into the text it was split from; text is the collapsed, bounded
// rendering without the closing terminator.
type segment struct {
	text       string
	start, end int
}

func splitSegments(text string) []segment {
	segments := make([]segment, 0, 16)
	start := 0
	for i := 0; i < len(text); {
		size, isBreak := segmentBreak(text, i)
		if isBreak {
			appendSegment(&segments, text, start, i)
			start = i + size
		}
		i += size
	}
	appendSegment(&segments, text, start, len(text))
	return segments
}

func appendSegment(segments *[]segment, text string, start, end int) {
	collapsed := collapse(text[start:end], segmentRuneLimit)
	if collapsed == "" {
		return
	}
	*segments = append(*segments, segment{text: truncate(collapsed, excerptRuneLimit), start: start, end: end})
}

// segmentBreak reports the length of the break rune at i and whether it ends a
// segment. Clauses break on commas so a greeting does not drag the sentence
// that follows it into an excerpt. A Latin sentence terminator only breaks when
// whitespace follows, so decimals, abbreviations, and URLs stay intact.
func segmentBreak(text string, i int) (int, bool) {
	r, size := utf8.DecodeRuneInString(text[i:])
	switch r {
	case '\n', '\r', ',', '，', '、', '。', '！', '？', '；':
		return size, true
	case '.', '!', '?', ';':
		rest := text[i+size:]
		if rest == "" {
			return size, true
		}
		next, _ := utf8.DecodeRuneInString(rest)
		return size, unicode.IsSpace(next)
	default:
		return size, false
	}
}

// collapse folds every whitespace run into a single space and bounds the result.
func collapse(value string, limit int) string {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return ""
	}
	return truncate(strings.Join(fields, " "), limit)
}

// truncate cuts a string to limit runes, appending an ellipsis when it does.
func truncate(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return strings.TrimSpace(string(runes[:limit])) + "…"
}
