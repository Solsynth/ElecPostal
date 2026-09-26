package smtp

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"src.solsynth.dev/sosys/elecpostal/internal/config"
	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/senderauth"
)

type fakeAuthVerifier struct {
	result  senderauth.Result
	calls   int
	lastEnv string
	lastIP  string
}

func (f *fakeAuthVerifier) Verify(_ context.Context, _ []byte, envelopeFrom, peerIP string) senderauth.Result {
	f.calls++
	f.lastEnv = envelopeFrom
	f.lastIP = peerIP
	return f.result
}

// newSessionWithVerifier mirrors newSession but lets the test install a sender
// authentication verifier before any connection is served.
func newSessionWithVerifier(t *testing.T, backend *fakeBackend, port string, verifier senderauth.Verifier) (*bufio.Reader, *bufio.Writer, net.Conn) {
	t.Helper()
	s, err := New(config.ListenerConfig{Enabled: true, Port: port, MaxMessageBytes: 1024 * 1024}, "example.test", backend)
	if err != nil {
		t.Fatal(err)
	}
	if verifier != nil {
		s.SetAuthVerifier(verifier)
	}
	client, server := net.Pipe()
	go s.serve(server)
	r, w := bufio.NewReader(client), bufio.NewWriter(client)
	if line := readReply(t, r); !strings.HasPrefix(line, "220") {
		t.Fatalf("greeting=%q", line)
	}
	return r, w, client
}

// TestSMTPInboundCarriesAuthentication: an unauthenticated inbound message runs
// the verifier and its JSON result reaches ReceiveEmailInput.Authentication.
func TestSMTPInboundCarriesAuthentication(t *testing.T) {
	backend := &fakeBackend{mailboxes: map[string]*database.Mailbox{"alice@example.test": {ID: "alice"}}}
	verifier := &fakeAuthVerifier{result: senderauth.Result{
		SPF: "fail", DKIM: "pass", DMARC: "reject",
		Warnings: []string{"Possible phishing: SPF verification failed"},
	}}
	r, w, conn := newSessionWithVerifier(t, backend, "25", verifier)
	defer conn.Close()

	setupMail(t, r, w, "alice@example.test")
	if got := data(t, r, w, "From: sender@remote.test\r\nSubject: hi\r\n\r\nbody"); !strings.HasPrefix(got, "250") {
		t.Fatal(got)
	}

	if verifier.calls != 1 {
		t.Fatalf("verifier calls = %d, want 1", verifier.calls)
	}
	if verifier.lastEnv != "sender@remote.test" {
		t.Fatalf("verifier envelope = %q", verifier.lastEnv)
	}
	if verifier.lastIP == "" {
		t.Fatal("verifier peer IP is empty")
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.inputs) != 1 {
		t.Fatalf("inputs = %d, want 1", len(backend.inputs))
	}
	var got struct {
		SPF      string   `json:"spf"`
		DKIM     string   `json:"dkim"`
		DMARC    string   `json:"dmarc"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(backend.inputs[0].Authentication, &got); err != nil {
		t.Fatalf("unmarshal authentication %q: %v", backend.inputs[0].Authentication, err)
	}
	if got.SPF != "fail" || got.DKIM != "pass" || got.DMARC != "reject" {
		t.Fatalf("authentication = %+v", got)
	}
	if len(got.Warnings) != 1 || got.Warnings[0] != "Possible phishing: SPF verification failed" {
		t.Fatalf("warnings = %v", got.Warnings)
	}
}

// TestSMTPInboundWithoutVerifier: no verifier means no Authentication JSON.
func TestSMTPInboundWithoutVerifier(t *testing.T) {
	backend := &fakeBackend{mailboxes: map[string]*database.Mailbox{"alice@example.test": {ID: "alice"}}}
	r, w, conn := newSession(t, backend, "25")
	defer conn.Close()

	setupMail(t, r, w, "alice@example.test")
	if got := data(t, r, w, "From: sender@remote.test\r\nSubject: hi\r\n\r\nbody"); !strings.HasPrefix(got, "250") {
		t.Fatal(got)
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.inputs) != 1 {
		t.Fatalf("inputs = %d, want 1", len(backend.inputs))
	}
	if len(backend.inputs[0].Authentication) != 0 {
		t.Fatalf("authentication = %q, want empty", backend.inputs[0].Authentication)
	}
}

// TestSMTPAuthenticatedSubmissionSkipsVerification: trusted submission is not
// authenticated and carries no Authentication JSON.
func TestSMTPAuthenticatedSubmissionSkipsVerification(t *testing.T) {
	backend := &fakeBackend{
		authOK:         true,
		mailboxSenders: map[string]map[string]bool{"alice-box": {"alice@example.test": true}},
		mailboxes:      map[string]*database.Mailbox{"bob@example.test": {ID: "bob"}},
	}
	verifier := &fakeAuthVerifier{result: senderauth.Result{SPF: "fail"}}
	r, w, conn := newSessionWithVerifier(t, backend, "587", verifier)
	defer conn.Close()

	_ = command(t, r, w, "EHLO test")
	if got := command(t, r, w, "AUTH PLAIN AGFsaWNlQGV4YW1wbGUudGVzdABzZWNyZXQ="); !strings.HasPrefix(got, "235") {
		t.Fatal(got)
	}
	if got := command(t, r, w, "MAIL FROM:<alice@example.test>"); !strings.HasPrefix(got, "250") {
		t.Fatal(got)
	}
	if got := command(t, r, w, "RCPT TO:<bob@example.test>"); !strings.HasPrefix(got, "250") {
		t.Fatal(got)
	}
	if got := data(t, r, w, "From: alice@example.test\r\nTo: bob@example.test\r\nSubject: hello\r\n\r\nbody"); !strings.HasPrefix(got, "250") {
		t.Fatal(got)
	}

	if verifier.calls != 0 {
		t.Fatalf("verifier calls = %d, want 0 for authenticated submission", verifier.calls)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.inputs) != 1 {
		t.Fatalf("inputs = %d, want 1", len(backend.inputs))
	}
	if len(backend.inputs[0].Authentication) != 0 {
		t.Fatalf("authentication = %q, want empty", backend.inputs[0].Authentication)
	}
}
