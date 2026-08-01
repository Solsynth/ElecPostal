// Package smtp exposes ElecPostal's inbound mail domain through go-smtp.
package smtp

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gosasl "github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"
	"github.com/google/uuid"

	"src.solsynth.dev/sosys/elecpostal/internal/config"
	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/relay"
	"src.solsynth.dev/sosys/elecpostal/internal/service"
)

const (
	defaultMaxMessageBytes int64 = 25 * 1024 * 1024
	defaultMaxRecipients         = 100
)

type DeliveryQueue interface {
	Enqueue(context.Context, deliveryJob) error
}
type Backend interface {
	ResolveLocalMailbox(context.Context, string) (*database.Mailbox, error)
	IsMailboxSender(context.Context, string, string) (bool, error)
	AuthenticateMailProtocolAddress(context.Context, string, string, string) (*service.ProtocolPrincipal, error)
	ReceiveEmail(context.Context, service.ReceiveEmailInput) (*database.Email, error)
	SendOutbound(context.Context, relay.Message) error
}

// Server owns the library server and its listener. Protocol parsing, ESMTP
// extensions, TLS upgrades, and command sequencing are provided by go-smtp.
type Server struct {
	cfg      config.ListenerConfig
	domain   string
	service  Backend
	delivery DeliveryQueue
	tls      *tls.Config
	server   *gosmtp.Server
	ln       net.Listener
	mu       sync.Mutex
	wg       sync.WaitGroup
}

func New(cfg config.ListenerConfig, domain string, backend Backend) (*Server, error) {
	if backend == nil {
		return nil, fmt.Errorf("email service is required")
	}
	mode := strings.ToLower(strings.TrimSpace(cfg.TLSMode))
	if mode != "" && mode != "disabled" && mode != "starttls" && mode != "implicit" {
		return nil, fmt.Errorf("unsupported SMTP TLS mode %q", cfg.TLSMode)
	}
	s := &Server{cfg: cfg, domain: strings.ToLower(strings.TrimSpace(domain)), service: backend, delivery: inlineDelivery{backend: backend}}
	if cfg.Enabled && (mode == "starttls" || mode == "implicit") {
		if cfg.CertFile == "" || cfg.KeyFile == "" {
			return nil, fmt.Errorf("SMTP %s TLS requires cert and key files", mode)
		}
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load SMTP TLS certificate: %w", err)
		}
		s.tls = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}
	s.server = s.newLibraryServer()
	return s, nil
}

func (s *Server) newLibraryServer() *gosmtp.Server {
	server := gosmtp.NewServer(s)
	server.Domain = s.domain
	server.MaxMessageBytes = s.maxMessageBytes()
	server.MaxRecipients = s.maxRecipients()
	server.ReadTimeout, server.WriteTimeout = 5*time.Minute, 5*time.Minute
	server.AllowInsecureAuth = s.tls == nil
	if strings.EqualFold(s.cfg.TLSMode, "starttls") {
		server.TLSConfig = s.tls
	}
	return server
}
func (s *Server) SetDeliveryQueue(q DeliveryQueue) {
	if q != nil {
		s.delivery = q
	}
}
func (s *Server) Start() error {
	if !s.cfg.Enabled {
		return nil
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(strings.TrimSpace(s.cfg.Host), s.cfg.Port))
	if err != nil {
		return fmt.Errorf("listen SMTP: %w", err)
	}
	if strings.EqualFold(s.cfg.TLSMode, "implicit") {
		ln = tls.NewListener(ln, s.tls)
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	s.wg.Add(1)
	go func() { defer s.wg.Done(); _ = s.server.Serve(ln) }()
	return nil
}
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}
func (s *Server) Close() error { _ = s.server.Close(); s.wg.Wait(); return nil }
func (s *Server) maxRecipients() int {
	if s.cfg.MaxRecipients > 0 {
		return s.cfg.MaxRecipients
	}
	return defaultMaxRecipients
}
func (s *Server) maxMessageBytes() int64 {
	if s.cfg.MaxMessageBytes > 0 {
		return s.cfg.MaxMessageBytes
	}
	return defaultMaxMessageBytes
}

