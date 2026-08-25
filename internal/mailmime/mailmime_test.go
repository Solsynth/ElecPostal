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
