package mailmime

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"path/filepath"
	"strings"

	"src.solsynth.dev/sosys/elecpostal/internal/mailtext"
)

type IncomingAttachment struct {
	Filename    string
	MimeType    string
	Size        int64
	ContentID   string
	Disposition string
	Content     io.Reader
}

type ParsedMessage struct {
	ID              string
	FromAddress     string
	FromName        string
	Subject         string
	Body            string
	BodyType        string
	OmitContentType bool
	// InReplyTo and References carry the RFC 5322 reply chain: the Message-IDs
	// a message answers (immediate parent) and the ancestors before it. Both
	// are normalized without angle brackets, so they can be matched against
	// stored message_ids as-is.
	InReplyTo   []string
	References  []string
	To          []Recipient
	Cc          []Recipient
	Attachments []IncomingAttachment
}

func ParseMessage(raw []byte, envelopeFrom string, envelopeRecipients []Recipient) (ParsedMessage, error) {
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ParsedMessage{}, err
	}
	result := ParsedMessage{
		ID: strings.TrimSpace(message.Header.Get("Message-ID")), FromAddress: envelopeFrom,
		BodyType: "text/plain", OmitContentType: strings.TrimSpace(message.Header.Get("Content-Type")) == "",
	}
	result.InReplyTo = splitMessageIDs(message.Header.Get("In-Reply-To"))
	result.References = splitMessageIDs(message.Header.Get("References"))
	if from, err := message.Header.AddressList("From"); err == nil && len(from) > 0 {
		result.FromAddress = strings.ToLower(from[0].Address)
		result.FromName = mailtext.DecodeHeader(from[0].Name)
	}
	result.Subject = mailtext.DecodeHeader(message.Header.Get("Subject"))
	result.To = recipients(message.Header, "To", "to")
	result.Cc = recipients(message.Header, "Cc", "cc")
	if len(result.To) == 0 {
		result.To = append(result.To, envelopeRecipients...)
	}
	plain, html, attachments, err := ParseEntity(message.Header, message.Body)
	if err != nil {
		return ParsedMessage{}, err
	}
	if html != "" {
		result.Body, result.BodyType = html, "text/html"
	} else {
		result.Body = plain
	}
	result.Attachments = attachments
	return result, nil
}

func recipients(header mail.Header, key, kind string) []Recipient {
	list, err := header.AddressList(key)
	if err != nil {
		return nil
	}
	result := make([]Recipient, 0, len(list))
	for _, address := range list {
		result = append(result, Recipient{Address: strings.ToLower(address.Address), Name: mailtext.DecodeHeader(address.Name), Kind: kind})
	}
	return result
}

// splitMessageIDs normalizes an RFC 5322 message-id header value
// (In-Reply-To, References) into individual Message-IDs without angle
// brackets, trimming any surrounding whitespace and continuation lines.
func splitMessageIDs(value string) []string {
	fields := strings.Fields(value)
	ids := make([]string, 0, len(fields))
	for _, field := range fields {
		id := strings.Trim(field, "<> ")
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func ParseEntity(header mail.Header, body io.Reader) (plain, html string, attachments []IncomingAttachment, err error) {
	mediaType, params, parseErr := mime.ParseMediaType(header.Get("Content-Type"))
	if parseErr != nil {
		mediaType, params = "text/plain", map[string]string{}
	}
	disposition, dispositionParams, _ := mime.ParseMediaType(header.Get("Content-Disposition"))
	contentID := strings.Trim(header.Get("Content-ID"), "<> ")
	if strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		reader := multipart.NewReader(body, params["boundary"])
		for {
			part, nextErr := reader.NextPart()
			if errors.Is(nextErr, io.EOF) {
				break
			}
			if nextErr != nil {
				return "", "", nil, nextErr
			}
			partPlain, partHTML, partAttachments, parseErr := ParseEntity(mail.Header(part.Header), part)
			_ = part.Close()
			if parseErr != nil {
				return "", "", nil, parseErr
			}
			if plain == "" {
				plain = partPlain
			}
			if html == "" {
				html = partHTML
			}
			attachments = append(attachments, partAttachments...)
		}
		return plain, html, attachments, nil
	}
	decoded := decodeTransfer(header.Get("Content-Transfer-Encoding"), body)
	data, err := io.ReadAll(decoded)
	if err != nil {
		return "", "", nil, err
	}
	filename := mailtext.DecodeHeader(dispositionParams["filename"])
	if filename == "" {
		filename = mailtext.DecodeHeader(params["name"])
	}
	if strings.EqualFold(disposition, "attachment") || strings.EqualFold(disposition, "inline") || filename != "" || contentID != "" {
		if filename == "" {
			filename = "attachment"
		}
		return "", "", []IncomingAttachment{{Filename: filepath.Base(filename), MimeType: mediaType, Size: int64(len(data)), ContentID: contentID, Disposition: strings.ToLower(disposition), Content: bytes.NewReader(data)}}, nil
	}
	if strings.EqualFold(mediaType, "text/html") {
		return "", mailtext.DecodeBody(data, params["charset"]), nil, nil
	}
	return mailtext.DecodeBody(data, params["charset"]), "", nil, nil
}

func decodeTransfer(encoding string, body io.Reader) io.Reader {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, body)
	case "quoted-printable":
		return quotedprintable.NewReader(body)
	default:
		return body
	}
}
