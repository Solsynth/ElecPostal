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
	CopyProtocolMessages(context.Context, string, string, string, []string) error
	StoreProtocolFlags(context.Context, string, string, []string, []string, string, uint64) ([]service.ProtocolStoreResult, error)
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
	// conn may be replaced by STARTTLS below; defer a closure so the active TLS
	// connection emits close_notify instead of abruptly closing the raw socket.
	defer func() { _ = conn.Close() }()
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
				out(w, "* CAPABILITY "+s.capability(secure, false))
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
			out(w, "* CAPABILITY "+s.capability(secure, true))
			out(w, tag+" OK CAPABILITY completed")
		case "NAMESPACE":
			out(w, "* NAMESPACE ((\"\" \"/\")) NIL NIL")
			out(w, tag+" OK NAMESPACE completed")
		case "LIST", "LSUB":
			for _, name := range []string{"INBOX", "Sent", "Drafts", "Spam", "Trash", "Archive"} {
				out(w, fmt.Sprintf("* LIST (\\HasNoChildren) \"/\" \"%s\"", name))
			}
			out(w, tag+" OK LIST completed")
		case "ENABLE":
			enabled := []string{}
			for _, capability := range args {
				if strings.EqualFold(capability, "CONDSTORE") || strings.EqualFold(capability, "QRESYNC") {
					enabled = append(enabled, strings.ToUpper(capability))
				}
			}
			if len(enabled) > 0 {
				out(w, "* ENABLED "+strings.Join(enabled, " "))
			}
			out(w, tag+" OK ENABLE completed")
		case "SELECT", "EXAMINE":
			if len(args) < 1 {
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
			out(w, fmt.Sprintf("* OK [HIGHESTMODSEQ %d]", folder.HighestModSeq))
			out(w, tag+" OK [READ-WRITE] SELECT completed")
		case "FETCH":
			s.fetch(w, tag, args, principal, selected, false)
		case "STORE":
			s.store(w, tag, args, principal, selected, false)
		case "COPY":
			s.copyMove(w, tag, args, principal, selected, false, false)
		case "MOVE":
			s.copyMove(w, tag, args, principal, selected, false, true)
		case "EXPUNGE":
			s.expunge(w, tag, principal, selected)
		case "UID":
			if len(args) == 0 {
				out(w, tag+" BAD UID command required")
				continue
			}
			switch strings.ToUpper(args[0]) {
			case "FETCH":
				s.fetch(w, tag, args[1:], principal, selected, true)
			case "SEARCH":
				s.search(w, tag, args[1:], principal, selected, true)
			case "STORE":
				s.store(w, tag, args[1:], principal, selected, true)
			case "COPY":
				s.copyMove(w, tag, args[1:], principal, selected, true, false)
			case "MOVE":
				s.copyMove(w, tag, args[1:], principal, selected, true, true)
			case "EXPUNGE":
				s.expunge(w, tag, principal, selected)
			default:
				out(w, tag+" BAD unsupported UID command")
			}
		case "SEARCH":
			s.search(w, tag, args, principal, selected, false)
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
func (s *Server) capability(secure, authenticated bool) string {
	values := []string{"IMAP4rev1", "UIDPLUS", "MOVE", "IDLE", "SEARCH", "CONDSTORE", "QRESYNC", "SPECIAL-USE", "NAMESPACE"}
	if !secure && s.tls != nil && strings.EqualFold(s.cfg.TLSMode, "starttls") {
		values = append(values, "STARTTLS")
	}
	if !authenticated && (secure || s.tls == nil) {
		values = append(values, "AUTH=PLAIN")
	}
	return strings.Join(values, " ")
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
	for i, m := range msgs {
		match := matchesSet(args[0], i+1, m.UID, uid, len(msgs))
		if match {
			out(w, fmt.Sprintf("* %d FETCH (UID %d RFC822.SIZE %d BODY[] {%d}", i+1, m.UID, len(m.Raw), len(m.Raw)))
			_, _ = w.Write(m.Raw)
			_, _ = w.WriteString("\r\n)\r\n")
			_ = w.Flush()
		}
	}
	out(w, tag+" OK FETCH completed")
}

func (s *Server) store(w *bufio.Writer, tag string, args []string, p *service.ProtocolPrincipal, folder string, uid bool) {
	if folder == "" || len(args) < 3 {
		out(w, tag+" BAD STORE requires a message set, operation, and flags")
		return
	}
	msgs, _, err := s.backend.ListProtocolFolder(context.Background(), p.MailboxID, folder)
	if err != nil {
		out(w, tag+" NO mailbox unavailable")
		return
	}
	ids := selectedIDs(msgs, args[0], uid)
	if len(ids) == 0 {
		out(w, tag+" OK STORE completed")
		return
	}
	operation := strings.ToUpper(args[1])
	mode := "replace"
	if strings.HasPrefix(operation, "+") {
		mode = "add"
	}
	if strings.HasPrefix(operation, "-") {
		mode = "remove"
	}
	unchanged := uint64(0)
	flagsAt := 2
	if strings.EqualFold(args[1], "UNCHANGEDSINCE") && len(args) >= 5 {
		unchanged, _ = strconv.ParseUint(args[2], 10, 64)
		operation = strings.ToUpper(args[3])
		flagsAt = 4
		mode = "replace"
		if strings.HasPrefix(operation, "+") {
			mode = "add"
		}
		if strings.HasPrefix(operation, "-") {
			mode = "remove"
		}
	}
	flags := parseFlags(strings.Join(args[flagsAt:], " "))
	updated, err := s.backend.StoreProtocolFlags(context.Background(), p.MailboxID, folder, ids, flags, mode, unchanged)
	if err != nil {
		out(w, tag+" NO STORE failed")
		return
	}
	if !strings.Contains(operation, ".SILENT") {
		for _, value := range updated {
			seq := sequenceFor(msgs, value.EmailID)
			out(w, fmt.Sprintf("* %d FETCH (UID %d FLAGS (%s) MODSEQ (%d))", seq, value.UID, strings.Join(value.Flags, " "), value.ModSeq))
		}
	}
	out(w, tag+" OK STORE completed")
}

func (s *Server) copyMove(w *bufio.Writer, tag string, args []string, p *service.ProtocolPrincipal, folder string, uid, move bool) {
	if folder == "" || len(args) < 2 {
		out(w, tag+" BAD select a mailbox and provide destination")
		return
	}
	msgs, _, err := s.backend.ListProtocolFolder(context.Background(), p.MailboxID, folder)
	if err != nil {
		out(w, tag+" NO mailbox unavailable")
		return
	}
	ids := selectedIDs(msgs, args[0], uid)
	destination := unquote(args[1])
	if move {
		err = s.backend.MoveProtocolMessages(context.Background(), p.MailboxID, folder, destination, ids)
	} else {
		err = s.backend.CopyProtocolMessages(context.Background(), p.MailboxID, folder, destination, ids)
	}
	if err != nil {
		out(w, tag+" NO destination unavailable")
		return
	}
	if move {
		out(w, tag+" OK [MOVE] MOVE completed")
	} else {
		out(w, tag+" OK [COPYUID] COPY completed")
	}
}

func (s *Server) expunge(w *bufio.Writer, tag string, p *service.ProtocolPrincipal, folder string) {
	if folder == "" {
		out(w, tag+" BAD select a mailbox first")
		return
	}
	msgs, _, err := s.backend.ListProtocolFolder(context.Background(), p.MailboxID, folder)
	if err != nil {
		out(w, tag+" NO mailbox unavailable")
		return
	}
	ids := []string{}
	for i, m := range msgs {
		if hasFlag(m.Flags, "\\Deleted") {
			ids = append(ids, m.EmailID)
			out(w, fmt.Sprintf("* %d EXPUNGE", i+1))
		}
	}
	if len(ids) > 0 {
		if err := s.backend.MoveProtocolMessages(context.Background(), p.MailboxID, folder, "Trash", ids); err != nil {
			out(w, tag+" NO EXPUNGE failed")
			return
		}
	}
	out(w, tag+" OK EXPUNGE completed")
}

func (s *Server) search(w *bufio.Writer, tag string, args []string, p *service.ProtocolPrincipal, folder string, uid bool) {
	if folder == "" {
		out(w, tag+" BAD select a mailbox first")
		return
	}
	msgs, _, err := s.backend.ListProtocolFolder(context.Background(), p.MailboxID, folder)
	if err != nil {
		out(w, tag+" NO mailbox unavailable")
		return
	}
	result := []string{}
	for i, m := range msgs {
		if searchMatch(m, args) {
			if uid {
				result = append(result, strconv.Itoa(int(m.UID)))
			} else {
				result = append(result, strconv.Itoa(i+1))
			}
		}
	}
	out(w, "* SEARCH "+strings.Join(result, " "))
	out(w, tag+" OK SEARCH completed")
}

func searchMatch(m service.ProtocolMessage, args []string) bool {
	if len(args) == 0 || (len(args) == 1 && strings.EqualFold(args[0], "ALL")) {
		return true
	}
	raw := strings.ToLower(string(m.Raw))
	for i := 0; i < len(args); i++ {
		token := strings.ToUpper(args[i])
		switch token {
		case "SEEN":
			if !hasFlag(m.Flags, "\\Seen") {
				return false
			}
		case "UNSEEN":
			if hasFlag(m.Flags, "\\Seen") {
				return false
			}
		case "DELETED":
			if !hasFlag(m.Flags, "\\Deleted") {
				return false
			}
		case "UNDELETED":
			if hasFlag(m.Flags, "\\Deleted") {
				return false
			}
		case "TEXT", "BODY", "FROM", "TO", "SUBJECT":
			if i+1 >= len(args) {
				return false
			}
			i++
			needle := strings.ToLower(unquote(args[i]))
			if !strings.Contains(raw, needle) {
				return false
			}
		case "UID":
			if i+1 >= len(args) {
				return false
			}
			i++
			if !matchesSet(args[i], 0, m.UID, true, int(m.UID)) {
				return false
			}
		case "NOT":
			if i+1 >= len(args) {
				return false
			}
			i++
			if searchMatch(m, []string{args[i]}) {
				return false
			}
		}
	}
	return true
}
func selectedIDs(msgs []service.ProtocolMessage, set string, uid bool) []string {
	ids := []string{}
	for i, m := range msgs {
		if matchesSet(set, i+1, m.UID, uid, len(msgs)) {
			ids = append(ids, m.EmailID)
		}
	}
	return ids
}
func sequenceFor(msgs []service.ProtocolMessage, id string) int {
	for i, m := range msgs {
		if m.EmailID == id {
			return i + 1
		}
	}
	return 0
}
func hasFlag(flags []string, flag string) bool {
	for _, value := range flags {
		if strings.EqualFold(value, flag) {
			return true
		}
	}
	return false
}
func parseFlags(raw string) []string {
	raw = strings.Trim(strings.TrimSpace(raw), "()")
	if raw == "" {
		return nil
	}
	return strings.Fields(raw)
}
func matchesSet(set string, sequence int, uid uint32, useUID bool, last int) bool {
	value := int(uid)
	if !useUID {
		value = sequence
	}
	for _, part := range strings.Split(set, ",") {
		bounds := strings.Split(part, ":")
		if len(bounds) == 1 {
			n, _ := strconv.Atoi(bounds[0])
			if bounds[0] == "*" {
				n = last
			}
			if value == n {
				return true
			}
			continue
		}
		lo, _ := strconv.Atoi(bounds[0])
		hi, _ := strconv.Atoi(bounds[1])
		if bounds[0] == "*" {
			lo = last
		}
		if bounds[1] == "*" {
			hi = last
		}
		if lo > hi {
			lo, hi = hi, lo
		}
		if value >= lo && value <= hi {
			return true
		}
	}
	return false
}
func out(w *bufio.Writer, v string) { _, _ = w.WriteString(v + "\r\n"); _ = w.Flush() }
func unquote(v string) string       { return strings.Trim(v, "\"") }
