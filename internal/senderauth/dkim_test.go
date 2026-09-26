package senderauth

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"hash"
	"strings"
	"sync"
	"testing"
)

const (
	dkimDomain   = "example.com"
	dkimSelector = "sel"
)

var (
	dkimKeyOnce sync.Once
	dkimTestKey *rsa.PrivateKey
)

func testDKIMKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	dkimKeyOnce.Do(func() {
		if k, err := rsa.GenerateKey(rand.Reader, 2048); err == nil {
			dkimTestKey = k
		}
	})
	if dkimTestKey == nil {
		t.Fatal("could not generate DKIM test key")
	}
	return dkimTestKey
}

// testRelaxedHeader implements RFC 6376 §3.4.2 relaxed header canonicalization
// independently of the package implementation.
func testRelaxedHeader(name, value string) string {
	return strings.ToLower(name) + ":" + strings.Join(strings.Fields(value), " ") + "\r\n"
}

// testRelaxedBody implements RFC 6376 §3.4.4 relaxed body canonicalization
// independently of the package implementation.
func testRelaxedBody(body string) []byte {
	lines := strings.Split(body, "\r\n")
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return nil
	}
	canon := make([]string, len(lines))
	for i, line := range lines {
		canon[i] = strings.Join(strings.Fields(line), " ")
	}
	return []byte(strings.Join(canon, "\r\n") + "\r\n")
}

func testHash(hashName string) hash.Hash {
	if hashName == "sha1" {
		return sha1.New()
	}
	return sha256.New()
}

func testCryptoHash(hashName string) crypto.Hash {
	if hashName == "sha1" {
		return crypto.SHA1
	}
	return crypto.SHA256
}

type signedFixture struct {
	raw        []byte
	pubB64     string
	signHeader string
}

// signMessage builds an RFC 5322 message plus a relaxed/relaxed DKIM-Signature
// over from/to/subject (and, when includeDKIMInH is set, the DKIM-Signature
// header itself with an emptied b=), hashed with hashName.
func signMessage(t *testing.T, body string, includeDKIMInH bool, hashName string) signedFixture {
	t.Helper()
	key := testDKIMKey(t)

	headers := [][2]string{
		{"From", "Alice <alice@" + dkimDomain + ">"},
		{"To", "bob@recipient.test"},
		{"Subject", "Quarterly report"},
	}

	bodyHasher := testHash(hashName)
	bodyHasher.Write(testRelaxedBody(body))
	bh := base64.StdEncoding.EncodeToString(bodyHasher.Sum(nil))

	hList := "from:to:subject"
	if includeDKIMInH {
		hList = "from:to:subject:dkim-signature"
	}
	emptySig := "DKIM-Signature: v=1; a=rsa-" + hashName + "; c=relaxed/relaxed; d=" + dkimDomain +
		"; s=" + dkimSelector + "; h=" + hList + "; bh=" + bh + "; b="

	var headerInput strings.Builder
	for _, h := range headers {
		headerInput.WriteString(testRelaxedHeader(h[0], h[1]))
	}
	if includeDKIMInH {
		headerInput.WriteString(testRelaxedHeader("DKIM-Signature", emptySig[len("DKIM-Signature:"):]))
	}
	headerHasher := testHash(hashName)
	headerHasher.Write([]byte(headerInput.String()))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, testCryptoHash(hashName), headerHasher.Sum(nil))
	if err != nil {
		t.Fatal(err)
	}
	signHeader := emptySig + base64.StdEncoding.EncodeToString(sig)

	var msg strings.Builder
	for _, h := range headers {
		msg.WriteString(h[0] + ": " + h[1] + "\r\n")
	}
	msg.WriteString(signHeader + "\r\n\r\n")
	msg.WriteString(body)

	return signedFixture{
		raw:        []byte(msg.String()),
		pubB64:     base64.StdEncoding.EncodeToString(x509.MarshalPKCS1PublicKey(&key.PublicKey)),
		signHeader: signHeader,
	}
}

