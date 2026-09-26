package spam

import (
	"reflect"
	"strings"
	"testing"
)

// Reference fnv-1a 64 values pin the tokenizer's hashing scheme so a change
// that would silently invalidate a trained Bayes model fails the build.
var refHashes = map[string]uint64{
	"hello":          0xa430d84680aabd0b,
	"#f:example.com": 0x9954297336c35047,
	"hello\x00world": 0x929792d32ae49607,
}

// refCJKHashes pins the CJK feature strings the same way.
var refCJKHashes = map[string]uint64{
	"免费":       0x8f764547b3f9cba9,
	"费领":       0xce2f90a100345697,
	"领取":       0xa1a6d532b15def5e,
	"中奖":       0x4ccaf299d768c68c,
	"免费领取":     0x3265510a1e942692,
	"银行":       0x0ee231a7fa8b8dfe,
	"转账":       0x13f9ee43c085f0c4,
	"银行\x00转账": 0x833dcbf27d256ad7,
	"银行转账":     0x9b1b4f9df1214e0b,
	"会议纪要":     0x81cd54192e7dc165,
	"iphone":   0x9b92d57c6f482920,
}

// fakeSegmenter is a dictionary stub so tokenizer-shape tests stay independent
// of the real dictionary.
type fakeSegmenter struct {
	words map[string][]string
}

func (f fakeSegmenter) Cut(text string) []string {
	if words, ok := f.words[text]; ok {
		return words
	}
	return []string{text}
}

func newTestSegmenter() Segmenter {
	return fakeSegmenter{words: map[string][]string{
		"银行转账": {"银行", "转账"},
		"会议纪要": {"会议纪要"},
	}}
}

func TestTokenizeDeterministicStableDeduped(t *testing.T) {
	in := Input{Subject: "Hello World", Body: "Hello world hello", FromAddress: "a@example.com"}
	a := Tokenize(in, nil)
	b := Tokenize(in, nil)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("tokenize not deterministic")
	}
	seen := map[uint64]bool{}
	for _, h := range a {
		if seen[h] {
			t.Fatalf("duplicate hash %#x", h)
		}
		seen[h] = true
	}
	for tok, want := range refHashes {
		if !seen[want] {
			t.Errorf("token %q hash %#x missing from set", tok, want)
		}
	}
}

func TestTokenizeStructure(t *testing.T) {
	// Subject "Hello World" -> hello, world, hello\x00world (3)
	// Body "one two three" -> one, two, three, one\x00two, two\x00three (5)
	// Domain #f:example.com (1)
	toks := tokenizeTokens(Input{Subject: "Hello World", Body: "one two three", FromAddress: "a@example.com"}, nil)
	if len(toks) != 9 {
		t.Fatalf("token count = %d, want 9: %+v", len(toks), toks)
	}
	byHash := map[uint64]token{}
	for _, tk := range toks {
		byHash[tk.hash] = tk
	}
	// Bigram carries fw 0.5, unigrams and domain carry 1.0.
	if tk := byHash[refHashes["hello\x00world"]]; tk.fw != 0.5 {
		t.Fatalf("bigram fw = %v, want 0.5", tk.fw)
	}
	if tk := byHash[refHashes["#f:example.com"]]; tk.fw != 1.0 {
		t.Fatalf("domain fw = %v, want 1.0", tk.fw)
	}
	if tk := byHash[refHashes["hello"]]; tk.fw != 1.0 {
		t.Fatalf("unigram fw = %v, want 1.0", tk.fw)
	}
}

func TestTokenizeFiltersShortWords(t *testing.T) {
	toks := tokenizeTokens(Input{Subject: "hi a b", Body: "be go to"}, nil)
	if len(toks) != 0 {
		t.Fatalf("short words should be dropped, got %d tokens", len(toks))
	}
}

func TestTokenizeCapsAtMaxTokens(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 500; i++ {
		sb.WriteString("abcdef ")
	}
	toks := tokenizeTokens(Input{Body: sb.String()}, nil)
	if len(toks) > maxTokens {
		t.Fatalf("token count %d exceeds cap %d", len(toks), maxTokens)
	}
}

