package smtp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"src.solsynth.dev/sosys/elecpostal/internal/config"
	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/service"
)

type fakeBackend struct {
	mailboxes  map[string]*database.Mailbox
	defaultBox *database.Mailbox
	authOK     bool
	inputs     []service.ReceiveEmailInput
	mu         sync.Mutex
}

func (f *fakeBackend) ResolveLocalMailbox(_ context.Context, address string) (*database.Mailbox, error) {
	if address == "postmaster@example.test" && f.defaultBox != nil {
		return f.defaultBox, nil
	}
	if box := f.mailboxes[address]; box != nil {
		return box, nil
	}
	return nil, service.ErrNotFound
}
func (f *fakeBackend) AuthenticateMailProtocolAddress(_ context.Context, address, secret, protocol string) (*service.ProtocolPrincipal, error) {
	if f.authOK && address == "alice@example.test" && secret == "secret" && protocol == "smtp" {
		return &service.ProtocolPrincipal{AccountID: uuid.New()}, nil
	}
	return nil, service.ErrForbidden
}
func (f *fakeBackend) ReceiveEmail(_ context.Context, input service.ReceiveEmailInput) (*database.Email, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inputs = append(f.inputs, input)
	return &database.Email{ID: "received"}, nil
}

func newSession(t *testing.T, backend *fakeBackend, port string) (*bufio.Reader, *bufio.Writer, net.Conn) {
	t.Helper()
	s, err := New(config.ListenerConfig{Enabled: true, Port: port, MaxMessageBytes: 1024 * 1024}, "example.test", backend)
	if err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	go s.serve(server)
	r, w := bufio.NewReader(client), bufio.NewWriter(client)
	if line := readReply(t, r); !strings.HasPrefix(line, "220") {
		t.Fatalf("greeting=%q", line)
	}
	return r, w, client
}
func command(t *testing.T, r *bufio.Reader, w *bufio.Writer, value string) string {
	t.Helper()
	_, _ = w.WriteString(value + "\r\n")
	_ = w.Flush()
	return readReply(t, r)
}
func readReply(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	var last string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		line = strings.TrimSpace(line)
		last = line
		if len(line) < 4 || line[3] != '-' {
			return last
		}
	}
}
func setupMail(t *testing.T, r *bufio.Reader, w *bufio.Writer, recipient string) {
	t.Helper()
	if got := command(t, r, w, "EHLO test"); !strings.HasPrefix(got, "250") {
		t.Fatal(got)
	}
	if got := command(t, r, w, "MAIL FROM:<sender@remote.test>"); !strings.HasPrefix(got, "250") {
		t.Fatal(got)
	}
	if got := command(t, r, w, "RCPT TO:<"+recipient+">"); !strings.HasPrefix(got, "250") {
		t.Fatal(got)
	}
}
func data(t *testing.T, r *bufio.Reader, w *bufio.Writer, body string) string {
	t.Helper()
	if got := command(t, r, w, "DATA"); !strings.HasPrefix(got, "354") {
		t.Fatal(got)
	}
	_, _ = w.WriteString(body + "\r\n.\r\n")
	_ = w.Flush()
	return readReply(t, r)
}

func TestSMTPValidLocalMailbox(t *testing.T) {
	box := &database.Mailbox{ID: "alice"}
	backend := &fakeBackend{mailboxes: map[string]*database.Mailbox{"alice@example.test": box}}
	r, w, conn := newSession(t, backend, "25")
	defer conn.Close()
	setupMail(t, r, w, "alice@example.test")
	if got := data(t, r, w, "From: Sender <sender@remote.test>\r\nTo: alice@example.test\r\nSubject: hello\r\n\r\nplain body"); !strings.HasPrefix(got, "250") {
		t.Fatal(got)
	}
	if len(backend.inputs) != 1 || backend.inputs[0].MailboxID != "alice" || backend.inputs[0].Body != "plain body\r\n" {
		t.Fatalf("unexpected delivery: %#v", backend.inputs)
	}
}

