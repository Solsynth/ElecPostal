// Package smtp implements the network-facing inbound SMTP listener.
package smtp

import (
	"bufio"
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

	"github.com/google/uuid"

	"src.solsynth.dev/sosys/elecpostal/internal/config"
	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/logging"
	"src.solsynth.dev/sosys/elecpostal/internal/service"
)

const (
	defaultMaxMessageBytes int64 = 25 * 1024 * 1024
	defaultMaxRecipients         = 100
)

// Server owns an SMTP listener and all active connections.
type Server struct {
	cfg      config.ListenerConfig
	domain   string
	service  Backend
	delivery DeliveryQueue
	ln       net.Listener
	tls      *tls.Config
	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	closed   bool
	wg       sync.WaitGroup
}

// DeliveryQueue accepts a fully parsed SMTP message once it is safe for the
// SMTP client to receive a 250 response.
type DeliveryQueue interface {
	Enqueue(context.Context, deliveryJob) error
}

// Backend is the narrow mail-domain surface used by SMTP. Keeping the
// protocol server behind this interface makes its wire behavior testable
// without a database or external storage service.
type Backend interface {
	ResolveLocalMailbox(context.Context, string) (*database.Mailbox, error)
	AuthenticateMailProtocolAddress(context.Context, string, string, string) (*service.ProtocolPrincipal, error)
	ReceiveEmail(context.Context, service.ReceiveEmailInput) (*database.Email, error)
}

func New(cfg config.ListenerConfig, domain string, emailService Backend) (*Server, error) {
	if emailService == nil {
		return nil, fmt.Errorf("email service is required")
	}
	mode := strings.ToLower(strings.TrimSpace(cfg.TLSMode))
	if mode != "" && mode != "disabled" && mode != "starttls" && mode != "implicit" {
		return nil, fmt.Errorf("unsupported SMTP TLS mode %q", cfg.TLSMode)
	}
	s := &Server{cfg: cfg, domain: strings.ToLower(strings.TrimSpace(domain)), service: emailService, delivery: inlineDelivery{backend: emailService}, conns: make(map[net.Conn]struct{})}
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
	return s, nil
}

// SetDeliveryQueue replaces direct delivery with a durable queue. It must be
// called before Start.
func (s *Server) SetDeliveryQueue(queue DeliveryQueue) {
	if queue != nil {
		s.delivery = queue
	}
}

func (s *Server) Start() error {
	if !s.cfg.Enabled {
		return nil
	}
	host := strings.TrimSpace(s.cfg.Host)
	ln, err := net.Listen("tcp", net.JoinHostPort(host, s.cfg.Port))
	if err != nil {
		return fmt.Errorf("listen SMTP: %w", err)
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				if s.isClosed() || errors.Is(err, net.ErrClosed) {
					return
				}
				logging.Log.Warn().Err(err).Msg("SMTP accept failed")
				continue
			}
			s.wg.Add(1)
			go func() { defer s.wg.Done(); s.serve(conn) }()
		}
	}()
	logging.Log.Info().Str("address", ln.Addr().String()).Str("tls_mode", s.cfg.TLSMode).Msg("SMTP server started")
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

func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	ln := s.ln
	for conn := range s.conns {
		_ = conn.Close()
	}
	s.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	s.wg.Wait()
	return nil
}

func (s *Server) isClosed() bool        { s.mu.Lock(); defer s.mu.Unlock(); return s.closed }
func (s *Server) addConn(c net.Conn)    { s.mu.Lock(); s.conns[c] = struct{}{}; s.mu.Unlock() }
func (s *Server) removeConn(c net.Conn) { s.mu.Lock(); delete(s.conns, c); s.mu.Unlock() }

type session struct {
	authenticated bool
	principal     *service.ProtocolPrincipal
	from          string
	recipients    []recipient
}
type recipient struct {
	address   string
	mailboxID string
}

