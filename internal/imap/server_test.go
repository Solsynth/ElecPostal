package imap

import (
	"context"
	"strings"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"

	"src.solsynth.dev/sosys/elecpostal/internal/config"
	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/mailmime"
	"src.solsynth.dev/sosys/elecpostal/internal/service"
)

func TestMatchesSet(t *testing.T) {
	for _, test := range []struct {
		set      string
		sequence int
		uid      uint32
		byUID    bool
		want     bool
	}{
		{"1:3", 2, 9, false, true}, {"2,4", 3, 9, false, false}, {"*", 5, 44, false, true}, {"40:45", 1, 42, true, true},
	} {
		if got := matchesSet(test.set, test.sequence, test.uid, test.byUID, 5); got != test.want {
			t.Fatalf("matchesSet(%q)=%v, want %v", test.set, got, test.want)
		}
	}
}

func TestSearchMatch(t *testing.T) {
	message := service.ProtocolMessage{UID: 7, Flags: []string{"\\Seen"}, Subject: "Invoice", Body: "receipt"}
	for _, terms := range [][]string{{"SEEN"}, {"SUBJECT", "invoice"}, {"TEXT", "receipt"}, {"UID", "7"}} {
		if !searchMatch(message, terms) {
			t.Fatalf("expected match for %v", terms)
		}
	}
	if searchMatch(message, []string{"UNSEEN"}) {
		t.Fatal("unexpected unseen match")
	}
}

func TestPopulateFetchMetadataForAppleMailFetch(t *testing.T) {
	items := []goimap.FetchItem{goimap.FetchEnvelope, goimap.FetchBodyStructure, goimap.FetchFlags, goimap.FetchRFC822Size}
	message := goimap.NewMessage(1, items)
	if err := populateFetchMetadata(message, []byte("From: sender@example.test\r\nTo: receiver@example.test\r\nSubject: Test\r\nContent-Type: text/plain\r\n\r\nbody"), items); err != nil {
		t.Fatal(err)
	}
	if message.Envelope == nil || message.Envelope.Subject != "Test" {
		t.Fatalf("envelope = %#v", message.Envelope)
	}
	if message.BodyStructure == nil {
		t.Fatal("BODYSTRUCTURE was not populated")
	}
}

// stubBackend implements the half of Backend a test does not exercise, so each
// fake declares only the methods it actually answers.
type stubBackend struct{}

func (stubBackend) AuthenticateMailProtocolAddress(context.Context, string, string, string) (*service.ProtocolPrincipal, error) {
	return nil, nil
}
func (stubBackend) ListProtocolFolders(context.Context, string) ([]database.MailFolder, error) {
	return nil, nil
}
func (stubBackend) ListProtocolFolder(context.Context, string, string) ([]service.ProtocolMessage, *database.MailFolder, error) {
	return nil, nil, nil
}
func (stubBackend) OpenProtocolMessage(context.Context, string) (mailmime.MessageSource, error) {
	return mailmime.MessageSource{}, nil
}
func (stubBackend) AppendProtocolMessage(context.Context, string, string, []byte, []string, time.Time) (uint32, uint64, error) {
	return 0, 0, nil
}
func (stubBackend) MoveProtocolMessages(context.Context, string, string, string, []string) error {
	return nil
}
func (stubBackend) DeleteProtocolMessages(context.Context, string, string, []string) error {
	return nil
}
func (stubBackend) CopyProtocolMessages(context.Context, string, string, string, []string) error {
	return nil
}
func (stubBackend) StoreProtocolFlags(context.Context, string, string, []string, []string, string, uint64) ([]service.ProtocolStoreResult, error) {
	return nil, nil
}
func (stubBackend) SetProtocolKeywords(context.Context, string, string, []string, []string, string) error {
	return nil
}

// expungeBackend records what EXPUNGE asked the storage layer to do.
type expungeBackend struct {
	stubBackend
	messages []service.ProtocolMessage
	moved    []string
	deleted  []string
}

