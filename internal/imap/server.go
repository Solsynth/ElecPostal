// Package imap implements the protocol core used by common mail clients.  The
// parser is intentionally strict and keeps all state in the mailbox-scoped
// service backend.
package imap

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
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
type Server struct {
	cfg     config.ListenerConfig
	backend Backend
	tls     *tls.Config
	ln      net.Listener
	wg      sync.WaitGroup
}

func New(cfg config.ListenerConfig, backend Backend) (*Server, error) {
	if backend == nil {
		return nil, fmt.Errorf("IMAP backend is required")
	}
	s := &Server{cfg: cfg, backend: backend}
	if cfg.Enabled && !strings.EqualFold(cfg.TLSMode, "disabled") {
		if cfg.CertFile == "" || cfg.KeyFile == "" {
			return nil, fmt.Errorf("IMAP TLS requires cert and key")
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
	ln, e := net.Listen("tcp", net.JoinHostPort(s.cfg.Host, s.cfg.Port))
	if e != nil {
		return e
	}
	s.ln = ln
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, e := ln.Accept()
			if e != nil {
				return
			}
			s.wg.Add(1)
			go func() { defer s.wg.Done(); s.serve(c) }()
		}
	}()
	return nil
}
func (s *Server) Close() error {
	if s.ln != nil {
		_ = s.ln.Close()
	}
	s.wg.Wait()
	return nil
}