func (s *Server) serve(raw net.Conn) {
	conn := raw
	s.addConn(raw)
	if strings.EqualFold(s.cfg.TLSMode, "implicit") {
		conn = tls.Server(raw, s.tls)
		if err := conn.(*tls.Conn).Handshake(); err != nil {
			s.removeConn(raw)
			_ = raw.Close()
			return
		}
		s.removeConn(raw)
		s.addConn(conn)
	}
	defer func() { s.removeConn(conn); _ = conn.Close() }()
	remote := conn.RemoteAddr().String()
	logging.Log.Info().Str("remote", remote).Msg("SMTP connection")
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	writeReply(w, "220 ElecPostal ESMTP ready")
	state := session{}
	for {
		_ = conn.SetDeadline(time.Now().Add(5 * time.Minute))
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb, arg := splitCommand(line)
		switch verb {
		case "EHLO", "HELO":
			state.from, state.recipients = "", nil
			s.writeGreeting(w, state.authenticated)
		case "NOOP":
			writeReply(w, "250 2.0.0 OK")
		case "RSET":
			state.from, state.recipients = "", nil
			writeReply(w, "250 2.0.0 OK")
		case "QUIT":
			writeReply(w, "221 2.0.0 Bye")
			return
		case "STARTTLS":
			if s.tls == nil || !strings.EqualFold(s.cfg.TLSMode, "starttls") {
				writeReply(w, "454 4.7.0 TLS not available")
				continue
			}
			writeReply(w, "220 2.0.0 Ready to start TLS")
			tlsConn := tls.Server(conn, s.tls)
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			s.removeConn(conn)
			conn = tlsConn
			s.addConn(conn)
			r = bufio.NewReader(conn)
			w = bufio.NewWriter(conn)
			state = session{}
		case "AUTH":
			if err := s.authenticate(r, w, arg, &state); err != nil {
				logging.Log.Warn().Err(err).Str("remote", remote).Msg("SMTP authentication failed")
			}
		case "MAIL":
			if s.requiresAuthentication() && !state.authenticated {
				writeReply(w, "530 5.7.0 Authentication required")
				continue
			}
			from, err := envelopeAddress(arg, "FROM:", true)
			if err != nil {
				writeReply(w, "501 5.5.4 Invalid MAIL FROM")
				continue
			}
			// Authenticated submission is address-bound.  Keep the empty reverse
			// path available for delivery-status notifications.
			if state.principal != nil && state.principal.Address != "" && from != "<>" && !strings.EqualFold(from, state.principal.Address) {
				writeReply(w, "553 5.7.1 Sender address does not match authenticated mailbox")
				continue
			}
			state.from, state.recipients = from, nil
			writeReply(w, "250 2.1.0 OK")
		case "RCPT":
			if s.requiresAuthentication() && !state.authenticated {
				writeReply(w, "530 5.7.0 Authentication required")
				continue
			}
			if state.from == "" {
				writeReply(w, "503 5.5.1 MAIL FROM required")
				continue
			}
			if len(state.recipients) >= s.maxRecipients() {
				writeReply(w, "452 4.5.3 Too many recipients")
				continue
			}
			address, err := envelopeAddress(arg, "TO:", false)
			if err != nil {
				writeReply(w, "501 5.5.4 Invalid RCPT TO")
				continue
			}
			mailbox, err := s.service.ResolveLocalMailbox(context.Background(), address)
			if err != nil {
				writeReply(w, "550 5.1.1 User unknown")
				logging.Log.Info().Str("recipient", address).Str("remote", remote).Msg("SMTP recipient rejected")
				continue
			}
			state.recipients = append(state.recipients, recipient{address: address, mailboxID: mailbox.ID})
			writeReply(w, "250 2.1.5 OK")
			logging.Log.Info().Str("recipient", address).Str("mailbox_id", mailbox.ID).Msg("SMTP recipient accepted")
		case "DATA":
			if state.from == "" || len(state.recipients) == 0 {
				writeReply(w, "503 5.5.1 MAIL FROM and RCPT TO required")
				continue
			}
			writeReply(w, "354 End data with <CR><LF>.<CR><LF>")
			rawMessage, tooLarge, err := readData(r, s.maxMessageBytes())
			if err != nil {
				return
			}
			if tooLarge {
				writeReply(w, "552 5.3.4 Message too large")
				state.from, state.recipients = "", nil
				continue
			}
			message, err := parseMessage(rawMessage, state.from, state.recipients)
			if err != nil {
				writeReply(w, "451 4.3.0 Unable to process message")
				continue
			}
			if err := s.deliver(message, rawMessage, state.from, state.recipients); err != nil {
				logging.Log.Error().Err(err).Str("smtp_message_id", message.id).Msg("SMTP delivery failed")
				writeReply(w, "451 4.3.0 Temporary delivery failure")
			} else {
				logging.Log.Info().Str("smtp_message_id", message.id).Int("recipient_count", len(state.recipients)).Msg("SMTP message accepted")
				writeReply(w, "250 2.0.0 Message accepted")
			}
			state.from, state.recipients = "", nil
		default:
			writeReply(w, "502 5.5.1 Command not implemented")
		}
	}
}

func (s *Server) requiresAuthentication() bool {
	return s.cfg.RequireAuth || strings.TrimSpace(s.cfg.Port) == "587"
}
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
func (s *Server) writeGreeting(w *bufio.Writer, authenticated bool) {
	writeReply(w, "250-ElecPostal")
	if s.tls != nil && strings.EqualFold(s.cfg.TLSMode, "starttls") {
		writeReply(w, "250-STARTTLS")
	}
	if !authenticated {
		writeReply(w, "250-AUTH PLAIN LOGIN")
	}
	writeReply(w, "250 SIZE "+fmt.Sprint(s.maxMessageBytes()))
}
func writeReply(w *bufio.Writer, value string) { _, _ = w.WriteString(value + "\r\n"); _ = w.Flush() }
func splitCommand(line string) (string, string) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", ""
	}
	return strings.ToUpper(fields[0]), strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
}

