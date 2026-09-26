package spam

import (
	"strings"
	"testing"

	"gorm.io/datatypes"
)

func auth(j string) datatypes.JSON { return datatypes.JSON(j) }

func hasSymbol(symbols []Symbol, name string) bool {
	for _, s := range symbols {
		if s.Name == name {
			return true
		}
	}
	return false
}

func symbolWeight(symbols []Symbol, name string) (float64, bool) {
	for _, s := range symbols {
		if s.Name == name {
			return s.Weight, true
		}
	}
	return 0, false
}

// TestRulesSingleSymbol drives each rule in isolation: one Input per row and
// asserts the expected symbol fired with its exact weight. Inputs may also
// trip MISSING_SUBJECT (empty subject) — only the target symbol is asserted.
func TestRulesSingleSymbol(t *testing.T) {
	cases := []struct {
		name   string
		input  Input
		symbol string
		weight float64
	}{
		{"block rule", Input{IsBlocked: true}, SymbolBlockRule, weightBlockRule},
		{"spf fail", Input{Authentication: auth(`{"spf":"fail"}`)}, SymbolSPFFail, weightSPFFail},
		{"spf softfail", Input{Authentication: auth(`{"spf":"softfail"}`)}, SymbolSPFSoftfail, weightSPFSoftfail},
		{"spf pass", Input{Authentication: auth(`{"spf":"pass"}`)}, SymbolSPFAllow, weightSPFAllow},
		{"dkim fail", Input{Authentication: auth(`{"dkim":"fail"}`)}, SymbolDKIMReject, weightDKIMReject},
		{"dkim permerror", Input{Authentication: auth(`{"dkim":"permerror"}`)}, SymbolDKIMPermfail, weightDKIMPermfail},
		{"dkim pass", Input{Authentication: auth(`{"dkim":"pass"}`)}, SymbolDKIMAllow, weightDKIMAllow},
		{"dmarc reject", Input{Authentication: auth(`{"dmarc":"reject"}`)}, SymbolDMARCPolicyReject, weightDMARCPolicyReject},
		{"dmarc quarantine", Input{Authentication: auth(`{"dmarc":"quarantine"}`)}, SymbolDMARCPolicyQuarantine, weightDMARCPolicyQuarantine},
		{"dmarc allow", Input{Authentication: auth(`{"dmarc":"allow"}`)}, SymbolDMARCPolicyAllow, weightDMARCPolicyAllow},
		{"envelope neq from", Input{EnvelopeFrom: "spammer@evil.example", FromAddress: "victim@good.example"}, SymbolEnvelopeFromNeqFrom, weightEnvelopeFromNeqFrom},
		{"spoof display name", Input{FromName: "Support", FromAddress: "scammer@evil.example"}, SymbolSpoofDisplayName, weightSpoofDisplayName},
		{"missing subject", Input{Subject: "   "}, SymbolMissingSubject, weightMissingSubject},
		{"money phrases", Input{Subject: "Urgent wire transfer needed"}, SymbolMoneyPhrases, weightMoneyPhrases},
		{"excessive uppercase", Input{Body: strings.Repeat("BUY NOW ", 30)}, SymbolExcessiveUppercase, weightExcessiveUppercase},
		{"excessive exclamation", Input{Body: "a!!!b!!!c!!!d!!!e!!!"}, SymbolExcessiveExclamation, weightExcessiveExclamation},
		{"many urls", Input{Body: "x https://a.example/1 https://a.example/2 https://a.example/3 https://a.example/4 https://a.example/5 https://a.example/6"}, SymbolManyURLs, weightManyURLsPerFive},
		{"many urls capped", Input{Body: urlsBody(30)}, SymbolManyURLs, weightManyURLsCap},
		{"zero width", Input{Body: "hi\u200b there"}, SymbolZeroWidthObfuscation, weightZeroWidthObfuscation},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewService(Config{}, nil).Score(t.Context(), tc.input)
			w, ok := symbolWeight(r.Symbols, tc.symbol)
			if !ok {
				t.Fatalf("symbol %s not present; got %+v", tc.symbol, r.Symbols)
			}
			if w != tc.weight {
				t.Fatalf("symbol %s weight = %v, want %v", tc.symbol, w, tc.weight)
			}
		})
	}
}

// TestRulesNegative asserts rules do NOT fire for non-matching inputs.
func TestRulesNegative(t *testing.T) {
	svc := NewService(Config{}, nil)
	ctx := t.Context()
	// DKIM pass suppresses the spoof symbol.
	r := svc.Score(ctx, Input{FromName: "Support", FromAddress: "scammer@evil.example", Authentication: auth(`{"dkim":"pass"}`)})
	if hasSymbol(r.Symbols, SymbolSpoofDisplayName) {
		t.Fatalf("spoof fired despite dkim pass: %+v", r.Symbols)
	}
	// Same-domain envelope and From: no ENVELOPE_FROM_NEQ_FROM.
	r = svc.Score(ctx, Input{EnvelopeFrom: "a@example.com", FromAddress: "b@example.com"})
	if hasSymbol(r.Symbols, SymbolEnvelopeFromNeqFrom) {
		t.Fatalf("envelope neq from fired for same domain: %+v", r.Symbols)
	}
	// Spoof display name equal to local part is not spoofing.
	r = svc.Score(ctx, Input{FromName: "support", FromAddress: "support@bank.example"})
	if hasSymbol(r.Symbols, SymbolSpoofDisplayName) {
		t.Fatalf("spoof fired for self-named sender: %+v", r.Symbols)
	}
}

func urlsBody(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(" https://u.example/x")
	}
	return b.String()
}
