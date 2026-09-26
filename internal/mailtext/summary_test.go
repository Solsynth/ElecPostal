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

func TestPreviewPrefersTheStoredSummary(t *testing.T) {
	if got := Preview("Agent summary", `<p>Body text</p>`, "text/html", 100); got != "Agent summary" {
		t.Fatalf("Preview() = %q, want the stored summary", got)
	}
	if got := Preview("   ", "Body text", "text/plain", 100); got != "Body text" {
		t.Fatalf("Preview() = %q, want the body preview for a blank summary", got)
	}
	if got := Preview("", `<p>Body <b>text</b></p>`, "text/html", 4); got != "Body" {
		t.Fatalf("Preview() = %q, want the bounded body preview", got)
	}
}