func (b *expungeBackend) ListProtocolFolder(context.Context, string, string) ([]service.ProtocolMessage, *database.MailFolder, error) {
	return b.messages, &database.MailFolder{Name: "Trash", NextUID: 2, UIDValidity: 1}, nil
}

func (b *expungeBackend) MoveProtocolMessages(_ context.Context, _, from, to string, ids []string) error {
	b.moved = append(b.moved, from+">"+to+":"+strings.Join(ids, ","))
	return nil
}

func (b *expungeBackend) DeleteProtocolMessages(_ context.Context, _, folder string, ids []string) error {
	b.deleted = append(b.deleted, folder+":"+strings.Join(ids, ","))
	return nil
}

func TestExpungeDeletesOnlyFlaggedMessagesThroughTheStorageLayer(t *testing.T) {
	backend := &expungeBackend{messages: []service.ProtocolMessage{
		{EmailID: "e-1", UID: 1, Flags: []string{goimap.DeletedFlag}},
		{EmailID: "e-2", UID: 2, Flags: []string{goimap.SeenFlag}},
	}}
	user := &imapUser{backend: backend, mailboxID: "mb-1"}

	// Only the \Deleted message is expunged, and the mailbox name travels with
	// it so the storage layer can empty Trash instead of filing into it.
	if err := (&imapMailbox{user: user, name: "Trash"}).Expunge(); err != nil {
		t.Fatalf("Expunge(Trash) error = %v", err)
	}
	if len(backend.deleted) != 1 || backend.deleted[0] != "Trash:e-1" {
		t.Fatalf("deleted = %v, want [Trash:e-1]", backend.deleted)
	}
	if len(backend.moved) != 0 {
		t.Fatalf("moved = %v, want none: EXPUNGE is not a mailbox move", backend.moved)
	}
}

// listBackend serves the protocol namespace a hosted address starts with.
type listBackend struct {
	stubBackend
	folders []database.MailFolder
}

func (b *listBackend) AuthenticateMailProtocolAddress(context.Context, string, string, string) (*service.ProtocolPrincipal, error) {
	return &service.ProtocolPrincipal{MailboxID: "mb-1"}, nil
}

func (b *listBackend) ListProtocolFolders(context.Context, string) ([]database.MailFolder, error) {
	return b.folders, nil
}

func (b *listBackend) ListProtocolFolder(context.Context, string, string) ([]service.ProtocolMessage, *database.MailFolder, error) {
	return nil, nil, service.ErrNotFound
}