func TestSMTPAcceptsESMTPEnvelopeParameters(t *testing.T) {
	box := &database.Mailbox{ID: "alice"}
	backend := &fakeBackend{mailboxes: map[string]*database.Mailbox{"alice@example.test": box}}
	r, w, conn := newSession(t, backend, "25")
	defer conn.Close()
	_ = command(t, r, w, "EHLO outlook.example")
	if got := command(t, r, w, "MAIL FROM:<sender@remote.test> SIZE=1234"); !strings.HasPrefix(got, "250") {
		t.Fatalf("MAIL FROM with SIZE parameter: %q", got)
	}
	if got := command(t, r, w, "RCPT TO:<alice@example.test> NOTIFY=SUCCESS,FAILURE"); !strings.HasPrefix(got, "250") {
		t.Fatalf("RCPT TO with ESMTP parameter: %q", got)
	}
}

func TestSMTPUnknownAndExternalRecipientsRejected(t *testing.T) {
	backend := &fakeBackend{mailboxes: map[string]*database.Mailbox{}}
	r, w, conn := newSession(t, backend, "25")
	defer conn.Close()
	_ = command(t, r, w, "EHLO test")
	_ = command(t, r, w, "MAIL FROM:<sender@remote.test>")
	for _, address := range []string{"missing@example.test", "person@external.test"} {
		if got := command(t, r, w, "RCPT TO:<"+address+">"); got != "550 5.1.1 User unknown" {
			t.Fatalf("%s: %q", address, got)
		}
	}
}

func TestSMTPPostmasterRoutesToDefaultMailbox(t *testing.T) {
	box := &database.Mailbox{ID: "default"}
	backend := &fakeBackend{defaultBox: box, mailboxes: map[string]*database.Mailbox{}}
	r, w, conn := newSession(t, backend, "25")
	defer conn.Close()
	setupMail(t, r, w, "postmaster@example.test")
	if got := data(t, r, w, "Subject: Postmaster\r\n\r\ntest"); !strings.HasPrefix(got, "250") {
		t.Fatal(got)
	}
	if len(backend.inputs) != 1 || backend.inputs[0].MailboxID != "default" {
		t.Fatalf("postmaster did not route to default: %#v", backend.inputs)
	}
}

func TestSMTPMIMEAttachmentIngestion(t *testing.T) {
	box := &database.Mailbox{ID: "alice"}
	backend := &fakeBackend{mailboxes: map[string]*database.Mailbox{"alice@example.test": box}}
	r, w, conn := newSession(t, backend, "25")
	defer conn.Close()
	setupMail(t, r, w, "alice@example.test")
	body := "Content-Type: multipart/mixed; boundary=abc\r\n\r\n--abc\r\nContent-Type: text/plain\r\n\r\nhello\r\n--abc\r\nContent-Type: text/plain; name=note.txt\r\nContent-Disposition: attachment; filename=note.txt\r\nContent-Transfer-Encoding: base64\r\n\r\nYXR0YWNobWVudCBieXRlcw==\r\n--abc--"
	if got := data(t, r, w, body); !strings.HasPrefix(got, "250") {
		t.Fatal(got)
	}
	if len(backend.inputs) != 1 || len(backend.inputs[0].Attachments) != 1 {
		t.Fatalf("attachments=%#v", backend.inputs)
	}
	a := backend.inputs[0].Attachments[0]
	got, _ := io.ReadAll(a.Content)
	if a.Filename != "note.txt" || !bytes.Equal(got, []byte("attachment bytes")) {
		t.Fatalf("attachment=%q %q", a.Filename, got)
	}
}

