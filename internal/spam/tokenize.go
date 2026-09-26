package spam

import (
	"crypto/sha256"
	"encoding/hex"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"src.solsynth.dev/sosys/elecpostal/internal/mailtext"
)

// maxTokens caps the emitted token stream (before hashing and dedupe) so an
// adversarial message cannot bloat the store call. First 512 in deterministic
// order: subject words, body words, from-domain.
const maxTokens = 512

// token is one hashed feature plus the feature weight Bayes applies to it
// (fw 1.0 unigram, 0.5 bigram, per the rspamd PROB_COMBINE port).
type token struct {
	hash uint64
	fw   float64
}

// Tokenize returns the deterministic, deduplicated (insertion order) fnv-1a
// 64 hashes of a message's tokens. Stable across restarts — deliberately NOT
// hash/maphash, which is randomized per process.
func Tokenize(input Input) []uint64 {
	toks := tokenizeTokens(input)
	out := make([]uint64, len(toks))
	for i, t := range toks {
		out[i] = t.hash
	}
	return out
}

func tokenizeTokens(input Input) []token {
	text := mailtext.Text(input.Body, input.ContentType)

	var raw []string
	var fws []float64
	appendStream := func(s string) {
		ws := words(s)
		for i, w := range ws {
			raw = append(raw, w)
			fws = append(fws, 1.0)
			if i+1 < len(ws) {
				// Bigrams are \x00-joined so they cannot collide with any
				// unigram; the raw string also tells Bayes it is a bigram.
				raw = append(raw, w+"\x00"+ws[i+1])
				fws = append(fws, 0.5)
			}
		}
	}
	appendStream(input.Subject)
	appendStream(text)
	if d := domainOf(input.FromAddress); d != "" {
		// Prefixed like rspamd's URL/domain metatokens to keep the feature
		// space disjoint from subject/body words.
		raw = append(raw, "#f:"+d)
		fws = append(fws, 1.0)
	}
	if len(raw) > maxTokens {
		raw = raw[:maxTokens]
		fws = fws[:maxTokens]
	}

	seen := make(map[uint64]bool, len(raw))
	out := make([]token, 0, len(raw))
	for i, s := range raw {
		h := fnvHash(s)
		if seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, token{hash: h, fw: fws[i]})
	}
	return out
}

func fnvHash(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

// words splits s on any non-letter/non-digit rune and keeps tokens of at
// least 3 runes, lowercased.
func words(s string) []string {
	var out []string
	runes := 0
	var cur strings.Builder
	flush := func() {
		if runes >= 3 {
			out = append(out, cur.String())
		}
		cur.Reset()
		runes = 0
	}
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(r)
			runes++
		} else {
			flush()
		}
	}
	flush()
	return out
}

// Digest is the learning idempotency key: hex of sha256 over the sorted,
// decimal comma-joined token hashes.
func Digest(hashes []uint64) string {
	sorted := make([]uint64, len(hashes))
	copy(sorted, hashes)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var b strings.Builder
	for i, h := range sorted {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatUint(h, 10))
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
