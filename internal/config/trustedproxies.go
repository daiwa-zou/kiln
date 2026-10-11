package config

import (
	"fmt"
	"net/netip"
	"strings"
)

// ParseTrustedProxies turns trusted_proxies entries into prefixes. An entry
// is a CIDR or a bare address (one host). Entries are trimmed, because the
// environment form is split on commas and "a, b" is how people write lists;
// empty entries (a trailing comma) are skipped. Every bad entry is reported,
// not just the first.
//
// Prefixes come back canonical, matching how internal/api canonicalises the
// addresses it compares against them: host bits masked, zone dropped, and an
// IPv4-mapped IPv6 prefix rewritten as the IPv4 prefix it covers, since a
// mapped peer is matched in its unmapped form.
func ParseTrustedProxies(entries []string) ([]netip.Prefix, error) {
	var (
		out []netip.Prefix
		bad []string
	)
	for _, raw := range entries {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		p, ok := parseTrustedProxy(s)
		if !ok {
			bad = append(bad, fmt.Sprintf("%q", s))
			continue
		}
		out = append(out, p)
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("trusted_proxies: not a CIDR or IP address: %s", strings.Join(bad, ", "))
	}
	return out, nil
}

func parseTrustedProxy(s string) (netip.Prefix, bool) {
	var p netip.Prefix
	if strings.Contains(s, "/") {
		var err error
		if p, err = netip.ParsePrefix(s); err != nil {
			return netip.Prefix{}, false
		}
	} else {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return netip.Prefix{}, false
		}
		a = a.WithZone("")
		p = netip.PrefixFrom(a, a.BitLen())
	}
	if a := p.Addr(); a.Is4In6() && p.Bits() >= 96 {
		p = netip.PrefixFrom(a.Unmap(), p.Bits()-96)
	}
	return p.Masked(), true
}

// TrustedProxyWarnings names trusted prefixes broad enough that clients, not
// proxies, are likely inside them. A trusted peer's X-Forwarded-For is
// believed, so a client inside the list chooses its own rate-limit key. The
// thresholds are a smell test, not a rule: an IPv4 /8 or an IPv6 /32 is
// already an organisation-sized range.
func TrustedProxyWarnings(prefixes []netip.Prefix) []string {
	var out []string
	for _, p := range prefixes {
		switch {
		case p.Bits() == 0:
			out = append(out, fmt.Sprintf(
				"trusted_proxies contains %s, which trusts every address: any client can set its own address with X-Forwarded-For", p))
		case p.Addr().Is4() && p.Bits() <= 8, p.Addr().Is6() && p.Bits() <= 32:
			out = append(out, fmt.Sprintf(
				"trusted_proxies contains %s, a broad range: clients inside it can set their own address with X-Forwarded-For; list only your proxies", p))
		}
	}
	return out
}