// NewSession implements go-smtp.Backend.
func (s *Server) NewSession(_ *gosmtp.Conn) (gosmtp.Session, error) {
	return &smtpSession{server: s}, nil
}

type smtpSession struct {
	server        *Server
	authenticated bool
	principal     *service.ProtocolPrincipal
	from          string
	recipients    []recipient
}
type recipient struct{ address, mailboxID string }

func (ss *smtpSession) Reset()                   { ss.from, ss.recipients = "", nil }
func (ss *smtpSession) Logout() error            { return nil }
func (ss *smtpSession) AuthMechanisms() []string { return []string{gosasl.Plain} }
func (ss *smtpSession) Auth(mech string) (gosasl.Server, error) {
	if !strings.EqualFold(mech, gosasl.Plain) {
		return nil, gosmtp.ErrAuthUnknownMechanism
	}
	return gosasl.NewPlainServer(func(identity, username, password string) error {
		if identity != "" && identity != username {
			return gosmtp.ErrAuthFailed
		}
		p, err := ss.server.service.AuthenticateMailProtocolAddress(context.Background(), username, password, "smtp")
		if err != nil {
			return gosmtp.ErrAuthFailed
		}
		ss.authenticated, ss.principal = true, p
		return nil
	}), nil
}
func (ss *smtpSession) Mail(from string, _ *gosmtp.MailOptions) error {
	if ss.server.requiresAuthentication() && !ss.authenticated {
		return &gosmtp.SMTPError{Code: 530, EnhancedCode: gosmtp.EnhancedCode{5, 7, 0}, Message: "Authentication required"}
	}
	if ss.principal != nil && ss.principal.MailboxID != "" && from != "" {
		allowed, err := ss.server.service.IsMailboxSender(context.Background(), ss.principal.MailboxID, from)
		if err != nil {
			return &gosmtp.SMTPError{Code: 451, EnhancedCode: gosmtp.EnhancedCode{4, 3, 0}, Message: "Temporary sender lookup failure"}
		}
		if !allowed {
			return &gosmtp.SMTPError{Code: 553, EnhancedCode: gosmtp.EnhancedCode{5, 7, 1}, Message: "Sender address does not match authenticated mailbox"}
		}
	}
	ss.from, ss.recipients = strings.ToLower(from), nil
	return nil
}
func (ss *smtpSession) Rcpt(to string, _ *gosmtp.RcptOptions) error {
	address := strings.ToLower(strings.TrimSpace(to))
	box, err := ss.server.service.ResolveLocalMailbox(context.Background(), address)
	if err == nil {
		ss.recipients = append(ss.recipients, recipient{address: address, mailboxID: box.ID})
		return nil
	}
	if !errors.Is(err, service.ErrNotFound) {
		return &gosmtp.SMTPError{Code: 451, EnhancedCode: gosmtp.EnhancedCode{4, 3, 0}, Message: "Temporary recipient lookup failure"}
	}
	// Relay is restricted to authenticated submission sessions so the MX port
	// never becomes an open relay for unauthenticated senders.
	if ss.principal == nil || ss.principal.MailboxID == "" {
		return &gosmtp.SMTPError{Code: 550, EnhancedCode: gosmtp.EnhancedCode{5, 1, 1}, Message: "User unknown"}
	}
	ss.recipients = append(ss.recipients, recipient{address: address, mailboxID: ""})
	return nil
}
func (ss *smtpSession) Data(r io.Reader) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	message, err := parseMessage(raw, ss.from, ss.recipients)
	if err != nil {
		return err
	}
	if ss.principal != nil && ss.principal.MailboxID != "" {
		if err := ss.submit(message, raw); err != nil {
			return err
		}
		ss.Reset()
		return nil
	}
	if err := ss.server.delivery.Enqueue(context.Background(), newDeliveryJob(message, raw, ss.from, ss.recipients)); err != nil {
		return err
	}
	ss.Reset()
	return nil
}

