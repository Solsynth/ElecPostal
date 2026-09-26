package spam

import (
	"crypto/sha256"
	"encoding/hex"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

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
// hash/maphash, which is randomized per process. segmenter may be nil.
func Tokenize(input Input, segmenter Segmenter) []uint64 {
	toks := tokenizeTokens(input, segmenter)
	out := make([]uint64, len(toks))
	for i, t := range toks {
		out[i] = t.hash
	}
	return out
}

func tokenizeTokens(input Input, segmenter Segmenter) []token {
	text := mailtext.Text(input.Body, input.ContentType)

	var raw []string
	var fws []float64
	emit := func(value string, weight float64) {
		raw = append(raw, value)
		fws = append(fws, weight)
	}
	appendStream := func(s string) {
		ws := words(s)
		for i, w := range ws {
			if hasCJK(w) {
				emitCJK(w, segmenter, emit)
				continue
			}
			emit(w, 1.0)
			if i+1 < len(ws) && !hasCJK(ws[i+1]) {
				// Bigrams are \x00-joined so they cannot collide with any
				// unigram; the raw string also tells Bayes it is a bigram.
				emit(w+"\x00"+ws[i+1], 0.5)
			}
		}
	}
	appendStream(input.Subject)
	appendStream(text)
	if d := domainOf(input.FromAddress); d != "" {
		// Prefixed like rspamd's URL/domain metatokens to keep the feature
		// space disjoint from subject/body words.
		emit("#f:"+d, 1.0)
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

// emitCJK turns a CJK-bearing run into features. CJK scripts are written
// without word separators, so a whitespace-style tokenizer would collapse a
// whole sentence into one useless feature. With a segmenter the run becomes
// dictionary words plus their bigrams; without one, character bigrams — the
// signal that survives without a dictionary, and a good approximation of word
// boundaries. Latin parts of a mixed run stay ordinary words.
func emitCJK(run string, segmenter Segmenter, emit func(string, float64)) {
	for _, segment := range scriptSegments(run) {
		if !hasCJK(segment) {
			if utf8.RuneCountInString(segment) >= minWordRunes {
				emit(segment, 1.0)
			}
			continue
		}
		words := segmentWords(segment, segmenter)
		if words == nil {
			// No dictionary available: the character bigram *is* the feature,
			// so each one is emitted on its own rather than chained.
			for _, bigram := range cjkBigrams(segment) {
				emit(bigram, 0.5)
			}
			continue
		}
		for i, word := range words {
			emit(word, 1.0)
			if i+1 < len(words) {
				emit(word+"\x00"+words[i+1], 0.5)
			}
		}
	}
}

// segmentWords returns the word features of one CJK segment, or nil when there
// is no segmenter or it produced no usable word (an out-of-vocabulary run then
// falls back to character bigrams).
func segmentWords(segment string, segmenter Segmenter) []string {
	if segmenter == nil {
		return nil
	}
	var words []string
	for _, word := range segmenter.Cut(segment) {
		word = strings.ToLower(strings.TrimSpace(word))
		if utf8.RuneCountInString(word) < minCJKRunes {
			continue
		}
		words = append(words, word)
	}
	if len(words) == 0 {
		return nil
	}
	return words
}

func fnvHash(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

// minWordRunes is the shortest run kept as a word feature.
const minWordRunes = 3

// words splits s on any non-letter/non-digit rune and keeps lowercased runs of
// at least minWordRunes runes. CJK runs are kept from minCJKRunes because a
// two-character Chinese word is already a complete feature.
func words(s string) []string {
	var out []string
	runes := 0
	var cur strings.Builder
	flush := func() {
		value := cur.String()
		if runes >= minWordRunes || (runes >= minCJKRunes && hasCJK(value)) {
			out = append(out, value)
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

// minCJKRunes is the shortest CJK run worth tokenizing: below it there is not
// even one character bigram to emit.
const minCJKRunes = 2

// isCJK reports whether r belongs to a script written without word separators.
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r) ||
		unicode.Is(unicode.Bopomofo, r)
}

func hasCJK(s string) bool {
	for _, r := range s {
		if isCJK(r) {
			return true
		}
	}
	return false
}

// scriptSegments splits a run into maximal CJK and non-CJK segments, so a
// mixed run such as "免费iPhone" keeps both its Chinese bigrams and the word.
func scriptSegments(s string) []string {
	var out []string
	var cur []rune
	cjk := false
	started := false
	flush := func() {
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = cur[:0]
		}
	}
	for _, r := range s {
		next := isCJK(r)
		if started && next != cjk {
			flush()
		}
		cur = append(cur, r)
		cjk = next
		started = true
	}
	flush()
	return out
}

// cjkBigrams returns every adjacent character pair of a CJK segment.
func cjkBigrams(s string) []string {
	runes := []rune(s)
	if len(runes) < minCJKRunes {
		return nil
	}
	out := make([]string, 0, len(runes)-1)
	for i := 0; i+1 < len(runes); i++ {
		out = append(out, string(runes[i:i+2]))
	}
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
