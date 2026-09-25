package mailmime

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"strings"

	"github.com/google/uuid"
)

const ManifestVersion = 1

// Manifest is the durable, attachment-free description of a message's MIME
// shape. Attachment bytes are addressed only by DysonFS file ID.
type Manifest struct {
	Version         int    `json:"version"`
	BodyType        string `json:"body_type"`
	OmitContentType bool   `json:"omit_content_type,omitempty"`
	Parts           []Part `json:"parts"`
}

type Part struct {
	AttachmentID string `json:"attachment_id,omitempty"`
	Filename     string `json:"filename,omitempty"`
	MimeType     string `json:"mime_type,omitempty"`
	Size         int64  `json:"size,omitempty"`
	ContentID    string `json:"content_id,omitempty"`
	Disposition  string `json:"disposition,omitempty"`
}

type Recipient struct {
	Address string
	Name    string
	Kind    string
}

type AttachmentMetadata struct {
	ID       string
	Name     string
	MimeType string
	Size     int64
	Hash     string
}

type AttachmentReader struct {
	Metadata AttachmentMetadata
	Content  io.ReadCloser
}

type AttachmentSource interface {
	Open(context.Context, string) (AttachmentReader, error)
}

type MessageSource struct {
	FromAddress string
	FromName    string
	Subject     string
	Body        string
	BodyType    string
	Recipients  []Recipient
	Manifest    Manifest
	Attachments map[string]Part
	Source      AttachmentSource
}

