// Package pop3 provides a mailbox-scoped POP3 server backed by ElecPostal's
// canonical RFC 5322 message sources.
package pop3

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	"src.solsynth.dev/sosys/elecpostal/internal/config"
	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/service"
)

type Backend interface {
	AuthenticateMailProtocolAddress(context.Context, string, string, string) (*service.ProtocolPrincipal, error)
	ListProtocolFolder(context.Context, string, string) ([]service.ProtocolMessage, *database.MailFolder, error)
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
	defer conn.Close()
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
			count, size := 0, 0
			for i, m := range messages {
				if !deleted[i] {
					count++
					size += len(m.Raw)
				}
			}
			reply(w, fmt.Sprintf("+OK %d %d", count, size))
		case "LIST":
			lines := []string{"+OK scan listing follows"}
			for i, m := range messages {
				if !deleted[i] {
					lines = append(lines, fmt.Sprintf("%d %d", i+1, len(m.Raw)))
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
			raw := messages[n-1].Raw
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
	split := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	end := 0
	for end < len(split) && split[end] != "" {
		end++
	}
	end = min(len(split), end+1+n)
	return []byte(strings.Join(split[:end], "\r\n"))
}
