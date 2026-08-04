package mailtext

import (
	"testing"
	"unicode/utf8"
)

func TestDecodeBodyDeclaredCharset(t *testing.T) {
	// 0xE9 = é, 0xBB = » in ISO-8859-1: the bytes that broke PostgreSQL
	// persistence with SQLSTATE 22021.
	got := DecodeBody([]byte{0x63, 0x61, 0x66, 0xE9, 0x20, 0xBB}, "iso-8859-1")
	if want := "café »"; got != want {
		t.Fatalf("DecodeBody(iso-8859-1) = %q, want %q", got, want)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("DecodeBody result is not valid UTF-8: %q", got)
	}
}

func TestDecodeBodyWindows1252Fallback(t *testing.T) {
	// 0x92 = U+2019 right single quote in Windows-1252; no charset declared.
	got := DecodeBody([]byte("smart \x92 quote"), "")
	if want := "smart ’ quote"; got != want {
		t.Fatalf("DecodeBody(fallback) = %q, want %q", got, want)
	}
}

func TestDecodeBodyKeepsValidUTF8(t *testing.T) {
	got := DecodeBody([]byte("héllo ✓"), "")
	if want := "héllo ✓"; got != want {
		t.Fatalf("DecodeBody(valid) = %q, want %q", got, want)
	}
}

func TestDecodeHeaderEncodedWord(t *testing.T) {
	got := DecodeHeader("=?iso-8859-1?Q?caf=E9?=")
	if want := "café"; got != want {
		t.Fatalf("DecodeHeader(encoded word) = %q, want %q", got, want)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("DecodeHeader result is not valid UTF-8: %q", got)
	}
}

func TestDecodeHeaderRawBytes(t *testing.T) {
	got := DecodeHeader("caf\xE9 \xBB")
	if want := "café »"; got != want {
		t.Fatalf("DecodeHeader(raw bytes) = %q, want %q", got, want)
	}
}

func TestToValidUTF8(t *testing.T) {
	if got := ToValidUTF8("already fine ✓"); got != "already fine ✓" {
		t.Fatalf("ToValidUTF8(valid) = %q", got)
	}
	got := ToValidUTF8("bad \xbb byte")
	if want := "bad \uFFFD byte"; got != want {
		t.Fatalf("ToValidUTF8(invalid) = %q, want %q", got, want)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("ToValidUTF8 result is not valid UTF-8: %q", got)
	}
}