// TestListAdvertisesRFC6154SpecialUseAttributes drives a real client over a
// socket: a folder whose role the client cannot read is a folder it will not
// file mail into.
func TestListAdvertisesRFC6154SpecialUseAttributes(t *testing.T) {
	backend := &listBackend{}
	for _, spec := range []struct{ name, use string }{
		{"INBOX", `\Inbox`}, {"Sent", `\Sent`}, {"Drafts", `\Drafts`},
		{"Spam", `\Junk`}, {"Trash", `\Trash`}, {"Archive", `\Archive`},
	} {
		backend.folders = append(backend.folders, database.MailFolder{Name: spec.name, SpecialUse: spec.use, Subscribed: true})
	}
	srv, err := New(config.ListenerConfig{Enabled: true, Host: "127.0.0.1", Port: "0", TLSMode: "disabled"}, backend)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer srv.Close()

	c, err := imapclient.Dial(srv.ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer c.Logout()
	if err := c.Login("ada@example.com", "secret"); err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	ch := make(chan *goimap.MailboxInfo, 8)
	if err := c.List("", "*", ch); err != nil {
		t.Fatalf("List() error = %v", err)
	}
	got := map[string][]string{}
	for mailbox := range ch {
		got[mailbox.Name] = mailbox.Attributes
	}
	if len(got) != 6 {
		t.Fatalf("LIST returned %v, want the six folders of the namespace", got)
	}
	for name, want := range map[string]string{
		"Sent": goimap.SentAttr, "Drafts": goimap.DraftsAttr,
		"Spam": goimap.JunkAttr, "Trash": goimap.TrashAttr, "Archive": goimap.ArchiveAttr,
	} {
		attrs := got[name]
		found := false
		for _, attr := range attrs {
			found = found || attr == want
		}
		if !found {
			t.Fatalf("%s attributes = %v, want %q", name, attrs, want)
		}
	}
	// RFC 6154 defines no INBOX attribute, so advertising `\Inbox` would leave
	// a client parsing an attribute it cannot act on.
	if attrs := got["INBOX"]; len(attrs) != 1 || attrs[0] != goimap.HasNoChildrenAttr {
		t.Fatalf("INBOX attributes = %v, want [%s]", attrs, goimap.HasNoChildrenAttr)
	}
}

// storeCall is one write a mailbox asked storage to perform.
type storeCall struct {
	ids   []string
	names []string
	mode  string
}

// keywordBackend serves one message carrying a tag keyword and records the
// writes a client issues, so the test can tell which store each half of a STORE
// was routed to.
type keywordBackend struct {
	stubBackend
	message  service.ProtocolMessage
	stored   []storeCall
	keywords []storeCall
}

func (b *keywordBackend) AuthenticateMailProtocolAddress(context.Context, string, string, string) (*service.ProtocolPrincipal, error) {
	return &service.ProtocolPrincipal{MailboxID: "mb-1"}, nil
}

func (b *keywordBackend) ListProtocolFolders(context.Context, string) ([]database.MailFolder, error) {
	return []database.MailFolder{{Name: "INBOX", Subscribed: true}}, nil
}

func (b *keywordBackend) ListProtocolFolder(context.Context, string, string) ([]service.ProtocolMessage, *database.MailFolder, error) {
	return []service.ProtocolMessage{b.message}, &database.MailFolder{Name: "INBOX", NextUID: 2, UIDValidity: 1}, nil
}

func (b *keywordBackend) OpenProtocolMessage(context.Context, string) (mailmime.MessageSource, error) {
	return mailmime.MessageSource{
		FromAddress: "sender@example.test", Subject: b.message.Subject, Body: b.message.Body, BodyType: "text/plain",
	}, nil
}

func (b *keywordBackend) StoreProtocolFlags(_ context.Context, _, _ string, ids, flags []string, mode string, _ uint64) ([]service.ProtocolStoreResult, error) {
	b.stored = append(b.stored, storeCall{ids: ids, names: flags, mode: mode})
	return nil, nil
}

func (b *keywordBackend) SetProtocolKeywords(_ context.Context, _, _ string, ids, keywords []string, mode string) error {
	b.keywords = append(b.keywords, storeCall{ids: ids, names: keywords, mode: mode})
	return nil
}

// TestTagKeywordsTravelAsIMAPFlags drives a real client over a socket: a tag a
// client cannot see is a tag it drops the next time it writes the message, so
// the tag has to arrive as a flag and come back as one.
func TestTagKeywordsTravelAsIMAPFlags(t *testing.T) {
	backend := &keywordBackend{message: service.ProtocolMessage{
		EmailID: "e-1", UID: 1,
		Flags:      []string{goimap.SeenFlag},
		Keywords:   []string{"Important"},
		RFC822Size: 64, Subject: "Invoice", Body: "receipt",
	}}
	srv, err := New(config.ListenerConfig{Enabled: true, Host: "127.0.0.1", Port: "0", TLSMode: "disabled"}, backend)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer srv.Close()

	c, err := imapclient.Dial(srv.ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer c.Logout()
	if err := c.Login("ada@example.com", "secret"); err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	status, err := c.Select("INBOX", false)
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	// `\*` is what lets a client store a keyword the server never sent it,
	// while FLAGS stays the fixed system list.
	if !hasFlag(status.PermanentFlags, goimap.TryCreateFlag) {
		t.Fatalf("PERMANENTFLAGS = %v, want %s", status.PermanentFlags, goimap.TryCreateFlag)
	}
	if !hasFlag(status.Flags, goimap.SeenFlag) || hasFlag(status.Flags, "Important") {
		t.Fatalf("FLAGS = %v, want the system flags only", status.Flags)
	}
	// Only SELECT carries PERMANENTFLAGS to a client (RFC 3501 has no such
	// STATUS item), so STATUS is here to prove the same mailbox status answers
	// both commands.
	summary, err := c.Status("INBOX", []goimap.StatusItem{goimap.StatusMessages, goimap.StatusUidNext})
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if summary.Messages != 1 {
		t.Fatalf("STATUS MESSAGES = %d, want 1", summary.Messages)
	}

	set := new(goimap.SeqSet)
	set.Add("1")
	ch := make(chan *goimap.Message, 4)
	if err := c.Fetch(set, []goimap.FetchItem{goimap.FetchUid, goimap.FetchFlags}, ch); err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	fetched := []*goimap.Message{}
	for msg := range ch {
		fetched = append(fetched, msg)
	}
	if len(fetched) != 1 {
		t.Fatalf("FETCH returned %d messages, want 1", len(fetched))
	}
	if flags := fetched[0].Flags; !hasFlag(flags, "Important") || !hasFlag(flags, goimap.SeenFlag) {
		t.Fatalf("FETCH FLAGS = %v, want the membership flag and the tag", flags)
	}

	ids, err := c.Search(&goimap.SearchCriteria{WithFlags: []string{"Important"}})
	if err != nil {
		t.Fatalf("Search(KEYWORD) error = %v", err)
	}
	if len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("SEARCH KEYWORD Important = %v, want [1]", ids)
	}
	ids, err = c.Search(&goimap.SearchCriteria{WithFlags: []string{"Archive"}})
	if err != nil {
		t.Fatalf("Search(KEYWORD) error = %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("SEARCH KEYWORD Archive = %v, want none: the message does not carry it", ids)
	}
	ids, err = c.Search(&goimap.SearchCriteria{WithoutFlags: []string{"Important"}})
	if err != nil {
		t.Fatalf("Search(UNKEYWORD) error = %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("SEARCH UNKEYWORD Important = %v, want none", ids)
	}

	if err := c.Store(set, goimap.AddFlags, []interface{}{"Important"}, nil); err != nil {
		t.Fatalf("Store(+FLAGS Important) error = %v", err)
	}
	if len(backend.keywords) != 1 {
		t.Fatalf("SetProtocolKeywords calls = %+v, want one for the tag", backend.keywords)
	}
	if call := backend.keywords[0]; !hasFlag(call.names, "Important") || call.mode != "add" {
		t.Fatalf("SetProtocolKeywords = %+v, want the tag added", call)
	}
	if call := backend.keywords[0]; len(call.ids) != 1 || call.ids[0] != "e-1" {
		t.Fatalf("SetProtocolKeywords ids = %v, want the selected message", call.ids)
	}
	if len(backend.stored) != 1 || len(backend.stored[0].names) != 0 {
		t.Fatalf("StoreProtocolFlags = %+v, want one call carrying no system flag", backend.stored)
	}

	if err := c.Store(set, goimap.AddFlags, []interface{}{goimap.SeenFlag}, nil); err != nil {
		t.Fatalf("Store(+FLAGS %s) error = %v", goimap.SeenFlag, err)
	}
	if len(backend.stored) != 2 || !hasFlag(backend.stored[1].names, goimap.SeenFlag) || backend.stored[1].mode != "add" {
		t.Fatalf("StoreProtocolFlags = %+v, want the system flag as before", backend.stored)
	}
	if len(backend.keywords) != 2 || len(backend.keywords[1].names) != 0 {
		t.Fatalf("SetProtocolKeywords = %+v, want a second call with no keyword", backend.keywords)
	}
}
