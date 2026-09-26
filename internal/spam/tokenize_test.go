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

func TestTokenizeDeterministicStableDeduped(t *testing.T) {
	in := Input{Subject: "Hello World", Body: "Hello world hello", FromAddress: "a@example.com"}
	a := Tokenize(in)
	b := Tokenize(in)
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
	toks := tokenizeTokens(Input{Subject: "Hello World", Body: "one two three", FromAddress: "a@example.com"})
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
	toks := tokenizeTokens(Input{Subject: "hi a b", Body: "be go to"})
	if len(toks) != 0 {
		t.Fatalf("short words should be dropped, got %d tokens", len(toks))
	}
}

func TestTokenizeCapsAtMaxTokens(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 500; i++ {
		sb.WriteString("abcdef ")
	}
	toks := tokenizeTokens(Input{Body: sb.String()})
	if len(toks) > maxTokens {
		t.Fatalf("token count %d exceeds cap %d", len(toks), maxTokens)
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
