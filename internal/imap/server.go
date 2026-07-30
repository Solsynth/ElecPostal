// Package imap exposes ElecPostal mailboxes through the stable go-imap server.
package imap

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/mail"
	"strings"
	"sync"
	"time"

	goimap "github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	"github.com/emersion/go-imap/backend/backendutil"
	imapserver "github.com/emersion/go-imap/server"
	messageproto "github.com/emersion/go-message/textproto"

	"src.solsynth.dev/sosys/elecpostal/internal/config"
	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/service"
)

type Backend interface {
	AuthenticateMailProtocolAddress(context.Context, string, string, string) (*service.ProtocolPrincipal, error)
	ListProtocolFolder(context.Context, string, string) ([]service.ProtocolMessage, *database.MailFolder, error)
	ListProtocolFolders(context.Context, string) ([]database.MailFolder, error)
	MoveProtocolMessages(context.Context, string, string, string, []string) error
	CopyProtocolMessages(context.Context, string, string, string, []string) error
	StoreProtocolFlags(context.Context, string, string, []string, []string, string, uint64) ([]service.ProtocolStoreResult, error)
}

// Server delegates command parsing, literals, STARTTLS, SASL PLAIN, and IMAP
// state transitions to go-imap; this package is the ElecPostal storage adapter.
type Server struct {
	cfg     config.ListenerConfig
	backend Backend
	tls     *tls.Config
	server  *imapserver.Server
	ln      net.Listener
	wg      sync.WaitGroup
}