// TestTokenizeCJKBigrams: without a segmenter a Chinese run becomes character
// bigrams (the whole run is never one feature) and the two-character word
// "中奖" survives on its own.
func TestTokenizeCJKBigrams(t *testing.T) {
	toks := tokenizeTokens(Input{Body: "免费领取 中奖"}, nil)
	byHash := map[uint64]token{}
	for _, tk := range toks {
		byHash[tk.hash] = tk
	}
	for _, want := range []struct {
		text string
		hash uint64
	}{
		{"免费", refCJKHashes["免费"]},
		{"费领", refCJKHashes["费领"]},
		{"领取", refCJKHashes["领取"]},
		{"中奖", refCJKHashes["中奖"]},
	} {
		tk, ok := byHash[want.hash]
		if !ok {
			t.Fatalf("bigram %s missing from %+v", want.text, toks)
		}
		if tk.fw != 0.5 {
			t.Fatalf("bigram %s fw = %v, want 0.5", want.text, tk.fw)
		}
	}
	if _, ok := byHash[refCJKHashes["免费领取"]]; ok {
		t.Fatal("the raw CJK run must not become a single feature")
	}
}

// TestTokenizeCJKMixedRun: the Latin half of a mixed run stays a word.
func TestTokenizeCJKMixedRun(t *testing.T) {
	toks := tokenizeTokens(Input{Body: "免费iphone"}, nil)
	byHash := map[uint64]token{}
	for _, tk := range toks {
		byHash[tk.hash] = tk
	}
	if _, ok := byHash[refCJKHashes["iphone"]]; !ok {
		t.Fatalf("latin segment dropped from mixed run: %+v", toks)
	}
	if _, ok := byHash[refCJKHashes["免费"]]; !ok {
		t.Fatalf("cjk segment dropped from mixed run: %+v", toks)
	}
}

// TestTokenizeCJKSegmenter: a segmenter replaces character bigrams with
// dictionary words plus word bigrams.
func TestTokenizeCJKSegmenter(t *testing.T) {
	segmenter := newTestSegmenter()
	toks := tokenizeTokens(Input{Body: "银行转账 会议纪要"}, segmenter)
	byHash := map[uint64]token{}
	for _, tk := range toks {
		byHash[tk.hash] = tk
	}
	for _, word := range []string{"银行", "转账", "会议纪要"} {
		tk, ok := byHash[refCJKHashes[word]]
		if !ok {
			t.Fatalf("segmenter word %s missing from %+v", word, toks)
		}
		if tk.fw != 1.0 {
			t.Fatalf("segmenter word %s fw = %v, want 1.0", word, tk.fw)
		}
	}
	if _, ok := byHash[refCJKHashes["银行\x00转账"]]; !ok {
		t.Fatalf("word bigram missing from %+v", toks)
	}
}

// TestTokenizeCJKFallback: a segmenter that finds no usable word (all shorter
// than two runes) still leaves character bigrams for the run.
func TestTokenizeCJKFallback(t *testing.T) {
	segmenter := fakeSegmenter{words: map[string][]string{"免费领取": {"的"}}}
	toks := tokenizeTokens(Input{Body: "免费领取"}, segmenter)
	byHash := map[uint64]token{}
	for _, tk := range toks {
		byHash[tk.hash] = tk
	}
	for _, bigram := range []string{"免费", "费领", "领取"} {
		if _, ok := byHash[refCJKHashes[bigram]]; !ok {
			t.Fatalf("fallback bigram %s missing from %+v", bigram, toks)
		}
	}
}

func TestDigestStable(t *testing.T) {
	a := Digest([]uint64{3, 1, 2})
	b := Digest([]uint64{1, 2, 3})
	if a != b {
		t.Fatalf("digest depends on input order: %s vs %s", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("digest length = %d, want 64 (sha256 hex)", len(a))
	}
}
