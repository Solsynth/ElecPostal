// Package mailtext decodes raw RFC 5322 message bytes into UTF-8 strings.
//
// Mail in the wild is frequently 8-bit (ISO-8859-1/Windows-1252) despite
// RFC 5322 mandating ASCII, and headers may carry RFC 2047 encoded words in
// non-UTF-8 charsets. Persisting such bytes verbatim into PostgreSQL text
// columns fails with SQLSTATE 22021 ("invalid byte sequence for encoding
// UTF8"), so every string that ends up in a database column must pass through
// one of the decoders here.
package mailtext

import (
	"bytes"
	"io"
	"mime"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/htmlindex"
)

// fallbackEncoding decodes 8-bit bodies and headers that declare no usable
// charset. Real-world non-UTF-8 mail is overwhelmingly Windows-1252, which is
// a superset of ISO-8859-1 in the 0x80-0x9F range.
var fallbackEncoding = charmap.Windows1252

// replacement marks byte sequences that are not representable in any declared
// or fallback charset.
var replacement = []byte("\uFFFD")

// DecodeBody converts raw message body bytes to a valid UTF-8 string. The
// declared charset wins when resolvable; otherwise already-valid UTF-8 is kept
// as-is and any other byte sequence is decoded as Windows-1252. The result is
// always valid UTF-8 and therefore safe to persist into text columns.
func DecodeBody(data []byte, charset string) string {
	charset = strings.TrimSpace(charset)
	if charset == "" {
		if utf8.Valid(data) {
			return string(data)
		}
		charset = "windows-1252"
	}
	enc := encodingFor(charset)
	decoded, err := enc.NewDecoder().Bytes(data)
	if err != nil {
		decoded = data
	}
	return string(bytes.ToValidUTF8(decoded, replacement))
}

// DecodeHeader decodes RFC 2047 encoded words with charset awareness (including
// non-UTF-8 charsets such as iso-8859-1) and guarantees that the surrounding
// raw header bytes are valid UTF-8 as well. Raw bytes are treated like
// DecodeBody: kept when valid UTF-8, otherwise decoded as Windows-1252.
func DecodeHeader(value string) string {
	decoder := &mime.WordDecoder{CharsetReader: func(charset string, input io.Reader) (io.Reader, error) {
		return encodingFor(charset).NewDecoder().Reader(input), nil
	}}
	decoded, err := decoder.DecodeHeader(value)
	if err != nil {
		decoded = value
	}
	if !utf8.ValidString(decoded) {
		// Raw (non-encoded-word) bytes pass through the word decoder
		// untouched; decode them like a body with no declared charset.
		return DecodeBody([]byte(decoded), "")
	}
	return decoded
}

// ToValidUTF8 returns s unchanged when it is already valid UTF-8 and otherwise
// replaces each invalid byte sequence with U+FFFD. It is the persist-boundary
// safety net for string fields whose producers are not charset-aware.
func ToValidUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return string(bytes.ToValidUTF8([]byte(s), replacement))
}

func encodingFor(charset string) encoding.Encoding {
	if charset == "" {
		return fallbackEncoding
	}
	if enc, err := htmlindex.Get(charset); err == nil {
		return enc
	}
	return fallbackEncoding
}
