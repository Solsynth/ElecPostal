package senderauth

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"net/mail"
	"net/textproto"
	"strings"
)

// dkimSignature is the subset of a DKIM-Signature header the verifier needs.
type dkimSignature struct {
	algo      string // a=
	domain    string // d=
	selector  string // s=
	canon     string // c=
	headers   string // h=
	bodyHash  string // bh=
	signature string // b=
}

// parseDKIMSignature returns nil when the signature is missing required tags
// or declares a version other than 1 (RFC 6376 §3.5).
func parseDKIMSignature(value string) *dkimSignature {
	sig := &dkimSignature{}
	for _, part := range strings.Split(value, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		eq := strings.Index(part, "=")
		if eq < 0 {
			continue
		}
		tag := strings.ToLower(strings.TrimSpace(part[:eq]))
		val := strings.TrimSpace(part[eq+1:])
		switch tag {
		case "v":
			if val != "1" {
				return nil
			}
		case "a":
			sig.algo = strings.ToLower(val)
		case "d":
			sig.domain = strings.ToLower(val)
		case "s":
			sig.selector = val
		case "c":
			sig.canon = strings.ToLower(val)
		case "h":
			sig.headers = val
		case "bh":
			sig.bodyHash = val
		case "b":
			sig.signature = val
		}
	}
	if sig.domain == "" || sig.selector == "" || sig.headers == "" || sig.bodyHash == "" || sig.signature == "" {
		return nil
	}
	return sig
}

// verifyDKIM verifies every DKIM-Signature header and returns the aggregate
// result plus the last valid signature's d= domain (for DMARC alignment).
//
// Aggregation: any valid signature wins; else a DNS failure yields temperror;
// else a signature that was verifiable but did not match yields fail; else
// permerror (all signatures malformed, unsupported algorithm, or no key).
func (v *verifier) verifyDKIM(ctx context.Context, msg *mail.Message, rawBody []byte) (string, string) {
	sigValues := headerValues(msg.Header, "DKIM-Signature")
	if len(sigValues) == 0 {
		return "none", ""
	}

	var (
		valid      bool
		lastValidD string
		dnsFail    bool
		verifyFail bool
	)

	for _, value := range sigValues {
		if ctx.Err() != nil {
			dnsFail = true
			break
		}
		sig := parseDKIMSignature(value)
		if sig == nil {
			continue // malformed: skipped
		}
		if !supportedHash(sig.algo) {
			continue // unsupported a=: permerror unless another signature wins
		}

		key, dnsErr, missingKey := v.dkimKey(ctx, sig.selector, sig.domain)
		if dnsErr {
			dnsFail = true
			continue
		}
		if missingKey {
			continue
		}

		bodyDigest := hashBytes(sig.algo, canonicalizeBody(rawBody, bodyRelaxed(sig.canon)))
		wantBodyHash, err := base64.StdEncoding.DecodeString(sig.bodyHash)
		if err != nil {
			continue // malformed bh=: skipped
		}
		if subtle.ConstantTimeCompare(bodyDigest, wantBodyHash) != 1 {
			verifyFail = true
			continue
		}

		headerDigest := hashBytes(sig.algo, canonicalizeSignedHeaders(msg.Header, sig.headers, headerRelaxed(sig.canon)))
		sigBytes, err := base64.StdEncoding.DecodeString(sig.signature)
		if err != nil {
			continue // malformed b=: skipped
		}
		if !verifySignature(key, sig.algo, headerDigest, sigBytes) {
			verifyFail = true
			continue
		}
		valid = true
		lastValidD = sig.domain
	}

	switch {
	case valid:
		return "pass", lastValidD
	case dnsFail:
		return "temperror", ""
	case verifyFail:
		return "fail", ""
	default:
		// permerror covers unsupported algorithms, missing/absent keys and
		// the all-signatures-malformed case.
		return "permerror", ""
	}
}

