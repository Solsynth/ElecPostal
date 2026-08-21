// Package pop3 provides a mailbox-scoped POP3 server backed by ElecPostal's
// canonical RFC 5322 message sources.
package pop3

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"sync"

	"src.solsynth.dev/sosys/elecpostal/internal/config"
	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/mailmime"
	"src.solsynth.dev/sosys/elecpostal/internal/service"
)

type Backend interface {
	AuthenticateMailProtocolAddress(context.Context, string, string, string) (*service.ProtocolPrincipal, error)
	ListProtocolFolder(context.Context, string, string) ([]service.ProtocolMessage, *database.MailFolder, error)
	OpenProtocolMessage(context.Context, string) (mailmime.MessageSource, error)
	MoveProtocolMessages(context.Context, string, string, string, []string) error
}

// Server implements the POP3 transaction model.  Deletions are committed only
// by QUIT and are moved into Trash so the HTTP recovery policy is retained.
type Server struct {
	cfg     config.ListenerConfig
	backend Backend
	tls     *tls.Config
	ln      net.Listener
	mu      sync.Mutex
	closed  bool
	wg      sync.WaitGroup
}

func New(cfg config.ListenerConfig, backend Backend) (*Server, error) {
	if backend == nil {
		return nil, fmt.Errorf("POP3 backend is required")
	}
	s := &Server{cfg: cfg, backend: backend}
	if cfg.Enabled && !strings.EqualFold(cfg.TLSMode, "disabled") {
		if cfg.CertFile == "" || cfg.KeyFile == "" {
			return nil, fmt.Errorf("POP3 implicit TLS requires cert and key")
		}
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, err
		}
		s.tls = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}
	return s, nil
}
func (s *Server) Start() error {
	if !s.cfg.Enabled {
		return nil
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(s.cfg.Host, s.cfg.Port))
	if err != nil {
		return err
	}
	s.ln = ln
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() { defer s.wg.Done(); s.serve(c) }()
		}
	}()
	return nil
}
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	ln := s.ln
	s.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	s.wg.Wait()
	return nil
}

