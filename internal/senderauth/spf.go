package senderauth

import (
	"context"
	"net"
	"strconv"
	"strings"
)

// maxSPFDNSMechanisms is the RFC 7208 §4.6.4 limit on DNS-lookup mechanisms
// per record (a, mx, include, redirect).
const maxSPFDNSMechanisms = 10

// maxSPFIncludeDepth bounds include: recursion.
const maxSPFIncludeDepth = 10

// verifySPF evaluates the envelope domain's SPF record against peerIP. It is
// an RFC 7208 subset: ip4/ip6, a, mx, include and all. Macros, exists,
// ptr and redirect are unsupported; a record containing them still evaluates
// its other mechanisms (unsupported terms are treated as neutral).
func (v *verifier) verifySPF(ctx context.Context, envelopeFrom, peerIP string) string {
	domain := domainOf(envelopeFrom)
	if domain == "" {
		return "none"
	}
	records, err := v.lookupTXT(ctx, domain)
	if err != nil {
		if isNotFound(err) {
			return "none"
		}
		return "temperror"
	}
	record := spfRecord(records)
	if record == "" {
		return "none"
	}
	return v.evalSPF(ctx, record, domain, peerIP, 0, 0)
}

// spfRecord returns the first v=spf1 record, or "".
func spfRecord(records []string) string {
	for _, r := range records {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(r)), "v=spf1") {
			return strings.TrimSpace(r)
		}
	}
	return ""
}

// evalSPF returns pass|fail|softfail|neutral|permerror|temperror. dns mechanism
// budget and include depth are threaded so recursion cannot run away.
func (v *verifier) evalSPF(ctx context.Context, record, domain, peerIP string, mechCount, depth int) string {
	terms := strings.Fields(record)
	if len(terms) == 0 {
		return "neutral"
	}

	for _, term := range terms[1:] {
		if ctx.Err() != nil {
			return "temperror"
		}
		if term == "" {
			continue
		}
		qualifier := byte('+')
		if strings.ContainsRune("+-~?", rune(term[0])) {
			qualifier = term[0]
			term = term[1:]
		}
		// redirect=/exp= are modifiers, not mechanisms; redirect is treated
		// as neutral in v1 (no macro support).
		if strings.HasPrefix(term, "redirect=") || strings.HasPrefix(term, "exp=") {
			continue
		}
		name, arg := term, ""
		if i := strings.Index(term, ":"); i >= 0 {
			name, arg = term[:i], term[i+1:]
		}

		var matched bool
		var errLevel string
		switch name {
		case "all":
			matched = true
		case "ip4":
			matched = ipMatch(arg, peerIP, 32)
		case "ip6":
			matched = ipMatch(arg, peerIP, 128)
		case "a":
			mechCount++
			if mechCount > maxSPFDNSMechanisms {
				return "permerror"
			}
			matched, errLevel = v.matchHost(ctx, arg, domain, peerIP)
		case "mx":
			mechCount++
			if mechCount > maxSPFDNSMechanisms {
				return "permerror"
			}
			matched, errLevel = v.matchMX(ctx, arg, domain, peerIP)
		case "include":
			mechCount++
			if mechCount > maxSPFDNSMechanisms || depth+1 > maxSPFIncludeDepth {
				return "permerror"
			}
			inc := v.evalInclude(ctx, arg, peerIP, depth+1)
			if inc == "permerror" || inc == "temperror" {
				return inc
			}
			matched = inc == "pass"
		default:
			// exists, ptr, unsupported macros: neutral, no match.
			matched = false
		}

		if errLevel != "" {
			return errLevel
		}
		if matched {
			switch qualifier {
			case '-':
				return "fail"
			case '~':
				return "softfail"
			case '?':
				return "neutral"
			default:
				return "pass"
			}
		}
	}
	return "neutral"
}

// evalInclude evaluates include:<domain>. pass means the mechanism matched;
// temperror/permerror propagate; anything else means no match.
func (v *verifier) evalInclude(ctx context.Context, domain, peerIP string, depth int) string {
	if domain == "" {
		return "permerror"
	}
	records, err := v.lookupTXT(ctx, domain)
	if err != nil {
		if isNotFound(err) {
			return "neutral"
		}
		return "temperror"
	}
	record := spfRecord(records)
	if record == "" {
		return "neutral"
	}
	return v.evalSPF(ctx, record, domain, peerIP, 0, depth)
}

// matchHost implements the a[:domain][/cidr] mechanism.
func (v *verifier) matchHost(ctx context.Context, arg, spfDomain, peerIP string) (bool, string) {
	domain, cidr := splitCIDR(arg)
	if domain == "" {
		domain = spfDomain
	}
	peer := net.ParseIP(peerIP)
	if peer == nil {
		return false, ""
	}
	addrs, err := v.ipResolver.LookupIP(ctx, "ip", domain)
	if err != nil {
		return false, "temperror"
	}
	for _, a := range addrs {
		if ipMatchNetwork(a, peer, cidr) {
			return true, ""
		}
	}
	return false, ""
}

// matchMX implements the mx[:domain][/cidr] mechanism.
func (v *verifier) matchMX(ctx context.Context, arg, spfDomain, peerIP string) (bool, string) {
	domain, cidr := splitCIDR(arg)
	if domain == "" {
		domain = spfDomain
	}
	peer := net.ParseIP(peerIP)
	if peer == nil {
		return false, ""
	}
	mxs, err := v.ipResolver.LookupMX(ctx, domain)
	if err != nil {
		return false, "temperror"
	}
	for _, mx := range mxs {
		addrs, err := v.ipResolver.LookupIP(ctx, "ip", mx.Host)
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipMatchNetwork(a, peer, cidr) {
				return true, ""
			}
		}
	}
	return false, ""
}

func splitCIDR(arg string) (domain, cidr string) {
	if i := strings.Index(arg, "/"); i >= 0 {
		return arg[:i], arg[i+1:]
	}
	return arg, ""
}

// ipMatch matches a bare/qualified ip4/ip6 mechanism argument against peerIP.
// bits is the address family of the mechanism (32 for ip4, 128 for ip6).
func ipMatch(arg, peerIP string, bits int) bool {
	spec, cidr := splitCIDR(arg)
	ip := net.ParseIP(spec)
	peer := net.ParseIP(peerIP)
	if ip == nil || peer == nil {
		return false
	}
	if bits == 32 {
		if ip.To4() == nil || peer.To4() == nil {
			return false
		}
	} else if ip.To4() != nil || peer.To4() != nil {
		return false
	}
	if cidr == "" {
		return ip.Equal(peer)
	}
	return ipMatchBits(ip, peer, cidr, bits)
}

// ipMatchNetwork matches an address from a, mx resolution against peerIP with
// an optional cidr.
func ipMatchNetwork(addr, peer net.IP, cidr string) bool {
	if cidr == "" {
		return addr.Equal(peer)
	}
	bits := 128
	if addr.To4() != nil && peer.To4() != nil {
		bits = 32
	} else if (addr.To4() == nil) != (peer.To4() == nil) {
		return false
	}
	return ipMatchBits(addr, peer, cidr, bits)
}

func ipMatchBits(a, b net.IP, cidr string, bits int) bool {
	ones, err := strconv.Atoi(cidr)
	if err != nil || ones < 0 || ones > bits {
		return false
	}
	mask := net.CIDRMask(ones, bits)
	if bits == 32 {
		return a.To4().Mask(mask).Equal(b.To4().Mask(mask))
	}
	return a.Mask(mask).Equal(b.Mask(mask))
}