func New(cfg config.ListenerConfig, b Backend) (*Server, error) {
	if b == nil {
		return nil, fmt.Errorf("IMAP backend is required")
	}
	s := &Server{cfg: cfg, backend: b}
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
	s.server = imapserver.New(&imapBackend{backend: b})
	s.server.AllowInsecureAuth = s.tls == nil
	if strings.EqualFold(cfg.TLSMode, "starttls") {
		s.server.TLSConfig = s.tls
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
	if strings.EqualFold(s.cfg.TLSMode, "implicit") {
		ln = tls.NewListener(ln, s.tls)
	}
	s.ln = ln
	s.wg.Add(1)
	go func() { defer s.wg.Done(); _ = s.server.Serve(ln) }()
	return nil
}
func (s *Server) Close() error { _ = s.server.Close(); s.wg.Wait(); return nil }

type imapBackend struct{ backend Backend }

func (b *imapBackend) Login(_ *goimap.ConnInfo, username, password string) (backend.User, error) {
	p, err := b.backend.AuthenticateMailProtocolAddress(context.Background(), username, password, "imap")
	if err != nil {
		return nil, backend.ErrInvalidCredentials
	}
	return &imapUser{backend: b.backend, mailboxID: p.MailboxID, username: username}, nil
}

type imapUser struct {
	backend             Backend
	mailboxID, username string
}

func (u *imapUser) Username() string { return u.username }
func (u *imapUser) ListMailboxes(_ bool) ([]backend.Mailbox, error) {
	folders, err := u.backend.ListProtocolFolders(context.Background(), u.mailboxID)
	if err != nil {
		return nil, err
	}
	out := make([]backend.Mailbox, 0, len(folders))
	for _, f := range folders {
		out = append(out, &imapMailbox{user: u, name: f.Name, folder: f})
	}
	return out, nil
}
func (u *imapUser) GetMailbox(name string) (backend.Mailbox, error) {
	msgs, f, err := u.backend.ListProtocolFolder(context.Background(), u.mailboxID, name)
	_ = msgs
	if err != nil {
		return nil, backend.ErrNoSuchMailbox
	}
	return &imapMailbox{user: u, name: f.Name, folder: *f}, nil
}
func (u *imapUser) CreateMailbox(string) error         { return fmt.Errorf("CREATE is not supported") }
func (u *imapUser) DeleteMailbox(string) error         { return fmt.Errorf("DELETE is not supported") }
func (u *imapUser) RenameMailbox(string, string) error { return fmt.Errorf("RENAME is not supported") }
func (u *imapUser) Logout() error                      { return nil }

type imapMailbox struct {
	user   *imapUser
	name   string
	folder database.MailFolder
}

func (m *imapMailbox) Name() string { return m.name }
func (m *imapMailbox) Info() (*goimap.MailboxInfo, error) {
	attrs := []string{goimap.HasNoChildrenAttr}
	if v := imapSpecialUse(m.folder.SpecialUse); v != "" {
		attrs = append(attrs, v)
	}
	return &goimap.MailboxInfo{Name: m.name, Delimiter: "/", Attributes: attrs}, nil
}
func (m *imapMailbox) messages() ([]service.ProtocolMessage, *database.MailFolder, error) {
	return m.user.backend.ListProtocolFolder(context.Background(), m.user.mailboxID, m.name)
}
func (m *imapMailbox) Status(items []goimap.StatusItem) (*goimap.MailboxStatus, error) {
	msgs, f, err := m.messages()
	if err != nil {
		return nil, err
	}
	unseen := uint32(0)
	for _, x := range msgs {
		if !hasFlag(x.Flags, goimap.SeenFlag) {
			unseen++
		}
	}
	st := goimap.NewMailboxStatus(m.name, items)
	st.Flags = []string{goimap.SeenFlag, goimap.AnsweredFlag, goimap.FlaggedFlag, goimap.DeletedFlag, goimap.DraftFlag}
	st.PermanentFlags = append([]string{}, st.Flags...)
	st.Messages = uint32(len(msgs))
	st.Unseen = unseen
	st.UidNext = f.NextUID
	st.UidValidity = f.UIDValidity
	return st, nil
}
func (m *imapMailbox) SetSubscribed(bool) error { return nil }
func (m *imapMailbox) Check() error             { return nil }
func (m *imapMailbox) ListMessages(uid bool, set *goimap.SeqSet, items []goimap.FetchItem, ch chan<- *goimap.Message) error {
	defer close(ch)
	msgs, _, err := m.messages()
	if err != nil {
		return err
	}
	for i, x := range msgs {
		n := uint32(i + 1)
		if uid {
			n = x.UID
		}
		if !set.Contains(n) {
			continue
		}
		// NewMessage records the client's requested items and their ordering.
		// Without it, go-imap formats FETCH responses as an empty list even
		// though the mailbox correctly reports EXISTS, which makes clients show
		// an apparently empty inbox.
		msg := goimap.NewMessage(uint32(i+1), items)
		msg.Uid = x.UID
		msg.Flags = x.Flags
		msg.Size = uint32(len(x.Raw))
		msg.InternalDate = time.Now()
		msg.Body = map[*goimap.BodySectionName]goimap.Literal{}
		if err := populateFetchMetadata(msg, x.Raw, items); err != nil {
			return err
		}
		for _, item := range items {
			for _, item = range item.Expand() {
				if section, err := goimap.ParseBodySectionName(item); err == nil {
					msg.Body[section] = literal{Reader: bytes.NewReader(x.Raw), n: len(x.Raw)}
				}
			}
		}
		ch <- msg
	}
	return nil
}

func populateFetchMetadata(msg *goimap.Message, raw []byte, items []goimap.FetchItem) error {
	needEnvelope, needBodyStructure := false, false
	for _, item := range items {
		for _, expanded := range item.Expand() {
			switch expanded {
			case goimap.FetchEnvelope:
				needEnvelope = true
			case goimap.FetchBody, goimap.FetchBodyStructure:
				needBodyStructure = true
			}
		}
	}
	if !needEnvelope && !needBodyStructure {
		return nil
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	header := messageHeader(parsed.Header)
	if needEnvelope {
		msg.Envelope, err = backendutil.FetchEnvelope(header)
		if err != nil {
			return err
		}
	}
	if needBodyStructure {
		msg.BodyStructure, err = backendutil.FetchBodyStructure(header, parsed.Body, true)
	}
	return err
}

func messageHeader(source mail.Header) messageproto.Header {
	var header messageproto.Header
	for key, values := range source {
		for _, value := range values {
			header.Add(key, value)
		}
	}
	return header
}
func (m *imapMailbox) SearchMessages(uid bool, c *goimap.SearchCriteria) ([]uint32, error) {
	msgs, _, err := m.messages()
	if err != nil {
		return nil, err
	}
	out := []uint32{}
	for i, x := range msgs {
		if matchesCriteria(x, c) {
			if uid {
				out = append(out, x.UID)
			} else {
				out = append(out, uint32(i+1))
			}
		}
	}
	return out, nil
}
func (m *imapMailbox) CreateMessage([]string, time.Time, goimap.Literal) error {
	return fmt.Errorf("APPEND is not supported")
}
func (m *imapMailbox) UpdateMessagesFlags(uid bool, set *goimap.SeqSet, op goimap.FlagsOp, flags []string) error {
	msgs, _, err := m.messages()
	if err != nil {
		return err
	}
	ids := []string{}
	for i, x := range msgs {
		n := uint32(i + 1)
		if uid {
			n = x.UID
		}
		if set.Contains(n) {
			ids = append(ids, x.EmailID)
		}
	}
	mode := "replace"
	if op == goimap.AddFlags {
		mode = "add"
	}
	if op == goimap.RemoveFlags {
		mode = "remove"
	}
	_, err = m.user.backend.StoreProtocolFlags(context.Background(), m.user.mailboxID, m.name, ids, flags, mode, 0)
	return err
}
func (m *imapMailbox) CopyMessages(uid bool, set *goimap.SeqSet, dest string) error {
	return m.transfer(uid, set, dest, false)
}
func (m *imapMailbox) MoveMessages(uid bool, set *goimap.SeqSet, dest string) error {
	return m.transfer(uid, set, dest, true)
}
func (m *imapMailbox) Expunge() error {
	msgs, _, err := m.messages()
	if err != nil {
		return err
	}
	ids := []string{}
	for _, x := range msgs {
		if hasFlag(x.Flags, goimap.DeletedFlag) {
			ids = append(ids, x.EmailID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return m.user.backend.MoveProtocolMessages(context.Background(), m.user.mailboxID, m.name, "Trash", ids)
}
func (m *imapMailbox) transfer(uid bool, set *goimap.SeqSet, dest string, move bool) error {
	msgs, _, err := m.messages()
	if err != nil {
		return err
	}
	ids := []string{}
	for i, x := range msgs {
		n := uint32(i + 1)
		if uid {
			n = x.UID
		}
		if set.Contains(n) {
			ids = append(ids, x.EmailID)
		}
	}
	if move {
		return m.user.backend.MoveProtocolMessages(context.Background(), m.user.mailboxID, m.name, dest, ids)
	}
	return m.user.backend.CopyProtocolMessages(context.Background(), m.user.mailboxID, m.name, dest, ids)
}

type literal struct {
	io.Reader
	n int
}

func (l literal) Len() int               { return l.n }
func imapSpecialUse(value string) string { return strings.TrimPrefix(value, `\`) }
func hasFlag(flags []string, flag string) bool {
	for _, v := range flags {
		if strings.EqualFold(v, flag) {
			return true
		}
	}
	return false
}
func matchesCriteria(m service.ProtocolMessage, c *goimap.SearchCriteria) bool {
	if c.Uid != nil && !c.Uid.Contains(m.UID) {
		return false
	}
	for _, f := range c.WithFlags {
		if !hasFlag(m.Flags, f) {
			return false
		}
	}
	for _, f := range c.WithoutFlags {
		if hasFlag(m.Flags, f) {
			return false
		}
	}
	raw := strings.ToLower(string(m.Raw))
	for _, v := range c.Text {
		if !strings.Contains(raw, strings.ToLower(v)) {
			return false
		}
	}
	for _, v := range c.Body {
		if !strings.Contains(raw, strings.ToLower(v)) {
			return false
		}
	}
	for k, values := range c.Header {
		for _, v := range values {
			if !strings.Contains(raw, strings.ToLower(k+": "+v)) {
				return false
			}
		}
	}
	return true
}
func searchMatch(m service.ProtocolMessage, args []string) bool {
	return matchesCriteria(m, &goimap.SearchCriteria{WithFlags: searchFlags(args, true), WithoutFlags: searchFlags(args, false)})
}
func searchFlags(args []string, seen bool) []string {
	for _, a := range args {
		if (seen && strings.EqualFold(a, "SEEN")) || (!seen && strings.EqualFold(a, "UNSEEN")) {
			return []string{goimap.SeenFlag}
		}
	}
	return nil
}
func matchesSet(set string, sequence int, uid uint32, useUID bool, last int) bool {
	value := sequence
	if useUID {
		value = int(uid)
	}
	for _, part := range strings.Split(set, ",") {
		bounds := strings.Split(part, ":")
		parse := func(v string) int {
			if v == "*" {
				return last
			}
			n := 0
			fmt.Sscan(v, &n)
			return n
		}
		lo := parse(bounds[0])
		hi := lo
		if len(bounds) > 1 {
			hi = parse(bounds[1])
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