func dkimResolver(f *fakeResolver, pubB64 string) *fakeResolver {
	f.txt[dkimSelector+"._domainkey."+dkimDomain] = []string{"v=DKIM1; k=rsa; p=" + pubB64}
	return f
}

func TestDKIMPass(t *testing.T) {
	for _, hashName := range []string{"sha256", "sha1"} {
		t.Run(hashName, func(t *testing.T) {
			fix := signMessage(t, "Hello, this is the quarterly report.\r\n\r\nRegards,\r\nAlice", false, hashName)
			v := newTestVerifier(t, dkimResolver(newFakeResolver(), fix.pubB64), 0)

			res := v.Verify(context.Background(), fix.raw, "bounce@"+dkimDomain, "192.0.2.1")
			if res.DKIM != "pass" {
				t.Fatalf("DKIM = %q, warnings %v", res.DKIM, res.Warnings)
			}
		})
	}
}

// TestDKIMSignsOwnHeader covers the RFC 6376 §3.7 case where h= includes
// dkim-signature and the b= value must be emptied before hashing.
func TestDKIMSignsOwnHeader(t *testing.T) {
	fix := signMessage(t, "Body text for the signed header case.", true, "sha256")
	v := newTestVerifier(t, dkimResolver(newFakeResolver(), fix.pubB64), 0)

	res := v.Verify(context.Background(), fix.raw, "bounce@"+dkimDomain, "192.0.2.1")
	if res.DKIM != "pass" {
		t.Fatalf("DKIM = %q, warnings %v", res.DKIM, res.Warnings)
	}
}

// TestDKIMBodyTampered: flipping a body byte breaks bh= and yields fail with
// the documented warning.
func TestDKIMBodyTampered(t *testing.T) {
	fix := signMessage(t, "Hello, this is the quarterly report.\r\n\r\nRegards,\r\nAlice", false, "sha256")
	mutated := bytes.Replace(fix.raw, []byte("quarterly report"), []byte("quarterly REPORT"), 1)
	if bytes.Equal(mutated, fix.raw) {
		t.Fatal("test setup: body byte not replaced")
	}
	v := newTestVerifier(t, dkimResolver(newFakeResolver(), fix.pubB64), 0)

	res := v.Verify(context.Background(), mutated, "bounce@"+dkimDomain, "192.0.2.1")
	if res.DKIM != "fail" {
		t.Fatalf("DKIM = %q, want fail", res.DKIM)
	}
	if !hasWarning(res.Warnings, "Possible forgery: DKIM signature is invalid") {
		t.Fatalf("warnings = %v, want DKIM forgery warning", res.Warnings)
	}
}

// TestDKIMSignatureTampered: a corrupted b= payload fails the RSA check
// (a mid-string base64 edit keeps the encoding valid so the failure is
// cryptographic, not a decode error).
func TestDKIMSignatureTampered(t *testing.T) {
	fix := signMessage(t, "Hello.", false, "sha256")
	i := len(fix.signHeader) / 2
	replacement := byte('A')
	if fix.signHeader[i] == replacement {
		replacement = 'B'
	}
	tamperedHeader := fix.signHeader[:i] + string(replacement) + fix.signHeader[i+1:]
	mutated := bytes.Replace(fix.raw, []byte(fix.signHeader), []byte(tamperedHeader), 1)
	v := newTestVerifier(t, dkimResolver(newFakeResolver(), fix.pubB64), 0)

	res := v.Verify(context.Background(), mutated, "bounce@"+dkimDomain, "192.0.2.1")
	if res.DKIM != "fail" {
		t.Fatalf("DKIM = %q, want fail", res.DKIM)
	}
}

