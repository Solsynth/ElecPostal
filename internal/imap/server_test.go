package imap

import (
	"testing"

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
	message := service.ProtocolMessage{UID: 7, Flags: []string{"\\Seen"}, Raw: []byte("From: sender@example.test\r\nSubject: Invoice\r\n\r\nreceipt")}
	for _, terms := range [][]string{{"SEEN"}, {"SUBJECT", "invoice"}, {"TEXT", "receipt"}, {"UID", "7"}} {
		if !searchMatch(message, terms) {
			t.Fatalf("expected match for %v", terms)
		}
	}
	if searchMatch(message, []string{"UNSEEN"}) {
		t.Fatal("unexpected unseen match")
	}
}
