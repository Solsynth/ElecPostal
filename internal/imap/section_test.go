package imap

import (
	"bytes"
	"testing"

	goimap "github.com/emersion/go-imap"
)

func TestSectionDataReturnsRequestedMIMEPart(t *testing.T) {
	raw := []byte("Content-Type: multipart/mixed; boundary=boundary\r\n\r\n--boundary\r\nContent-Type: text/plain\r\n\r\nhello\r\n--boundary\r\nContent-Type: image/png\r\nContent-Transfer-Encoding: base64\r\n\r\nAQID\r\n--boundary--\r\n")
	section, err := goimap.ParseBodySectionName("BODY[2]")
	if err != nil {
		t.Fatal(err)
	}
	data, err := sectionData(raw, section)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("Content-Type: image/png")) || !bytes.Contains(data, []byte("AQID")) || bytes.Equal(data, raw) {
		t.Fatalf("section = %q", data)
	}
}
