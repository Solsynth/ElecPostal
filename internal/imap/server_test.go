package imap

import (
	"context"
	"strings"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap"

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

// expungeBackend records what EXPUNGE asked the storage layer to do.
type expungeBackend struct {
	messages []service.ProtocolMessage
	moved    []string
	deleted  []string
}

func (b *expungeBackend) AuthenticateMailProtocolAddress(context.Context, string, string, string) (*service.ProtocolPrincipal, error) {
	return nil, nil
}

func (b *expungeBackend) ListProtocolFolder(context.Context, string, string) ([]service.ProtocolMessage, *database.MailFolder, error) {
	return b.messages, &database.MailFolder{Name: "Trash", NextUID: 2, UIDValidity: 1}, nil
}

func (b *expungeBackend) ListProtocolFolders(context.Context, string) ([]database.MailFolder, error) {
	return nil, nil
}

func (b *expungeBackend) OpenProtocolMessage(context.Context, string) (mailmime.MessageSource, error) {
	return mailmime.MessageSource{}, nil
}

func (b *expungeBackend) AppendProtocolMessage(context.Context, string, string, []byte, []string, time.Time) (uint32, uint64, error) {
	return 0, 0, nil
}

func (b *expungeBackend) MoveProtocolMessages(_ context.Context, _, from, to string, ids []string) error {
	b.moved = append(b.moved, from+">"+to+":"+strings.Join(ids, ","))
	return nil
}

func (b *expungeBackend) DeleteProtocolMessages(_ context.Context, _, folder string, ids []string) error {
	b.deleted = append(b.deleted, folder+":"+strings.Join(ids, ","))
	return nil
}

func (b *expungeBackend) CopyProtocolMessages(context.Context, string, string, string, []string) error {
	return nil
}

func (b *expungeBackend) StoreProtocolFlags(context.Context, string, string, []string, []string, string, uint64) ([]service.ProtocolStoreResult, error) {
	return nil, nil
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