func (s *Server) serve(raw net.Conn) {
	conn := raw
	secure := false
	if s.tls != nil && strings.EqualFold(s.cfg.TLSMode, "implicit") {
		conn = tls.Server(raw, s.tls)
		if err := conn.(*tls.Conn).Handshake(); err != nil {
			_ = raw.Close()
			return
		}
		secure = true
	}
	// STLS replaces conn during the session; close the active wrapper so clients
	// receive the TLS close_notify alert on a normal QUIT.
	defer func() { _ = conn.Close() }()
	r, w := bufio.NewReader(conn), bufio.NewWriter(conn)
	reply(w, "+OK ElecPostal POP3 ready")
	var user string
	var principal *service.ProtocolPrincipal
	var messages []service.ProtocolMessage
	deleted := map[int]bool{}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			reply(w, "-ERR empty command")
			continue
		}
		cmd := strings.ToUpper(fields[0])
		arg := ""
		if len(fields) > 1 {
			arg = strings.Join(fields[1:], " ")
		}
		if principal == nil {
			switch cmd {
			case "STLS":
				if s.tls == nil || !strings.EqualFold(s.cfg.TLSMode, "starttls") || secure {
					reply(w, "-ERR STLS unavailable")
					continue
				}
				reply(w, "+OK Begin TLS negotiation")
				tlsConn := tls.Server(conn, s.tls)
				if err := tlsConn.Handshake(); err != nil {
					return
				}
				conn = tlsConn
				r, w = bufio.NewReader(conn), bufio.NewWriter(conn)
				secure = true
			case "CAPA":
				multi(w, []string{"+OK Capability list follows", "USER", "SASL PLAIN", "UIDL", "TOP", "."})
			case "USER":
				user = arg
				reply(w, "+OK user accepted")
			case "PASS":
				if s.tls != nil && !secure {
					reply(w, "-ERR STLS required")
					continue
				}
				p, e := s.backend.AuthenticateMailProtocolAddress(context.Background(), user, arg, "pop3")
				if e != nil {
					reply(w, "-ERR authentication failed")
					continue
				}
				principal = p
				messages, _, e = s.backend.ListProtocolFolder(context.Background(), p.MailboxID, "INBOX")
				if e != nil {
					reply(w, "-ERR mailbox unavailable")
					principal = nil
					continue
				}
				reply(w, "+OK mailbox locked and ready")
			case "QUIT":
				reply(w, "+OK bye")
				return
			default:
				reply(w, "-ERR authenticate first")
			}
			continue
		}
		switch cmd {
		case "STAT":
			count, size := 0, int64(0)
			for i, m := range messages {
				if !deleted[i] {
					count++
					size += m.RFC822Size
				}
			}
			reply(w, fmt.Sprintf("+OK %d %d", count, size))
		case "LIST":
			lines := []string{"+OK scan listing follows"}
			for i, m := range messages {
				if !deleted[i] {
					lines = append(lines, fmt.Sprintf("%d %d", i+1, m.RFC822Size))
				}
			}
			lines = append(lines, ".")
			multi(w, lines)
		case "UIDL":
			lines := []string{"+OK unique-id listing follows"}
			for i, m := range messages {
				if !deleted[i] {
					lines = append(lines, fmt.Sprintf("%d %d", i+1, m.UID))
				}
			}
			lines = append(lines, ".")
			multi(w, lines)
		case "RETR", "TOP":
			parts := strings.Fields(arg)
			if len(parts) == 0 {
				reply(w, "-ERR message number required")
				continue
			}
			n, _ := strconv.Atoi(parts[0])
			if n < 1 || n > len(messages) || deleted[n-1] {
				reply(w, "-ERR no such message")
				continue
			}
			source, err := s.backend.OpenProtocolMessage(context.Background(), messages[n-1].EmailID)
			if err != nil {
				reply(w, "-ERR message source unavailable")
				continue
			}
			raw, err := mailmime.RenderBytes(context.Background(), source)
			if err != nil {
				reply(w, "-ERR message source unavailable")
				continue
			}
			if cmd == "TOP" && len(parts) > 1 {
				raw = top(raw, parts[1])
			}
			multi(w, append([]string{fmt.Sprintf("+OK %d octets", len(raw))}, dotStuff(raw)...))
		case "DELE":
			n, _ := strconv.Atoi(arg)
			if n < 1 || n > len(messages) || deleted[n-1] {
				reply(w, "-ERR no such message")
				continue
			}
			deleted[n-1] = true
			reply(w, "+OK marked for deletion")
		case "RSET":
			deleted = map[int]bool{}
			reply(w, "+OK reset")
		case "NOOP":
			reply(w, "+OK")
		case "QUIT":
			ids := []string{}
			for i, m := range messages {
				if deleted[i] {
					ids = append(ids, m.EmailID)
				}
			}
			if len(ids) > 0 {
				_ = s.backend.MoveProtocolMessages(context.Background(), principal.MailboxID, "INBOX", "Trash", ids)
			}
			reply(w, "+OK bye")
			return
		default:
			reply(w, "-ERR unsupported command")
		}
	}
}
func reply(w *bufio.Writer, s string) { _, _ = w.WriteString(s + "\r\n"); _ = w.Flush() }
func multi(w *bufio.Writer, lines []string) {
	for _, line := range lines {
		reply(w, line)
	}
}
func dotStuff(raw []byte) []string {
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	for i := range lines {
		if strings.HasPrefix(lines[i], ".") {
			lines[i] = "." + lines[i]
		}
	}
	return append(lines, ".")
}
func top(raw []byte, lines string) []byte {
	n, _ := strconv.Atoi(lines)
	if n < 0 {
		n = 0
	}
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return raw
	}
	body := topText(message.Header, message.Body)
	bodyLines := strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n")
	if len(bodyLines) > n {
		bodyLines = bodyLines[:n]
	}
	var output bytes.Buffer
	for key, values := range message.Header {
		for _, value := range values {
			fmt.Fprintf(&output, "%s: %s\r\n", key, value)
		}
	}
	output.WriteString("\r\n")
	output.WriteString(strings.Join(bodyLines, "\r\n"))
	return output.Bytes()
}

func topText(header mail.Header, body io.Reader) []byte {
	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err == nil && strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		reader := multipart.NewReader(body, params["boundary"])
		for {
			part, nextErr := reader.NextPart()
			if nextErr != nil {
				break
			}
			disposition, _, _ := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
			if strings.EqualFold(disposition, "attachment") || (part.Header.Get("Content-ID") != "" && !strings.HasPrefix(strings.ToLower(part.Header.Get("Content-Type")), "text/")) {
				_, _ = io.Copy(io.Discard, part)
				_ = part.Close()
				continue
			}
			text := topText(mail.Header(part.Header), part)
			_ = part.Close()
			if len(text) > 0 {
				return text
			}
		}
		return nil
	}
	if !strings.HasPrefix(strings.ToLower(mediaType), "text/") {
		return nil
	}
	var reader io.Reader = body
	switch strings.ToLower(strings.TrimSpace(header.Get("Content-Transfer-Encoding"))) {
	case "base64":
		reader = base64.NewDecoder(base64.StdEncoding, body)
	case "quoted-printable":
		reader = quotedprintable.NewReader(body)
	}
	data, _ := io.ReadAll(reader)
	return data
}