func (s *Server) authenticate(r *bufio.Reader, w *bufio.Writer, arg string, state *session) error {
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		writeReply(w, "501 5.5.4 AUTH mechanism required")
		return errors.New("missing mechanism")
	}
	mechanism := strings.ToUpper(fields[0])
	var username, secret string
	switch mechanism {
	case "PLAIN":
		if len(fields) < 2 {
			writeReply(w, "334 ")
			line, err := r.ReadString('\n')
			if err != nil {
				return err
			}
			fields = append(fields, strings.TrimSpace(line))
		}
		decoded, err := base64.StdEncoding.DecodeString(fields[1])
		if err != nil {
			writeReply(w, "535 5.7.8 Authentication credentials invalid")
			return err
		}
		pieces := strings.Split(string(decoded), "\x00")
		if len(pieces) != 3 {
			writeReply(w, "535 5.7.8 Authentication credentials invalid")
			return errors.New("invalid plain auth")
		}
		username, secret = pieces[1], pieces[2]
	case "LOGIN":
		writeReply(w, "334 VXNlcm5hbWU6")
		encodedUser, err := r.ReadString('\n')
		if err != nil {
			return err
		}
		user, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encodedUser))
		if err != nil {
			writeReply(w, "535 5.7.8 Authentication credentials invalid")
			return err
		}
		username = string(user)
		writeReply(w, "334 UGFzc3dvcmQ6")
		encodedSecret, err := r.ReadString('\n')
		if err != nil {
			return err
		}
		password, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encodedSecret))
		if err != nil {
			writeReply(w, "535 5.7.8 Authentication credentials invalid")
			return err
		}
		secret = string(password)
	default:
		writeReply(w, "504 5.5.4 Unsupported authentication mechanism")
		return errors.New("unsupported auth")
	}
	principal, err := s.service.AuthenticateMailProtocolAddress(context.Background(), username, secret, "smtp")
	if err != nil {
		writeReply(w, "535 5.7.8 Authentication credentials invalid")
		return err
	}
	state.authenticated = true
	state.principal = principal
	writeReply(w, "235 2.7.0 Authentication successful")
	return nil
}

func envelopeAddress(arg, prefix string, permitEmpty bool) (string, error) {
	if !strings.HasPrefix(strings.ToUpper(arg), prefix) {
		return "", errors.New("missing prefix")
	}
	value := strings.TrimSpace(arg[len(prefix):])
	if !strings.HasPrefix(value, "<") {
		return "", errors.New("invalid path")
	}
	end := strings.IndexByte(value, '>')
	if end < 0 {
		return "", errors.New("invalid path")
	}
	// RFC 5321 permits ESMTP parameters after the reverse/forward path (for
	// example, Outlook sends "SIZE=1234" with MAIL FROM). This server does
	// not use those parameters yet, but must accept a whitespace-separated list.
	if rest := value[end+1:]; rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return "", errors.New("invalid path parameters")
	}
	value = strings.TrimSpace(value[1:end])
	if value == "" && permitEmpty {
		return "<>", nil
	}
	parsed, err := mail.ParseAddress(value)
	if err != nil {
		return "", err
	}
	return strings.ToLower(parsed.Address), nil
}

func readData(r *bufio.Reader, limit int64) ([]byte, bool, error) {
	var result bytes.Buffer
	tooLarge := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, false, err
		}
		if line == ".\r\n" || line == ".\n" {
			return result.Bytes(), tooLarge, nil
		}
		if strings.HasPrefix(line, "..") {
			line = line[1:]
		}
		if int64(result.Len()+len(line)) > limit {
			tooLarge = true
			continue
		}
		if !tooLarge {
			_, _ = result.WriteString(line)
		}
	}
}

type parsedMessage struct {
	id, fromAddress, fromName, subject, body, contentType string
	to, cc                                                []service.RecipientInput
	attachments                                           []storedAttachment
}
type storedAttachment struct {
	filename, mimeType string
	content            []byte
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
	result.subject = decodeHeader(m.Header.Get("Subject"))
	result.to = addresses(m.Header, "To", "to")
	result.cc = addresses(m.Header, "Cc", "cc")
	if len(result.to) == 0 {
		for _, recipient := range envelopeRecipients {
			result.to = append(result.to, service.RecipientInput{Address: recipient.address, Kind: "to"})
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
	contentType := header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = "text/plain"
	}
	disposition, dparams, _ := mime.ParseMediaType(header.Get("Content-Disposition"))
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
	if strings.EqualFold(disposition, "attachment") || filename != "" {
		if filename == "" {
			filename = "attachment"
		}
		return "", "", []storedAttachment{{filename: filepath.Base(filename), mimeType: mediaType, content: data}}, nil
	}
	if strings.EqualFold(mediaType, "text/html") {
		return string(data), "", nil, nil
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

func (s *Server) deliver(message parsedMessage, raw []byte, envelopeFrom string, recipients []recipient) error {
	return s.delivery.Enqueue(context.Background(), newDeliveryJob(message, raw, envelopeFrom, recipients))
}