func TestParseMessagePreservesInlineImageReference(t *testing.T) {
	raw := []byte("Content-Type: multipart/related; boundary=inline\r\n\r\n--inline\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>Hello <img src=\"cid:logo-123\"></p>\r\n--inline\r\nContent-Type: image/png; name=logo.png\r\nContent-ID: <logo-123>\r\nContent-Disposition: inline; filename=logo.png\r\nContent-Transfer-Encoding: base64\r\n\r\naW1hZ2U=\r\n--inline--")
	message, err := parseMessage(raw, "sender@example.test", nil)
	if err != nil {
		t.Fatalf("parseMessage() error = %v", err)
	}
	if message.contentType != "text/html" || !strings.Contains(message.body, "cid:logo-123") {
		t.Fatalf("body = %q (%s), want HTML cid content", message.body, message.contentType)
	}
	if len(message.attachments) != 1 {
		t.Fatalf("attachments = %#v", message.attachments)
	}
	attachment := message.attachments[0]
	if attachment.contentID != "logo-123" || attachment.disposition != "inline" {
		t.Fatalf("inline metadata = %#v", attachment)
	}
}

func TestSMTPSubmissionRequiresAuthentication(t *testing.T) {
	box := &database.Mailbox{ID: "alice"}
	backend := &fakeBackend{authOK: true, mailboxes: map[string]*database.Mailbox{"alice@example.test": box}}
	r, w, conn := newSession(t, backend, "587")
	defer conn.Close()
	_ = command(t, r, w, "EHLO test")
	if got := command(t, r, w, "MAIL FROM:<sender@remote.test>"); got != "530 5.7.0 Authentication required" {
		t.Fatal(got)
	}
	encoded := "AGFsaWNlQGV4YW1wbGUudGVzdABzZWNyZXQ="
	if got := command(t, r, w, "AUTH PLAIN "+encoded); !strings.HasPrefix(got, "235") {
		t.Fatal(got)
	}
	if got := command(t, r, w, "MAIL FROM:<sender@remote.test>"); !strings.HasPrefix(got, "250") {
		t.Fatal(got)
	}
}

func TestSMTPStartAndShutdown(t *testing.T) {
	backend := &fakeBackend{mailboxes: map[string]*database.Mailbox{}}
	s, err := New(config.ListenerConfig{Enabled: true, Host: "127.0.0.1", Port: "0", TLSMode: "disabled"}, "example.test", backend)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		if strings.Contains(err.Error(), "operation not permitted") {
			t.Skip("network listeners are blocked by this sandbox")
		}
		t.Fatal(err)
	}
	if s.Addr() == nil {
		t.Fatal("listener was not started")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSMTPTLSModes(t *testing.T) {
	certFile, keyFile := testCertificate(t)
	backend := &fakeBackend{mailboxes: map[string]*database.Mailbox{}}
	for _, mode := range []string{"starttls", "implicit"} {
		t.Run(mode, func(t *testing.T) {
			s, err := New(config.ListenerConfig{Enabled: true, Port: "25", TLSMode: mode, CertFile: certFile, KeyFile: keyFile}, "example.test", backend)
			if err != nil {
				t.Fatal(err)
			}
			client, server := net.Pipe()
			go s.serve(server)
			if mode == "starttls" {
				r, w := bufio.NewReader(client), bufio.NewWriter(client)
				if got := readReply(t, r); !strings.HasPrefix(got, "220") {
					t.Fatal(got)
				}
				if got := command(t, r, w, "STARTTLS"); !strings.HasPrefix(got, "220") {
					t.Fatal(got)
				}
				secure := tls.Client(client, &tls.Config{InsecureSkipVerify: true}) // #nosec G402 -- test-only self-signed certificate.
				if err := secure.Handshake(); err != nil {
					t.Fatal(err)
				}
				secureReader, secureWriter := bufio.NewReader(secure), bufio.NewWriter(secure)
				if got := command(t, secureReader, secureWriter, "EHLO test"); !strings.HasPrefix(got, "250") {
					t.Fatal(got)
				}
				_ = secure.Close()
				return
			}
			secure := tls.Client(client, &tls.Config{InsecureSkipVerify: true}) // #nosec G402 -- test-only self-signed certificate.
			if err := secure.Handshake(); err != nil {
				t.Fatal(err)
			}
			if got := readReply(t, bufio.NewReader(secure)); !strings.HasPrefix(got, "220") {
				t.Fatal(got)
			}
			_ = secure.Close()
		})
	}
}

func testCertificate(t *testing.T) (string, string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"localhost"}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile := dir+"/cert.pem", dir+"/key.pem"
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}
