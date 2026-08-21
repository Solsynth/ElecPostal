package pop3

import (
	"strings"
	"testing"
)

func TestTOPExcludesAttachmentPayload(t *testing.T) {
	raw := []byte("From: a@example.test\r\nContent-Type: multipart/mixed; boundary=boundary\r\n\r\n--boundary\r\nContent-Type: text/plain\r\n\r\nhello\r\n--boundary\r\nContent-Disposition: attachment; filename=secret.bin\r\nContent-Type: application/octet-stream\r\n\r\nU0VDUkVU\r\n--boundary--\r\n")
	result := string(top(raw, "10"))
	if !strings.Contains(result, "hello") || strings.Contains(result, "U0VDUkVU") || strings.Contains(result, "secret.bin") {
		t.Fatalf("TOP included attachment data: %q", result)
	}
}
