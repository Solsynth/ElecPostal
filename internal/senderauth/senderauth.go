// Package senderauth verifies SPF, DKIM and DMARC for inbound SMTP messages at
// DATA time. It runs on the SMTP thread, so it is fast, bounded by a per-message
// timeout, and fail-soft: Verify never returns an error, only a Result whose
// fields are pass/fail/softfail/none/temperror/permerror (plus DMARC policy
// reject/quarantine/allow).
package senderauth

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/mail"
	"strings"
	"sync"
	"time"
)

// Verifier authenticates a message's sender using DNS records.
type Verifier interface {
	// Verify checks raw (an RFC 5322 message) against SPF, DKIM and DMARC.
	// envelopeFrom is the SMTP MAIL FROM; peerIP the connecting client's IP
	// (host:port tolerated). Failures degrade to none/temperror — never an
	// error return.
	Verify(ctx context.Context, raw []byte, envelopeFrom, peerIP string) Result
}

// Result is the JSON-serializable outcome stored in Email.Authentication.
type Result struct {
	SPF      string   `json:"spf"`
	DKIM     string   `json:"dkim"`
	DMARC    string   `json:"dmarc"`
	Warnings []string `json:"warnings"`
}

// TXTLookuper resolves DNS TXT records. *net.Resolver satisfies it; tests
// inject a stub.
type TXTLookuper interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

// ipLookuper resolves A/AAAA and MX records for the SPF a/mx mechanisms. A
// *net.Resolver satisfies it; a TXT stub may implement it too for tests.
type ipLookuper interface {
	LookupIP(ctx context.Context, network, host string) ([]net.IP, error)
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
}

// Config controls a verifier.
type Config struct {
	Resolver TXTLookuper   // nil falls back to net.DefaultResolver
	Timeout  time.Duration // per-message budget; 0 defaults to 2s
}

// DefaultTimeout bounds a single Verify call.
const DefaultTimeout = 2 * time.Second

// TXT cache parameters: fixed positive/negative TTLs (LookupTXT exposes no
// TTL) and a FIFO cap so an adversarial domain list cannot grow the map.
const (
	txtTTL         = 300 * time.Second
	txtNegativeTTL = 60 * time.Second
	cacheMax       = 1024
)

type cacheEntry struct {
	values  []string
	err     error
	expires time.Time
}

type verifier struct {
	resolver   TXTLookuper
	ipResolver ipLookuper
	timeout    time.Duration

	cacheMu    sync.Mutex
	cache      map[string]cacheEntry
	cacheOrder []string
}

// NewVerifier returns a Verifier. A nil Config.Resolver uses
// net.DefaultResolver (the app passes relay.NewDNSResolver's *net.Resolver,
// which also serves the SPF a/mx IP lookups).
func NewVerifier(cfg Config) Verifier {
	v := &verifier{
		resolver: cfg.Resolver,
		timeout:  cfg.Timeout,
		cache:    make(map[string]cacheEntry),
	}
	if v.resolver == nil {
		v.resolver = net.DefaultResolver
	}
	if r, ok := v.resolver.(ipLookuper); ok {
		v.ipResolver = r
	} else {
		v.ipResolver = net.DefaultResolver
	}
	return v
}