// Render writes a fresh canonical RFC 5322 message to dst. It never buffers an
// attachment; each referenced file is opened and copied once.
func Render(ctx context.Context, source MessageSource, dst io.Writer) error {
	if source.Source == nil && hasAttachments(source.Manifest) {
		return errors.New("attachment source is required")
	}
	if source.Manifest.Version != 0 && source.Manifest.Version != ManifestVersion {
		return fmt.Errorf("unsupported MIME manifest version %d", source.Manifest.Version)
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := writeHeaders(source, dst); err != nil {
		return err
	}
	parts := orderedParts(source)
	inline, regular := splitAttachments(parts)
	bodyType := normalizeBodyType(source.BodyType, source.Manifest.BodyType)
	body := stripLeadingContentType(source.Body, bodyType)
	switch {
	case len(parts) == 0:
		return writeBody(dst, bodyType, body, source.Manifest.OmitContentType)
	case len(regular) == 0:
		return writeRelated(ctx, dst, bodyType, body, inline, source, source.Manifest.OmitContentType)
	default:
		boundary := newBoundary()
		if _, err := fmt.Fprintf(dst, "Content-Type: multipart/mixed; boundary=%q\r\n\r\n", boundary); err != nil {
			return err
		}
		writer := multipart.NewWriter(dst)
		if err := writer.SetBoundary(boundary); err != nil {
			return err
		}
		if len(inline) > 0 {
			if err := writeRelatedPart(ctx, writer, bodyType, body, inline, source, source.Manifest.OmitContentType); err != nil {
				return err
			}
		} else if err := writeBodyPart(writer, bodyType, body, source.Manifest.OmitContentType); err != nil {
			return err
		}
		for _, part := range regular {
			if err := writeAttachmentPart(ctx, writer, part, source); err != nil {
				return err
			}
		}
		return writer.Close()
	}
}

func Count(ctx context.Context, source MessageSource) (int64, error) {
	var counter countingWriter
	if err := Render(ctx, source, &counter); err != nil {
		return 0, err
	}
	return counter.n, nil
}

func RenderBytes(ctx context.Context, source MessageSource) ([]byte, error) {
	var b strings.Builder
	if err := Render(ctx, source, &b); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

func writeHeaders(source MessageSource, dst io.Writer) error {
	from := source.FromAddress
	if source.FromName != "" {
		from = (&mail.Address{Name: source.FromName, Address: from}).String()
	}
	if _, err := fmt.Fprintf(dst, "From: %s\r\n", cleanHeader(from)); err != nil {
		return err
	}
	for _, kind := range []string{"to", "cc"} {
		values := make([]string, 0)
		for _, recipient := range source.Recipients {
			if strings.EqualFold(recipient.Kind, kind) {
				address := recipient.Address
				if recipient.Name != "" {
					address = (&mail.Address{Name: recipient.Name, Address: recipient.Address}).String()
				}
				values = append(values, address)
			}
		}
		if len(values) > 0 {
			if _, err := fmt.Fprintf(dst, "%s: %s\r\n", title(kind), cleanHeader(strings.Join(values, ", "))); err != nil {
				return err
			}
		}
	}
	if _, err := fmt.Fprintf(dst, "Subject: %s\r\nMIME-Version: 1.0\r\n", cleanHeader(source.Subject)); err != nil {
		return err
	}
	return nil
}

func writeBody(dst io.Writer, bodyType, body string, omitContentType bool) error {
	if omitContentType {
		_, err := fmt.Fprintf(dst, "\r\n%s", body)
		return err
	}
	_, err := fmt.Fprintf(dst, "Content-Type: %s; charset=utf-8\r\n\r\n%s", bodyType, body)
	return err
}

func writeBodyPart(writer *multipart.Writer, bodyType, body string, omitContentType bool) error {
	header := textproto.MIMEHeader{}
	if !omitContentType {
		header.Set("Content-Type", bodyType+"; charset=utf-8")
	}
	part, err := writer.CreatePart(header)
	if err != nil {
		return err
	}
	_, err = io.WriteString(part, body)
	return err
}

func writeRelated(ctx context.Context, dst io.Writer, bodyType, body string, inline []Part, source MessageSource, omitContentType bool) error {
	boundary := newBoundary()
	if _, err := fmt.Fprintf(dst, "Content-Type: multipart/related; boundary=%q\r\n\r\n", boundary); err != nil {
		return err
	}
	writer := multipart.NewWriter(dst)
	if err := writer.SetBoundary(boundary); err != nil {
		return err
	}
	if err := writeBodyPart(writer, bodyType, body, omitContentType); err != nil {
		return err
	}
	for _, part := range inline {
		if err := writeAttachmentPart(ctx, writer, part, source); err != nil {
			return err
		}
	}
	return writer.Close()
}

func writeRelatedPart(ctx context.Context, outer *multipart.Writer, bodyType, body string, inline []Part, source MessageSource, omitContentType bool) error {
	header := textproto.MIMEHeader{}
	boundary := newBoundary()
	header.Set("Content-Type", fmt.Sprintf("multipart/related; boundary=%q", boundary))
	part, err := outer.CreatePart(header)
	if err != nil {
		return err
	}
	writer := multipart.NewWriter(part)
	if err := writer.SetBoundary(boundary); err != nil {
		return err
	}
	if err := writeBodyPart(writer, bodyType, body, omitContentType); err != nil {
		return err
	}
	for _, attachment := range inline {
		if err := writeAttachmentPart(ctx, writer, attachment, source); err != nil {
			return err
		}
	}
	return writer.Close()
}

func writeAttachmentPart(ctx context.Context, writer *multipart.Writer, part Part, source MessageSource) error {
	if strings.TrimSpace(part.AttachmentID) == "" {
		return errors.New("MIME attachment has no DysonFS file ID")
	}
	metadata, ok := source.Attachments[part.AttachmentID]
	if !ok {
		metadata = part
	}
	if metadata.Filename == "" {
		metadata.Filename = part.Filename
	}
	if metadata.MimeType == "" {
		metadata.MimeType = part.MimeType
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Type", mime.FormatMediaType(metadata.MimeType, map[string]string{"name": metadata.Filename}))
	disposition := metadata.Disposition
	if disposition == "" {
		disposition = "attachment"
	}
	header.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": metadata.Filename}))
	if metadata.ContentID != "" {
		header.Set("Content-ID", formatContentID(metadata.ContentID))
	}
	header.Set("Content-Transfer-Encoding", "base64")
	partWriter, err := writer.CreatePart(header)
	if err != nil {
		return err
	}
	reader, err := source.Source.Open(ctx, metadata.AttachmentID)
	if err != nil {
		return fmt.Errorf("open attachment %q: %w", metadata.AttachmentID, err)
	}
	if reader.Content == nil {
		return errors.New("attachment source returned a nil reader")
	}
	defer reader.Content.Close()
	encoder := base64.NewEncoder(base64.StdEncoding, &lineWriter{dst: partWriter})
	buffer := make([]byte, 64*1024)
	for {
		if err := contextError(ctx); err != nil {
			_ = encoder.Close()
			return err
		}
		count, readErr := reader.Content.Read(buffer)
		if count > 0 {
			if _, err := encoder.Write(buffer[:count]); err != nil {
				_ = encoder.Close()
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = encoder.Close()
			return fmt.Errorf("read attachment %q: %w", metadata.AttachmentID, readErr)
		}
	}
	if err := encoder.Close(); err != nil {
		return err
	}
	return nil
}

func orderedParts(source MessageSource) []Part {
	parts := append([]Part(nil), source.Manifest.Parts...)
	return parts
}

func splitAttachments(parts []Part) (inline, regular []Part) {
	for _, part := range parts {
		if strings.EqualFold(part.Disposition, "inline") || part.ContentID != "" {
			inline = append(inline, part)
		} else {
			regular = append(regular, part)
		}
	}
	return inline, regular
}

func hasAttachments(manifest Manifest) bool { return len(manifest.Parts) > 0 }

func normalizeBodyType(bodyType, manifestType string) string {
	if bodyType == "" {
		bodyType = manifestType
	}
	if bodyType != "text/html" {
		return "text/plain"
	}
	return bodyType
}

func stripLeadingContentType(body, bodyType string) string {
	lineEnd := strings.Index(body, "\n")
	if lineEnd < 0 {
		return body
	}
	headerLine := strings.TrimSuffix(body[:lineEnd], "\r")
	if !strings.HasPrefix(strings.ToLower(headerLine), "content-type:") {
		return body
	}
	mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(headerLine[len("content-type:"):]))
	if err != nil || !strings.EqualFold(mediaType, bodyType) || !strings.EqualFold(params["charset"], "utf-8") {
		return body
	}
	separatorEnd := lineEnd + 1
	if separatorEnd < len(body) && body[separatorEnd] == '\r' {
		separatorEnd++
	}
	if separatorEnd >= len(body) || body[separatorEnd] != '\n' {
		return body
	}
	return body[separatorEnd+1:]
}

func formatContentID(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "<") && strings.HasSuffix(value, ">") {
		return value
	}
	return "<" + value + ">"
}

func cleanHeader(value string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(value)
}

func title(value string) string {
	if value == "to" {
		return "To"
	}
	return "Cc"
}

func newBoundary() string { return "=_elecpostal_" + strings.ReplaceAll(uuid.NewString(), "-", "") }

func contextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) { w.n += int64(len(p)); return len(p), nil }

type lineWriter struct {
	dst    io.Writer
	width  int
	column int
}

func (w *lineWriter) Write(p []byte) (int, error) {
	if w.width == 0 {
		w.width = 76
	}
	written := 0
	for len(p) > 0 {
		remaining := w.width - w.column
		if remaining > len(p) {
			remaining = len(p)
		}
		count, err := w.dst.Write(p[:remaining])
		written += count
		w.column += count
		p = p[count:]
		if err != nil {
			return written, err
		}
		if count < remaining {
			return written, io.ErrShortWrite
		}
		if w.column == w.width {
			if _, err := io.WriteString(w.dst, "\r\n"); err != nil {
				return written, err
			}
			w.column = 0
		}
	}
	return written, nil
}