// submit handles SMTP submission from an authenticated mailbox. Local
// recipients are stored in their INBOX while external recipients are relayed
// through the configured provider. The client keeps its own Sent copy via IMAP
// APPEND, so no duplicate is created here.
func (ss *smtpSession) submit(message parsedMessage, raw []byte) error {
	local := make([]recipient, 0, len(ss.recipients))
	var external relay.Message
	external.FromAddress, external.FromName = message.fromAddress, message.fromName
	external.Subject, external.Body, external.ContentType = message.subject, message.body, message.contentType
	for _, r := range ss.recipients {
		if r.mailboxID != "" {
			local = append(local, r)
			continue
		}
		address := r.address
		switch classifyRecipient(address, message.to, message.cc) {
		case "cc":
			external.Cc = append(external.Cc, address)
		case "bcc":
			external.Bcc = append(external.Bcc, address)
		default:
			external.To = append(external.To, address)
		}
	}
	if len(local) > 0 {
		if err := deliverJob(context.Background(), ss.server.service, newDeliveryJob(message, raw, ss.from, local)); err != nil {
			return err
		}
	}
	if len(external.To)+len(external.Cc)+len(external.Bcc) == 0 {
		return nil
	}
	if err := ss.server.service.SendOutbound(context.Background(), external); err != nil {
		return submissionError(err)
	}
	return nil
}

// classifyRecipient assigns an envelope recipient to the header section it
// appears in. Envelope recipients that are absent from both To and Cc headers
// are Bcc recipients.
func classifyRecipient(address string, to, cc []service.RecipientInput) string {
	address = strings.ToLower(strings.TrimSpace(address))
	for _, r := range to {
		if strings.ToLower(strings.TrimSpace(r.Address)) == address {
			return "to"
		}
	}
	for _, r := range cc {
		if strings.ToLower(strings.TrimSpace(r.Address)) == address {
			return "cc"
		}
	}
	return "bcc"
}

func submissionError(err error) error {
	switch {
	case errors.Is(err, service.ErrOutboundRelayUnavailable):
		return &gosmtp.SMTPError{Code: 554, EnhancedCode: gosmtp.EnhancedCode{5, 3, 0}, Message: "Outbound relay is not configured"}
	case errors.Is(err, service.ErrSendLimitExceeded):
		return &gosmtp.SMTPError{Code: 452, EnhancedCode: gosmtp.EnhancedCode{4, 3, 2}, Message: "Outbound send limit exceeded"}
	case errors.Is(err, relay.ErrAttachmentSourceRequired):
		return &gosmtp.SMTPError{Code: 554, EnhancedCode: gosmtp.EnhancedCode{5, 6, 0}, Message: "Message attachments are not supported"}
	default:
		return &gosmtp.SMTPError{Code: 554, EnhancedCode: gosmtp.EnhancedCode{5, 3, 0}, Message: "Message delivery failed"}
	}
}
func (s *Server) requiresAuthentication() bool {
	return s.cfg.RequireAuth || strings.TrimSpace(s.cfg.Port) == "587"
}

// serve retains the package-level test hook while delegating all wire handling
// to go-smtp through a single-connection listener.
func (s *Server) serve(conn net.Conn) {
	ln := &singleConnListener{conn: conn, addr: conn.LocalAddr()}
	if strings.EqualFold(s.cfg.TLSMode, "implicit") {
		ln.conn = tls.Server(conn, s.tls)
	}
	_ = s.newLibraryServer().Serve(ln)
}

