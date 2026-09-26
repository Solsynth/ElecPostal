package senderauth

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// fakeResolver is the in-memory DNS stub: TXT (with per-name errors and an
// IsNotFound default), A/AAAA and MX.
type fakeResolver struct {
	txt    map[string][]string
	txtErr map[string]error
	ip     map[string][]net.IP
	mx     map[string][]*net.MX
	calls  map[string]int
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{
		txt:    map[string][]string{},
		txtErr: map[string]error{},
		ip:     map[string][]net.IP{},
		mx:     map[string][]*net.MX{},
		calls:  map[string]int{},
	}
}

func (f *fakeResolver) LookupTXT(ctx context.Context, name string) ([]string, error) {
	f.calls[name]++
	if err, ok := f.txtErr[name]; ok {
		return nil, err
	}
	if values, ok := f.txt[name]; ok {
		return values, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (f *fakeResolver) LookupIP(ctx context.Context, network, host string) ([]net.IP, error) {
	if values, ok := f.ip[host]; ok {
		return values, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func (f *fakeResolver) LookupMX(ctx context.Context, name string) ([]*net.MX, error) {
	if values, ok := f.mx[name]; ok {
		return values, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func newTestVerifier(t *testing.T, f *fakeResolver, timeout time.Duration) *verifier {
	t.Helper()
	v, ok := NewVerifier(Config{Resolver: f, Timeout: timeout}).(*verifier)
	if !ok {
		t.Fatal("NewVerifier returned an unexpected concrete type")
	}
	return v
}

func rawMessage(from, body string) []byte {
	return []byte("From: " + from + "\r\nTo: bob@recipient.test\r\nSubject: Hi\r\n\r\n" + body)
}

func hasWarning(warnings []string, want string) bool {
	for _, w := range warnings {
		if w == want {
			return true
		}
	}
	return false
}

func TestHostOnly(t *testing.T) {
	cases := map[string]string{
		"192.0.2.1:2525":     "192.0.2.1",
		"192.0.2.1":          "192.0.2.1",
		"[2001:db8::1]:2525": "2001:db8::1",
		"2001:db8::1":        "2001:db8::1",
	}
	for in, want := range cases {
		if got := hostOnly(in); got != want {
			t.Errorf("hostOnly(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestVerifyWarningsSPFFail pins the documented warning string.
func TestVerifyWarningsSPFFail(t *testing.T) {
	f := newFakeResolver()
	f.txt["sender.example"] = []string{"v=spf1 -all"}
	v := newTestVerifier(t, f, 0)

	res := v.Verify(context.Background(), rawMessage("Bob <bob@sender.example>", "hi"), "a@sender.example", "192.0.2.1")
	if res.SPF != "fail" {
		t.Fatalf("SPF = %q, want fail", res.SPF)
	}
	if !hasWarning(res.Warnings, "Possible phishing: SPF verification failed") {
		t.Fatalf("warnings = %v, want SPF phishing warning", res.Warnings)
	}
}

// TestVerifyDMARCWarnings pins the documented policy warning strings.
func TestVerifyDMARCWarnings(t *testing.T) {
	for _, tc := range []struct {
		policy string
		want   string
	}{
		{"reject", "Domain policy rejects unauthenticated mail"},
		{"quarantine", "Domain policy quarantines unauthenticated mail"},
	} {
		f := newFakeResolver()
		f.txt["_dmarc.example.com"] = []string{"v=DMARC1; p=" + tc.policy}
		v := newTestVerifier(t, f, 0)

		res := v.Verify(context.Background(), rawMessage("Alice <alice@example.com>", "hi"), "bounce@other.test", "192.0.2.1")
		if res.DMARC != tc.policy {
			t.Fatalf("p=%s: DMARC = %q", tc.policy, res.DMARC)
		}
		if !hasWarning(res.Warnings, tc.want) {
			t.Fatalf("p=%s: warnings = %v, want %q", tc.policy, res.Warnings, tc.want)
		}
	}
}

// TestVerifyTimeoutTemperror: an expired budget turns every mechanism into
// temperror and never blocks.
func TestVerifyTimeoutTemperror(t *testing.T) {
	v := newTestVerifier(t, newFakeResolver(), time.Nanosecond)
	res := v.Verify(context.Background(), rawMessage("a@example.com", "hi"), "a@example.com", "192.0.2.1")
	if res.SPF != "temperror" || res.DKIM != "temperror" || res.DMARC != "temperror" {
		t.Fatalf("expired budget: got spf=%q dkim=%q dmarc=%q, want all temperror", res.SPF, res.DKIM, res.DMARC)
	}
}

// TestVerifyPeerPortTolerated: a host:port peer address still matches ip4.
func TestVerifyPeerPortTolerated(t *testing.T) {
	f := newFakeResolver()
	f.txt["sender.example"] = []string{"v=spf1 ip4:192.0.2.0/24 -all"}
	v := newTestVerifier(t, f, 0)

	res := v.Verify(context.Background(), rawMessage("Bob <bob@sender.example>", "hi"), "a@sender.example", "192.0.2.10:2525")
	if res.SPF != "pass" {
		t.Fatalf("SPF = %q, want pass with host:port peer", res.SPF)
	}
}

// TestTXTCache: a resolved name is not looked up twice; a "no record" answer is
// cached negatively.
func TestTXTCache(t *testing.T) {
	f := newFakeResolver()
	f.txt["sender.example"] = []string{"v=spf1 ip4:192.0.2.0/24 -all"}
	v := newTestVerifier(t, f, 0)

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if got := v.verifySPF(ctx, "a@sender.example", "192.0.2.10"); got != "pass" {
			t.Fatalf("lookup %d: SPF = %q", i, got)
		}
	}
	if f.calls["sender.example"] != 1 {
		t.Fatalf("TXT lookups = %d, want 1 (positive cache)", f.calls["sender.example"])
	}

	for i := 0; i < 3; i++ {
		if got := v.verifySPF(ctx, "a@absent.example", "192.0.2.10"); got != "none" {
			t.Fatalf("lookup %d: SPF = %q, want none", i, got)
		}
	}
	if f.calls["absent.example"] != 1 {
		t.Fatalf("TXT lookups = %d, want 1 (negative cache)", f.calls["absent.example"])
	}
}

// TestDNSFailureNotCached: transient errors are retried.
func TestDNSFailureNotCached(t *testing.T) {
	f := newFakeResolver()
	f.txtErr["sender.example"] = errors.New("temporary failure")
	v := newTestVerifier(t, f, 0)

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if got := v.verifySPF(ctx, "a@sender.example", "192.0.2.1"); got != "temperror" {
			t.Fatalf("lookup %d: SPF = %q, want temperror", i, got)
		}
	}
	if f.calls["sender.example"] != 3 {
		t.Fatalf("TXT lookups = %d, want 3 (errors not cached)", f.calls["sender.example"])
	}
}
