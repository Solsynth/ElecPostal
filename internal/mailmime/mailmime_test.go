package mailmime

import (
	"bytes"
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"os"
	"strings"
	"testing"
)

type fakeSource struct{ files map[string][]byte }

func (s fakeSource) Open(_ context.Context, id string) (AttachmentReader, error) {
	data, ok := s.files[id]
	if !ok {
		return AttachmentReader{}, os.ErrNotExist
	}
	return AttachmentReader{Metadata: AttachmentMetadata{ID: id}, Content: io.NopCloser(bytes.NewReader(data))}, nil
}

func TestRenderStreamsInlineAndRegularAttachments(t *testing.T) {
	source := MessageSource{
		FromAddress: "sender@example.test", Subject: "invoice", Body: "<p>hello<img src=\"cid:logo\"></p>", BodyType: "text/html",
		Recipients: []Recipient{{Address: "recipient@example.test", Kind: "to"}},
		Manifest: Manifest{Version: ManifestVersion, BodyType: "text/html", Parts: []Part{
			{AttachmentID: "logo", Filename: "logo.png", MimeType: "image/png", Size: 3, ContentID: "logo", Disposition: "inline"},
			{AttachmentID: "pdf", Filename: "invoice.pdf", MimeType: "application/pdf", Size: 4, Disposition: "attachment"},
		}},
		Attachments: map[string]Part{
			"logo": {AttachmentID: "logo", Filename: "logo.png", MimeType: "image/png", Size: 3, ContentID: "logo", Disposition: "inline"},
			"pdf":  {AttachmentID: "pdf", Filename: "invoice.pdf", MimeType: "application/pdf", Size: 4, Disposition: "attachment"},
		},
		Source: fakeSource{files: map[string][]byte{"logo": {1, 2, 3}, "pdf": {4, 5, 6, 7}}},
	}
	rendered, err := RenderBytes(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	count, err := Count(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(rendered)) != count {
		t.Fatalf("Count() = %d, rendered length = %d", count, len(rendered))
	}
	message, err := mail.ReadMessage(bytes.NewReader(rendered))
	if err != nil {
		t.Fatal(err)
	}
	topType, params, err := mime.ParseMediaType(message.Header.Get("Content-Type"))
	if err != nil || topType != "multipart/mixed" {
		t.Fatalf("top MIME type = %q: %v", topType, err)
	}
	reader := multipart.NewReader(message.Body, params["boundary"])
	var sawInline, sawPDF bool
	for {
		part, nextErr := reader.NextPart()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		contentType, nested, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if strings.HasPrefix(contentType, "multipart/") {
			nestedReader := multipart.NewReader(part, nested["boundary"])
			for {
				nestedPart, nestedErr := nestedReader.NextPart()
				if nestedErr == io.EOF {
					break
				}
				if nestedErr != nil {
					t.Fatal(nestedErr)
				}
				if nestedPart.Header.Get("Content-ID") == "<logo>" {
					data, _ := io.ReadAll(nestedPart)
					if string(data) != "AQID" {
						t.Fatalf("inline data = %q", data)
					}
					sawInline = true
				}
			}
		} else if strings.Contains(part.Header.Get("Content-Disposition"), "invoice.pdf") {
			data, _ := io.ReadAll(part)
			if string(data) != "BAUGBw==" {
				t.Fatalf("PDF data = %q", data)
			}
			sawPDF = true
		}
	}
	if !sawInline || !sawPDF {
		t.Fatalf("attachments found inline=%v pdf=%v", sawInline, sawPDF)
	}
}

func TestRenderRemovesLeadingContentTypeFromHTMLBody(t *testing.T) {
	source := MessageSource{
		Body:     "Content-Type: text/html; charset=utf-8\r\n\r\n<p>Hello</p>",
		BodyType: "text/html",
		Manifest: Manifest{Version: ManifestVersion, BodyType: "text/html"},
	}
	rendered, err := RenderBytes(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	message, err := mail.ReadMessage(bytes.NewReader(rendered))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(message.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "<p>Hello</p>" {
		t.Fatalf("HTML body = %q, want %q", body, "<p>Hello</p>")
	}
}

func TestRenderKeepsHTMLBodyThatStartsWithDifferentContentType(t *testing.T) {
	body := "Content-Type: text/html; charset=windows-1252\r\n\r\n<p>Hello</p>"
	rendered, err := RenderBytes(context.Background(), MessageSource{
		Body: body, BodyType: "text/html",
		Manifest: Manifest{Version: ManifestVersion, BodyType: "text/html"},
	})
	if err != nil {
		t.Fatal(err)
	}
	message, err := mail.ReadMessage(bytes.NewReader(rendered))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(message.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("HTML body = %q, want %q", got, body)
	}
}
func TestRenderPreservesAbsentContentType(t *testing.T) {
	parsed, err := ParseMessage([]byte("From: sender@example.test\r\nSubject: hello\r\n\r\nbody\r\n"), "sender@example.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.OmitContentType || parsed.BodyType != "text/plain" {
		t.Fatalf("parsed MIME metadata = omit=%v type=%q", parsed.OmitContentType, parsed.BodyType)
	}
	rendered, err := RenderBytes(context.Background(), MessageSource{
		FromAddress: parsed.FromAddress,
		Subject:     parsed.Subject,
		Body:        parsed.Body,
		BodyType:    parsed.BodyType,
		Manifest: Manifest{
			Version:         ManifestVersion,
			BodyType:        parsed.BodyType,
			OmitContentType: parsed.OmitContentType,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	message, err := mail.ReadMessage(bytes.NewReader(rendered))
	if err != nil {
		t.Fatal(err)
	}
	if got := message.Header.Get("Content-Type"); got != "" {
		t.Fatalf("Content-Type = %q, want omitted", got)
	}
	body, err := io.ReadAll(message.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "body\r\n" {
		t.Fatalf("body = %q", body)
	}
}

func TestRenderWritesThreadingHeaders(t *testing.T) {
	source := MessageSource{
		FromAddress: "sender@example.test", Subject: "Re: Plan", Body: "reply", BodyType: "text/plain",
		Recipients: []Recipient{{Address: "recipient@example.test", Kind: "to"}},
		MessageID:  "reply-1@example.test",
		InReplyTo:  "root-1@example.test",
		References: "root-1@example.test parent-1@example.test",
	}
	rendered, err := RenderBytes(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	message, err := mail.ReadMessage(bytes.NewReader(rendered))
	if err != nil {
		t.Fatal(err)
	}
	if got := message.Header.Get("Message-ID"); got != "<reply-1@example.test>" {
		t.Fatalf("Message-ID = %q, want <reply-1@example.test>", got)
	}
	if got := message.Header.Get("In-Reply-To"); got != "<root-1@example.test>" {
		t.Fatalf("In-Reply-To = %q, want <root-1@example.test>", got)
	}
	if got := message.Header.Get("References"); got != "<root-1@example.test> <parent-1@example.test>" {
		t.Fatalf("References = %q, want both ids bracketed", got)
	}
}

// TestParseContentIDOnTextBodyIsNotAttachment pins the Outlook behavior where
// the outbound transport tags a plain text body with a Content-ID header. The
// body must stay the message body instead of being demoted to a spurious
// "attachment" part, which previously emptied the message.
func TestParseContentIDOnTextBodyIsNotAttachment(t *testing.T) {
	raw := []byte("From: sender@example.test\r\nTo: recipient@example.test\r\nSubject: hello\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\nContent-ID: <E4AD0DC3E66A4144A86EE7C2DC287ABF@jpnprd01.prod.outlook.com>\r\n\r\n5L2g5aW9\r\n")
	parsed, err := ParseMessage(raw, "sender@example.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Body != "你好" {
		t.Fatalf("body = %q, want %q", parsed.Body, "你好")
	}
	if len(parsed.Attachments) != 0 {
		t.Fatalf("attachments = %#v, want none", parsed.Attachments)
	}
}

// TestParseContentIDInlineImageIsAttachment keeps the inline-resource case: a
// non-text part referenced by Content-ID remains an attachment even without an
// explicit disposition or filename.
func TestParseContentIDInlineImageIsAttachment(t *testing.T) {
	raw := []byte("Content-Type: multipart/related; boundary=rel\r\n\r\n--rel\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>Hello <img src=\"cid:logo-123\"></p>\r\n--rel\r\nContent-Type: image/png\r\nContent-ID: <logo-123>\r\nContent-Transfer-Encoding: base64\r\n\r\naW1hZ2U=\r\n--rel--")
	parsed, err := ParseMessage(raw, "sender@example.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Attachments) != 1 {
		t.Fatalf("attachments = %#v, want the inline image", parsed.Attachments)
	}
	if parsed.Attachments[0].ContentID != "logo-123" || parsed.Attachments[0].MimeType != "image/png" {
		t.Fatalf("inline image metadata = %#v", parsed.Attachments[0])
	}
}

func TestRenderOmitsThreadingHeadersWhenAbsent(t *testing.T) {
	source := MessageSource{
		FromAddress: "sender@example.test", Subject: "Fresh", Body: "body", BodyType: "text/plain",
		Recipients: []Recipient{{Address: "recipient@example.test", Kind: "to"}},
	}
	rendered, err := RenderBytes(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	message, err := mail.ReadMessage(bytes.NewReader(rendered))
	if err != nil {
		t.Fatal(err)
	}
	for _, header := range []string{"Message-ID", "In-Reply-To", "References"} {
		if got := message.Header.Get(header); got != "" {
			t.Fatalf("%s = %q, want omitted", header, got)
		}
	}
}