func TestDKIMNoSignature(t *testing.T) {
	v := newTestVerifier(t, newFakeResolver(), 0)
	res := v.Verify(context.Background(), rawMessage("a@"+dkimDomain, "hi"), "a@"+dkimDomain, "192.0.2.1")
	if res.DKIM != "none" {
		t.Fatalf("DKIM = %q, want none", res.DKIM)
	}
}

func TestDKIMMalformed(t *testing.T) {
	cases := map[string]string{
		"empty b":        "DKIM-Signature: v=1; a=rsa-sha256; c=relaxed/relaxed; d=example.com; s=sel; h=from; bh=YWJj; b=",
		"invalid base64": "DKIM-Signature: v=1; a=rsa-sha256; c=relaxed/relaxed; d=example.com; s=sel; h=from; bh=YWJj; b=!!!!",
		"missing bh":     "DKIM-Signature: v=1; a=rsa-sha256; c=relaxed/relaxed; d=example.com; s=sel; h=from; b=YWJj",
		"bad version":    "DKIM-Signature: v=2; a=rsa-sha256; c=relaxed/relaxed; d=example.com; s=sel; h=from; bh=YWJj; b=YWJj",
		"unsupported a=": "DKIM-Signature: v=1; a=rsa-md5; c=relaxed/relaxed; d=example.com; s=sel; h=from; bh=YWJj; b=YWJj",
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			raw := []byte("From: a@example.com\r\nSubject: hi\r\n" + header + "\r\n\r\nbody")
			v := newTestVerifier(t, dkimResolver(newFakeResolver(), "unused"), 0)
			res := v.Verify(context.Background(), raw, "a@example.com", "192.0.2.1")
			if res.DKIM != "permerror" {
				t.Fatalf("DKIM = %q, want permerror", res.DKIM)
			}
		})
	}
}

// TestDKIMNoKeyRecord: DNS answers but publishes no p= key.
func TestDKIMNoKeyRecord(t *testing.T) {
	fix := signMessage(t, "Hello.", false, "sha256")
	f := newFakeResolver()
	f.txt[dkimSelector+"._domainkey."+dkimDomain] = []string{"v=DKIM1; k=rsa"}
	v := newTestVerifier(t, f, 0)

	res := v.Verify(context.Background(), fix.raw, "bounce@"+dkimDomain, "192.0.2.1")
	if res.DKIM != "permerror" {
		t.Fatalf("DKIM = %q, want permerror (no p= key)", res.DKIM)
	}
}

// TestDKIMDNSFailure: a resolver failure on the selector lookup is temporary.
func TestDKIMDNSFailure(t *testing.T) {
	fix := signMessage(t, "Hello.", false, "sha256")
	f := newFakeResolver()
	f.txtErr[dkimSelector+"._domainkey."+dkimDomain] = errors.New("boom")
	v := newTestVerifier(t, f, 0)

	res := v.Verify(context.Background(), fix.raw, "bounce@"+dkimDomain, "192.0.2.1")
	if res.DKIM != "temperror" {
		t.Fatalf("DKIM = %q, want temperror", res.DKIM)
	}
}

// TestDKIMOneValidOneBad: two signatures over different bodies; the stale one
// fails bh= while the good one still verifies, so pass wins.
func TestDKIMOneValidOneBad(t *testing.T) {
	good := signMessage(t, "Good body.", false, "sha256")
	bad := signMessage(t, "Bad body.", false, "sha256")
	raw := append([]byte{}, good.raw...)
	raw = bytes.Replace(raw, []byte("\r\n\r\n"), []byte("\r\n"+bad.signHeader+"\r\n\r\n"), 1)

	v := newTestVerifier(t, dkimResolver(newFakeResolver(), good.pubB64), 0)
	res := v.Verify(context.Background(), raw, "bounce@"+dkimDomain, "192.0.2.1")
	if res.DKIM != "pass" {
		t.Fatalf("DKIM = %q, want pass (one valid signature)", res.DKIM)
	}
}