// dkimKey fetches and parses the selector's public key. dnsErr reports a DNS
// failure; missing reports DNS success without a usable p= key.
func (v *verifier) dkimKey(ctx context.Context, selector, domain string) (key *dkimKey, dnsErr, missing bool) {
	records, err := v.lookupTXT(ctx, selector+"._domainkey."+domain)
	if err != nil {
		if isNotFound(err) {
			return nil, false, true
		}
		return nil, true, false
	}
	parsed := parseDKIMKey(records)
	if parsed == nil || len(parsed.p) == 0 {
		return nil, false, true
	}
	return parsed, false, false
}

// dkimKey is a parsed DKIM1 DNS record.
type dkimKey struct {
	k string // k=
	p []byte // decoded p=
}

// parseDKIMKey returns the first usable DKIM1 record, or nil. A record whose
// v= is present but not DKIM1 is ignored; k= defaults to rsa.
func parseDKIMKey(records []string) *dkimKey {
	for _, record := range records {
		record = strings.TrimSpace(record)
		if record == "" {
			continue
		}
		var key dkimKey
		for _, part := range strings.Split(record, ";") {
			part = strings.TrimSpace(part)
			eq := strings.Index(part, "=")
			if eq < 0 {
				continue
			}
			tag := strings.ToLower(strings.TrimSpace(part[:eq]))
			val := strings.TrimSpace(part[eq+1:])
			switch tag {
			case "v":
				if !strings.EqualFold(val, "DKIM1") {
					return nil
				}
			case "k":
				key.k = strings.ToLower(val)
			case "p":
				decoded, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(val), ""))
				if err != nil {
					return &dkimKey{}
				}
				key.p = decoded
			}
		}
		if len(key.p) == 0 {
			return &dkimKey{}
		}
		if key.k == "" {
			key.k = "rsa"
		}
		return &key
	}
	return nil
}

// hashAlgo extracts the hash algorithm from an a= value such as
// "rsa-sha256" or "ed25519-sha256"; empty defaults to sha256.
func hashAlgo(algo string) string {
	algo = strings.ToLower(strings.TrimSpace(algo))
	if algo == "" {
		return "sha256"
	}
	if i := strings.LastIndex(algo, "-"); i >= 0 {
		return algo[i+1:]
	}
	return algo
}

func supportedHash(algo string) bool {
	switch hashAlgo(algo) {
	case "sha1", "sha256":
		return true
	}
	return false
}

func hashBytes(algo string, data []byte) []byte {
	if hashAlgo(algo) == "sha1" {
		sum := sha1.Sum(data)
		return sum[:]
	}
	sum := sha256.Sum256(data)
	return sum[:]
}

func hashFunc(algo string) crypto.Hash {
	if hashAlgo(algo) == "sha1" {
		return crypto.SHA1
	}
	return crypto.SHA256
}

func verifySignature(key *dkimKey, algo string, digest, sig []byte) bool {
	if key.k == "ed25519" {
		if len(key.p) != ed25519.PublicKeySize {
			return false
		}
		return ed25519.Verify(ed25519.PublicKey(key.p), digest, sig)
	}
	pub := parseRSAPublicKey(key.p)
	if pub == nil {
		return false
	}
	return rsa.VerifyPKCS1v15(pub, hashFunc(algo), digest, sig) == nil
}

// parseRSAPublicKey accepts both PKCS#1 and PKIX DER encodings, which
// different signers publish.
func parseRSAPublicKey(der []byte) *rsa.PublicKey {
	if pk, err := x509.ParsePKCS1PublicKey(der); err == nil {
		return pk
	}
	if pk, err := x509.ParsePKIXPublicKey(der); err == nil {
		if rsaPub, ok := pk.(*rsa.PublicKey); ok {
			return rsaPub
		}
	}
	return nil
}

