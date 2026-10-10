// Package netguard decides which destination addresses a connector must not
// reach on a user's behalf, and owns the one HTTP client that enforces it.
//
// Connector configs are API-writable data, so a URL a worker fetches or clones
// is attacker-shaped. The deny-list lives here, once, because the web fetcher
// and the git clone each carried their own copy, and a gap fixed in one stayed
// open in the other.
//
// The same reasoning puts the enforcing client here (client.go): NewClient
// resolves, vets and dials the vetted IP itself on every connection, redirect
// hops included, so any feature that makes an outbound request to a
// user-supplied address gets the guard by construction rather than by copy.
// The git clone is the exception: git does its own connecting, so
// connector/git can only check Forbidden as a pre-flight, not pin.
package netguard

import (
	"net"
	"net/netip"
)

// special lists the ranges the standard library's classification does not
// cover and that are not the public internet.
//
//   - 0.0.0.0/8: "this network"; on Linux 0.0.0.0 itself connects to localhost.
//   - 100.64.0.0/10: carrier-grade NAT shared space. Not RFC 1918, so IsPrivate
//     misses it, and it is where some clouds put their metadata service
//     (Alibaba's is 100.100.100.200).
//   - 192.0.0.0/24: IETF protocol assignments.
//   - 198.18.0.0/15: benchmarking, routinely reachable inside a private
//     network.
//   - 240.0.0.0/4: reserved, including the 255.255.255.255 broadcast address.
//   - 168.63.129.16/32: Azure's WireServer, which serves VM provisioning and
//     health data on a public-looking address, so no category above holds it.
//   - 192.88.99.0/24: the deprecated 6to4 relay anycast block (RFC 7526).
//   - ::/96: deprecated IPv4-compatible addresses (::7f00:1 is 127.0.0.1 in
//     this spelling); :: and ::1 inside it are caught above as well.
//   - ::ffff:0:0:0/96: IPv4-translated addresses (RFC 2765), another IPv6
//     spelling of an IPv4 host that Unmap does not convert.
//   - 64:ff9b::/96: NAT64, which a translator maps back onto IPv4 -- including
//     the addresses above -- so allowing it reopens every IPv4 range blocked
//     here.
//   - 64:ff9b:1::/48: local-use NAT64 (RFC 8215), the same hole by another
//     prefix.
//   - 2001::/32: Teredo. IANA lists it as globally reachable, but it tunnels
//     to an IPv4 address embedded in the IPv6 one, so it is blocked for the
//     same reason as NAT64. A Teredo-only destination breaks; that is the
//     intended trade.
//   - 2002::/16: 6to4, deprecated, and likewise embeds an IPv4 address
//     (2002:a9fe:a9fe:: carries 169.254.169.254).
//   - fec0::/10: deprecated site-local, which IsPrivate (fc00::/7) misses.
//   - 100::/64, 2001:2::/48, 2001:db8::/32, 3fff::/20: discard-only,
//     benchmarking and documentation space. Nothing legitimate lives there, so
//     a name resolving into one is misconfigured or hostile.
var special = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("168.63.129.16/32"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("::ffff:0:0:0/96"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("fec0::/10"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001:2::/48"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("3fff::/20"),
}

// Forbidden reports whether ip is an address a connector must refuse: loopback,
// private, link-local (which includes the 169.254.169.254 cloud metadata
// endpoint), multicast, unspecified, or one of the special-purpose ranges
// above. An address that cannot be parsed is forbidden: failing closed is the
// only safe answer for a guard.
func Forbidden(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	return ForbiddenAddr(addr)
}

// ForbiddenAddr is Forbidden for a netip.Addr.
func ForbiddenAddr(addr netip.Addr) bool {
	// An IPv4-mapped IPv6 address (::ffff:127.0.0.1) reaches the IPv4 host, but
	// would not match an IPv4 prefix as written, so compare the IPv4 form. A
	// zone ("fe80::1%eth0") is irrelevant to which range it is in.
	addr = addr.Unmap().WithZone("")
	if !addr.IsValid() {
		return true
	}
	if addr.IsLoopback() ||
		addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() ||
		addr.IsMulticast() ||
		addr.IsUnspecified() {
		return true
	}
	for _, p := range special {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
