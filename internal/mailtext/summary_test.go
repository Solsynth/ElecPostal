package mailtext

import "testing"

func TestSummaryRemovesHTMLMarkupAndNonContent(t *testing.T) {
	got := Summary(`<html><head><style>.hidden{display:none}</style></head><body><h1>Hello&nbsp;world</h1><p>First <b>message</b>.</p><script>alert("secret")</script></body></html>`, "text/html", 100)
	if got != "Hello world First message." {
		t.Fatalf("Summary() = %q, want readable text without style/script content", got)
	}
}

func TestSummaryKeepsPlainTextAndBoundsByRunes(t *testing.T) {
	if got := Summary(" Héllo\nworld ", "text/plain", 7); got != "Héllo w" {
		t.Fatalf("Summary() = %q, want bounded plain text", got)
	}
}