func (s *Server) serve(conn net.Conn) {
	secure := false
	if s.tls != nil && strings.EqualFold(s.cfg.TLSMode, "implicit") {
		tlsConn := tls.Server(conn, s.tls)
		if err := tlsConn.Handshake(); err != nil {
			return
		}
		conn = tlsConn
		secure = true
	}
	defer conn.Close()
	r, w := bufio.NewReader(conn), bufio.NewWriter(conn)
	out(w, "* OK ElecPostal IMAP4rev1 ready")
	var principal *service.ProtocolPrincipal
	selected := ""
	for {
		line, e := r.ReadString('\n')
		if e != nil {
			return
		}
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			out(w, "* BAD malformed command")
			continue
		}
		tag, cmd := fields[0], strings.ToUpper(fields[1])
		args := fields[2:]
		if principal == nil {
			switch cmd {
			case "STARTTLS":
				if s.tls == nil || !strings.EqualFold(s.cfg.TLSMode, "starttls") || secure {
					out(w, tag+" BAD STARTTLS unavailable")
					continue
				}
				out(w, tag+" OK Begin TLS negotiation")
				tlsConn := tls.Server(conn, s.tls)
				if err := tlsConn.Handshake(); err != nil {
					return
				}
				conn = tlsConn
				r, w = bufio.NewReader(conn), bufio.NewWriter(conn)
				secure = true
			case "CAPABILITY":
				out(w, "* CAPABILITY IMAP4rev1 UIDPLUS MOVE IDLE SEARCH CONDSTORE QRESYNC SPECIAL-USE NAMESPACE AUTH=PLAIN")
				out(w, tag+" OK CAPABILITY completed")
			case "LOGIN":
				if s.tls != nil && !secure {
					out(w, tag+" NO [PRIVACYREQUIRED] STARTTLS required")
					continue
				}
				if len(args) != 2 {
					out(w, tag+" BAD LOGIN requires username and password")
					continue
				}
				p, err := s.backend.AuthenticateMailProtocolAddress(context.Background(), unquote(args[0]), unquote(args[1]), "imap")
				if err != nil {
					out(w, tag+" NO authentication failed")
					continue
				}
				principal = p
				out(w, tag+" OK authenticated")
			case "AUTHENTICATE":
				if s.tls != nil && !secure {
					out(w, tag+" NO [PRIVACYREQUIRED] STARTTLS required")
					continue
				}
				if len(args) < 2 || strings.ToUpper(args[0]) != "PLAIN" {
					out(w, tag+" NO unsupported authentication mechanism")
					continue
				}
				decoded, err := base64.StdEncoding.DecodeString(args[1])
				parts := strings.Split(string(decoded), "\x00")
				if err != nil || len(parts) != 3 {
					out(w, tag+" NO authentication failed")
					continue
				}
				p, err := s.backend.AuthenticateMailProtocolAddress(context.Background(), parts[1], parts[2], "imap")
				if err != nil {
					out(w, tag+" NO authentication failed")
					continue
				}
				principal = p
				out(w, tag+" OK authenticated")
			case "LOGOUT":
				out(w, "* BYE logout")
				out(w, tag+" OK LOGOUT completed")
				return
			default:
				out(w, tag+" NO authenticate first")
			}
			continue
		}
		switch cmd {
		case "CAPABILITY":
			out(w, "* CAPABILITY IMAP4rev1 UIDPLUS MOVE IDLE SEARCH CONDSTORE QRESYNC SPECIAL-USE NAMESPACE")
			out(w, tag+" OK CAPABILITY completed")
		case "NAMESPACE":
			out(w, "* NAMESPACE ((\"\" \"/\")) NIL NIL")
			out(w, tag+" OK NAMESPACE completed")
		case "LIST", "LSUB":
			for _, name := range []string{"INBOX", "Sent", "Drafts", "Spam", "Trash", "Archive"} {
				out(w, fmt.Sprintf("* LIST (\\HasNoChildren) \"/\" \"%s\"", name))
			}
			out(w, tag+" OK LIST completed")
		case "SELECT", "EXAMINE":
			if len(args) != 1 {
				out(w, tag+" BAD mailbox required")
				continue
			}
			name := unquote(args[0])
			msgs, folder, err := s.backend.ListProtocolFolder(context.Background(), principal.MailboxID, name)
			if err != nil {
				out(w, tag+" NO no such mailbox")
				continue
			}
			selected = name
			out(w, fmt.Sprintf("* %d EXISTS", len(msgs)))
			out(w, fmt.Sprintf("* OK [UIDVALIDITY %d]", folder.UIDValidity))
			out(w, fmt.Sprintf("* OK [UIDNEXT %d]", folder.NextUID))
			out(w, tag+" OK [READ-WRITE] SELECT completed")
		case "FETCH":
			s.fetch(w, tag, args, principal, selected, false)
		case "UID":
			if len(args) > 0 && strings.ToUpper(args[0]) == "FETCH" {
				s.fetch(w, tag, args[1:], principal, selected, true)
			} else {
				out(w, tag+" BAD unsupported UID command")
			}
		case "SEARCH":
			out(w, "* SEARCH")
			out(w, tag+" OK SEARCH completed")
		case "NOOP":
			out(w, tag+" OK NOOP completed")
		case "IDLE":
			out(w, "+ idling")
			for {
				l, e := r.ReadString('\n')
				if e != nil {
					return
				}
				if strings.EqualFold(strings.TrimSpace(l), "DONE") {
					out(w, tag+" OK IDLE completed")
					break
				}
			}
		case "LOGOUT":
			out(w, "* BYE logout")
			out(w, tag+" OK LOGOUT completed")
			return
		default:
			out(w, tag+" BAD command not implemented")
		}
	}
}
func (s *Server) fetch(w *bufio.Writer, tag string, args []string, p *service.ProtocolPrincipal, folder string, uid bool) {
	if folder == "" || len(args) < 2 {
		out(w, tag+" BAD select a mailbox first")
		return
	}
	msgs, _, e := s.backend.ListProtocolFolder(context.Background(), p.MailboxID, folder)
	if e != nil {
		out(w, tag+" NO mailbox unavailable")
		return
	}
	n, _ := strconv.Atoi(strings.Split(args[0], ":")[0])
	for i, m := range msgs {
		match := i+1 == n
		if uid {
			match = int(m.UID) == n
		}
		if match {
			out(w, fmt.Sprintf("* %d FETCH (UID %d RFC822.SIZE %d BODY[] {%d}", i+1, m.UID, len(m.Raw), len(m.Raw)))
			_, _ = w.Write(m.Raw)
			_, _ = w.WriteString("\r\n)\r\n")
			_ = w.Flush()
		}
	}
	out(w, tag+" OK FETCH completed")
}
func out(w *bufio.Writer, v string) { _, _ = w.WriteString(v + "\r\n"); _ = w.Flush() }
func unquote(v string) string       { return strings.Trim(v, "\"") }
