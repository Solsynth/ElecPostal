package senderauth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
)

func TestSPF(t *testing.T) {
	cases := []struct {
		name     string
		txt      map[string][]string
		txtErr   map[string]error
		ip       map[string][]net.IP
		mx       map[string][]*net.MX
		envelope string
		peer     string
		want     string
	}{
		{
			name: "ip4 cidr pass", txt: map[string][]string{"sender.example": {"v=spf1 ip4:192.0.2.0/24 -all"}},
			envelope: "a@sender.example", peer: "192.0.2.10", want: "pass",
		},
		{
			name: "ip4 exact pass", txt: map[string][]string{"sender.example": {"v=spf1 ip4:192.0.2.10 -all"}},
			envelope: "a@sender.example", peer: "192.0.2.10", want: "pass",
		},
		{
			name: "ip4 cidr fail -all", txt: map[string][]string{"sender.example": {"v=spf1 ip4:192.0.2.0/24 -all"}},
			envelope: "a@sender.example", peer: "198.51.100.7", want: "fail",
		},
		{
			name: "ip4 cidr softfail ~all", txt: map[string][]string{"sender.example": {"v=spf1 ip4:192.0.2.0/24 ~all"}},
			envelope: "a@sender.example", peer: "198.51.100.7", want: "softfail",
		},
		{
			name: "bare -all fails", txt: map[string][]string{"sender.example": {"v=spf1 -all"}},
			envelope: "a@sender.example", peer: "192.0.2.10", want: "fail",
		},
		{
			name: "+all passes", txt: map[string][]string{"sender.example": {"v=spf1 +all"}},
			envelope: "a@sender.example", peer: "198.51.100.7", want: "pass",
		},
		{
			name: "?all neutral", txt: map[string][]string{"sender.example": {"v=spf1 ?all"}},
			envelope: "a@sender.example", peer: "198.51.100.7", want: "neutral",
		},
		{
			name: "ip6 pass", txt: map[string][]string{"sender.example": {"v=spf1 ip6:2001:db8::/32 -all"}},
			envelope: "a@sender.example", peer: "2001:db8::1", want: "pass",
		},
		{
			name: "ip6 no match", txt: map[string][]string{"sender.example": {"v=spf1 ip6:2001:db8::/32 -all"}},
			envelope: "a@sender.example", peer: "2001:db9::1", want: "fail",
		},
		{
			name: "include pass",
			txt: map[string][]string{
				"sender.example": {"v=spf1 include:other.example -all"},
				"other.example":  {"v=spf1 ip4:203.0.113.5 -all"},
			},
			envelope: "a@sender.example", peer: "203.0.113.5", want: "pass",
		},
		{
			name: "include no match falls through to -all",
			txt: map[string][]string{
				"sender.example": {"v=spf1 include:other.example -all"},
				"other.example":  {"v=spf1 ip4:203.0.113.5 -all"},
			},
			envelope: "a@sender.example", peer: "198.51.100.7", want: "fail",
		},
		{
			name:     "include missing record falls through",
			txt:      map[string][]string{"sender.example": {"v=spf1 include:absent.example -all"}},
			envelope: "a@sender.example", peer: "198.51.100.7", want: "fail",
		},
		{
			name:     "a mechanism",
			txt:      map[string][]string{"sender.example": {"v=spf1 a -all"}},
			ip:       map[string][]net.IP{"sender.example": {net.ParseIP("192.0.2.44")}},
			envelope: "a@sender.example", peer: "192.0.2.44", want: "pass",
		},
		{
			name:     "mx mechanism",
			txt:      map[string][]string{"sender.example": {"v=spf1 mx -all"}},
			mx:       map[string][]*net.MX{"sender.example": {{Host: "mx.sender.example.", Pref: 10}}},
			ip:       map[string][]net.IP{"mx.sender.example.": {net.ParseIP("192.0.2.55")}},
			envelope: "a@sender.example", peer: "192.0.2.55", want: "pass",
		},
		{
			name: "no record", txt: map[string][]string{},
			envelope: "a@sender.example", peer: "192.0.2.10", want: "none",
		},
		{
			name: "dns failure", txtErr: map[string]error{"sender.example": errors.New("boom")},
			envelope: "a@sender.example", peer: "192.0.2.10", want: "temperror",
		},
		{
			name: "empty envelope domain", txt: map[string][]string{},
			envelope: "bounce", peer: "192.0.2.10", want: "none",
		},
		{
			name: "txt without spf record", txt: map[string][]string{"sender.example": {"google-site-verification=abc"}},
			envelope: "a@sender.example", peer: "192.0.2.10", want: "none",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeResolver()
			for k, v := range tc.txt {
				f.txt[k] = v
			}
			for k, v := range tc.txtErr {
				f.txtErr[k] = v
			}
			for k, v := range tc.ip {
				f.ip[k] = v
			}
			for k, v := range tc.mx {
				f.mx[k] = v
			}
			v := newTestVerifier(t, f, 0)
			if got := v.verifySPF(context.Background(), tc.envelope, tc.peer); got != tc.want {
				t.Fatalf("verifySPF = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSPFIncludeDepthCap: a chain longer than the RFC recursion limit yields
// permerror instead of recursing forever.
func TestSPFIncludeDepthCap(t *testing.T) {
	f := newFakeResolver()
	for i := 0; i <= 11; i++ {
		f.txt[fmt.Sprintf("d%d.example", i)] = []string{fmt.Sprintf("v=spf1 include:d%d.example -all", i+1)}
	}
	v := newTestVerifier(t, f, 0)
	if got := v.verifySPF(context.Background(), "a@d0.example", "192.0.2.1"); got != "permerror" {
		t.Fatalf("verifySPF = %q, want permerror (include depth cap)", got)
	}
}