// headerValues returns every instance of a header in message order
// (net/mail.Header is a plain map; textproto adds canonicalized lookup).
func headerValues(header mail.Header, name string) []string {
	return textproto.MIMEHeader(header).Values(name)
}

// canonicalizeSignedHeaders hashes the headers listed in h=, in order, with
// every instance of each header appended (RFC 6376 §3.7). The dkim-signature
// header, when signed, uses an emptied b= value.
func canonicalizeSignedHeaders(header mail.Header, list string, relaxed bool) []byte {
	var buf bytes.Buffer
	for _, name := range strings.Split(list, ":") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		for _, value := range headerValues(header, name) {
			if strings.EqualFold(name, "dkim-signature") {
				value = stripDKIMB(value)
			}
			buf.WriteString(canonicalizeHeader(name, value, relaxed))
		}
	}
	return buf.Bytes()
}

func canonicalizeHeader(name, value string, relaxed bool) string {
	if !relaxed {
		return name + ":" + value + "\r\n"
	}
	return strings.ToLower(name) + ":" + collapseWSP(strings.TrimSpace(value)) + "\r\n"
}

// stripDKIMB replaces the b= tag's value with an empty value, preserving the
// tag's surrounding whitespace so the reconstructed header matches what the
// signer hashed (RFC 6376 §3.7).
func stripDKIMB(value string) string {
	parts := strings.Split(value, ";")
	for i, part := range parts {
		trimmed := strings.TrimLeft(part, " \t")
		if !strings.HasPrefix(strings.ToLower(trimmed), "b=") {
			continue
		}
		parts[i] = part[:len(part)-len(trimmed)] + "b="
	}
	return strings.Join(parts, ";")
}

func headerRelaxed(canon string) bool {
	parts := strings.SplitN(strings.ToLower(canon), "/", 2)
	return parts[0] == "relaxed"
}

func bodyRelaxed(canon string) bool {
	parts := strings.SplitN(strings.ToLower(canon), "/", 2)
	return len(parts) > 1 && parts[1] == "relaxed"
}

// canonicalizeBody applies RFC 6376 §3.4.3 (simple) or §3.4.4 (relaxed).
func canonicalizeBody(body []byte, relaxed bool) []byte {
	if relaxed {
		return relaxedBody(body)
	}
	return simpleBody(body)
}

// simpleBody deletes trailing line terminators and appends a single CRLF to a
// non-empty body.
func simpleBody(body []byte) []byte {
	trimmed := bytes.TrimRight(body, "\r\n")
	if len(trimmed) == 0 {
		return nil
	}
	out := make([]byte, 0, len(trimmed)+2)
	out = append(out, trimmed...)
	return append(out, '\r', '\n')
}

// relaxedBody collapses WSP runs, strips per-line trailing WSP, ignores
// trailing empty lines and appends a single CRLF to a non-empty body.
func relaxedBody(body []byte) []byte {
	lines := splitLines(body)
	for len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return nil
	}
	var out bytes.Buffer
	for i, line := range lines {
		if i > 0 {
			out.WriteString("\r\n")
		}
		out.WriteString(collapseWSP(string(bytes.TrimRight(line, " \t"))))
	}
	out.WriteString("\r\n")
	return out.Bytes()
}

func splitLines(body []byte) [][]byte {
	raw := bytes.Split(body, []byte{'\n'})
	lines := make([][]byte, len(raw))
	for i, line := range raw {
		lines[i] = bytes.TrimSuffix(line, []byte{'\r'})
	}
	return lines
}

// collapseWSP replaces every run of spaces/tabs with a single space.
func collapseWSP(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	pendingSpace := false
	for i := 0; i < len(s); i++ {
		if c := s[i]; c == ' ' || c == '\t' {
			pendingSpace = true
		} else {
			if pendingSpace {
				b.WriteByte(' ')
				pendingSpace = false
			}
			b.WriteByte(c)
		}
	}
	return b.String()
}