type singleConnListener struct {
	conn net.Conn
	addr net.Addr
	used bool
	mu   sync.Mutex
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.used {
		return nil, net.ErrClosed
	}
	l.used = true
	return l.conn, nil
}
func (l *singleConnListener) Close() error   { return nil }
func (l *singleConnListener) Addr() net.Addr { return l.addr }

type parsedMessage struct {
	id, fromAddress, fromName, subject, body, contentType string
	to, cc                                                []service.RecipientInput
	attachments                                           []storedAttachment
}
type storedAttachment struct {
	filename, mimeType, contentID, disposition string
	content                                    []byte
}

func parseMessage(raw []byte, envelopeFrom string, envelopeRecipients []recipient) (parsedMessage, error) {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return parsedMessage{}, err
	}
	result := parsedMessage{id: strings.TrimSpace(m.Header.Get("Message-ID")), fromAddress: envelopeFrom, contentType: "text/plain"}
	if result.id == "" {
		result.id = "<" + uuid.NewString() + "@elecpostal>"
	}
	if from, err := m.Header.AddressList("From"); err == nil && len(from) > 0 {
		result.fromAddress, result.fromName = strings.ToLower(from[0].Address), from[0].Name
	}
	result.subject, result.to, result.cc = decodeHeader(m.Header.Get("Subject")), addresses(m.Header, "To", "to"), addresses(m.Header, "Cc", "cc")
	if len(result.to) == 0 {
		for _, r := range envelopeRecipients {
			result.to = append(result.to, service.RecipientInput{Address: r.address, Kind: "to"})
		}
	}
	plain, html, attachments, err := parseEntity(m.Header, m.Body)
	if err != nil {
		return parsedMessage{}, err
	}
	if html != "" {
		result.body, result.contentType = html, "text/html"
	} else {
		result.body = plain
	}
	result.attachments = attachments
	return result, nil
}
func decodeHeader(v string) string {
	decoded, err := new(mime.WordDecoder).DecodeHeader(v)
	if err != nil {
		return v
	}
	return decoded
}
func addresses(h mail.Header, key, kind string) []service.RecipientInput {
	list, err := h.AddressList(key)
	if err != nil {
		return nil
	}
	result := make([]service.RecipientInput, 0, len(list))
	for _, a := range list {
		result = append(result, service.RecipientInput{Address: strings.ToLower(a.Address), Name: a.Name, Kind: kind})
	}
	return result
}
func parseEntity(header mail.Header, body io.Reader) (string, string, []storedAttachment, error) {
	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil {
		mediaType = "text/plain"
	}
	disposition, dparams, _ := mime.ParseMediaType(header.Get("Content-Disposition"))
	contentID := strings.Trim(header.Get("Content-ID"), "<> ")
	if strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		reader := multipart.NewReader(body, params["boundary"])
		var plain, html string
		var all []storedAttachment
		for {
			part, err := reader.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return "", "", nil, err
			}
			p, h, a, err := parseEntity(mail.Header(part.Header), part)
			_ = part.Close()
			if err != nil {
				return "", "", nil, err
			}
			if plain == "" {
				plain = p
			}
			if html == "" {
				html = h
			}
			all = append(all, a...)
		}
		return plain, html, all, nil
	}
	data, err := io.ReadAll(decodeTransfer(header.Get("Content-Transfer-Encoding"), body))
	if err != nil {
		return "", "", nil, err
	}
	filename := dparams["filename"]
	if filename == "" {
		filename = params["name"]
	}
	if strings.EqualFold(disposition, "attachment") || strings.EqualFold(disposition, "inline") || filename != "" || contentID != "" {
		if filename == "" {
			filename = "attachment"
		}
		return "", "", []storedAttachment{{filename: filepath.Base(filename), mimeType: mediaType, contentID: contentID, disposition: strings.ToLower(disposition), content: data}}, nil
	}
	if strings.EqualFold(mediaType, "text/html") {
		return "", string(data), nil, nil
	}
	return string(data), "", nil, nil
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