func (v *verifier) budget(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := v.timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

// Verify implements Verifier. Order: SPF, DKIM, DMARC. When the budget
// expires mid-flight, every not-yet-computed result becomes "temperror"; the
// call never outlives the budget.
func (v *verifier) Verify(ctx context.Context, raw []byte, envelopeFrom, peerIP string) Result {
	ctx, cancel := v.budget(ctx)
	defer cancel()
	peerIP = hostOnly(peerIP)

	res := Result{SPF: "none", DKIM: "none", DMARC: "none"}

	if ctx.Err() != nil {
		res.SPF = "temperror"
	} else {
		res.SPF = v.verifySPF(ctx, envelopeFrom, peerIP)
		if res.SPF == "fail" {
			res.Warnings = append(res.Warnings, "Possible phishing: SPF verification failed")
		}
	}

	msg, body := parseMessage(raw)
	var dkimD string
	switch {
	case ctx.Err() != nil:
		res.DKIM = "temperror"
	case msg == nil:
		res.DKIM = "none"
	default:
		res.DKIM, dkimD = v.verifyDKIM(ctx, msg, body)
		if res.DKIM == "fail" {
			res.Warnings = append(res.Warnings, "Possible forgery: DKIM signature is invalid")
		}
	}

	if ctx.Err() != nil {
		res.DMARC = "temperror"
	} else if msg != nil {
		fromDomain := fromDomainOf(msg)
		res.DMARC = v.verifyDMARC(ctx, fromDomain, domainOf(envelopeFrom), res.SPF, res.DKIM, dkimD)
		switch res.DMARC {
		case "reject":
			res.Warnings = append(res.Warnings, "Domain policy rejects unauthenticated mail")
		case "quarantine":
			res.Warnings = append(res.Warnings, "Domain policy quarantines unauthenticated mail")
		}
	}

	return res
}

// parseMessage reads raw as a mail message and returns the parsed header
// object plus the raw body bytes (needed for DKIM body canonicalization).
// A nil message means the header block could not be parsed.
func parseMessage(raw []byte) (*mail.Message, []byte) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, nil
	}
	body, err := io.ReadAll(msg.Body)
	if err != nil {
		return nil, nil
	}
	return msg, body
}

// fromDomainOf extracts the From header's address domain (lowercased), or ""
// when absent.
func fromDomainOf(msg *mail.Message) string {
	addrs, err := msg.Header.AddressList("From")
	if err != nil || len(addrs) == 0 {
		return ""
	}
	return domainOf(addrs[0].Address)
}

// domainOf returns the lowercased domain part of an address, "" when absent.
func domainOf(addr string) string {
	addr = strings.TrimSpace(addr)
	if i := strings.LastIndex(addr, "<"); i >= 0 {
		addr = addr[i+1:]
	}
	addr = strings.TrimSuffix(addr, ">")
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		return strings.ToLower(strings.TrimSpace(addr[i+1:]))
	}
	return ""
}

// hostOnly strips a ":port" suffix (net.Addr.String form) from a peer IP.
func hostOnly(s string) string {
	if h, _, err := net.SplitHostPort(s); err == nil {
		return h
	}
	return s
}

// isNotFound reports whether err is a DNS "no such host" error (NXDOMAIN or
// NODATA), which callers treat as "no record" rather than "DNS failure".
func isNotFound(err error) bool {
	var dnsErr *net.DNSError
	return err != nil && errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

// lookupTXT resolves name through the resolver with a bounded FIFO cache.
// Positive results live txtTTL, "no record" (IsNotFound) results
// txtNegativeTTL; transient errors are not cached.
func (v *verifier) lookupTXT(ctx context.Context, name string) ([]string, error) {
	now := time.Now()
	v.cacheMu.Lock()
	if e, ok := v.cache[name]; ok && now.Before(e.expires) {
		v.cacheMu.Unlock()
		return e.values, e.err
	}
	v.cacheMu.Unlock()

	values, err := v.resolver.LookupTXT(ctx, name)

	var ttl time.Duration
	switch {
	case err == nil:
		ttl = txtTTL
	case isNotFound(err):
		ttl = txtNegativeTTL
	default:
		return values, err
	}

	v.cacheMu.Lock()
	if _, ok := v.cache[name]; !ok {
		if len(v.cache) >= cacheMax {
			oldest := v.cacheOrder[0]
			delete(v.cache, oldest)
			v.cacheOrder = v.cacheOrder[1:]
		}
		v.cacheOrder = append(v.cacheOrder, name)
	}
	v.cache[name] = cacheEntry{values: values, err: err, expires: now.Add(ttl)}
	v.cacheMu.Unlock()
	return values, err
}
