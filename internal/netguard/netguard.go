// Package netguard decides which destination addresses a connector must not
// reach on a user's behalf.
//
// Connector configs are API-writable data, so a URL a worker fetches or clones
// is attacker-shaped. The deny-list lives here, once, because the web fetcher
// and the git clone each carried their own copy, and a gap fixed in one stayed
// open in the other.
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
//   - 64:ff9b::/96: NAT64, which a translator maps back onto IPv4 -- including
//     the addresses above -- so allowing it reopens every IPv4 range blocked
//     here.
var special = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
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
