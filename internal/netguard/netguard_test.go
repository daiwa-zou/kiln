package netguard

import (
	"net"
	"testing"
)

func TestForbidden(t *testing.T) {
	tests := []struct {
		name string
		ip   string
		want bool
	}{
		// What the standard library already classifies.
		{"loopback v4", "127.0.0.1", true},
		{"loopback v4 other", "127.8.9.10", true},
		{"loopback v6", "::1", true},
		{"rfc1918 10/8", "10.0.0.8", true},
		{"rfc1918 172.16/12", "172.16.5.4", true},
		{"rfc1918 192.168/16", "192.168.1.5", true},
		{"unique local v6", "fd12:3456::1", true},
		{"link-local v4", "169.254.10.10", true},
		{"cloud metadata", "169.254.169.254", true},
		{"link-local v6", "fe80::1", true},
		{"multicast v4", "224.0.0.1", true},
		{"multicast v6", "ff02::1", true},
		{"unspecified v4", "0.0.0.0", true},
		{"unspecified v6", "::", true},

		// The ranges the standard library does not.
		{"0.0.0.0/8", "0.1.2.3", true},
		{"0.0.0.0/8 top", "0.255.255.255", true},
		{"cgnat start", "100.64.0.0", true},
		{"cgnat alibaba metadata", "100.100.100.200", true},
		{"cgnat end", "100.127.255.255", true},
		{"ietf protocol assignments", "192.0.0.8", true},
		{"benchmarking start", "198.18.0.1", true},
		{"benchmarking end", "198.19.255.254", true},
		{"reserved 240/4", "240.0.0.1", true},
		{"reserved top", "250.1.2.3", true},
		{"broadcast", "255.255.255.255", true},
		{"nat64", "64:ff9b::7f00:1", true},
		{"nat64 embedding metadata", "64:ff9b::a9fe:a9fe", true},

		// The first and last address of each range added for M11, with the
		// IPv4-embedding cases that are the reason for most of them.
		{"6to4 relay first", "192.88.99.0", true},
		{"6to4 relay last", "192.88.99.255", true},
		{"local nat64 first", "64:ff9b:1::", true},
		{"local nat64 last", "64:ff9b:1:ffff:ffff:ffff:ffff:ffff", true},
		{"local nat64 embedding metadata", "64:ff9b:1::a9fe:a9fe", true},
		{"teredo first", "2001::", true},
		{"teredo last", "2001:0:ffff:ffff:ffff:ffff:ffff:ffff", true},
		{"6to4 first", "2002::", true},
		{"6to4 last", "2002:ffff:ffff:ffff:ffff:ffff:ffff:ffff", true},
		{"6to4 embedding metadata", "2002:a9fe:a9fe::1", true},
		{"site-local first", "fec0::", true},
		{"site-local last", "feff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", true},
		{"discard-only first", "100::", true},
		{"discard-only last", "100::ffff:ffff:ffff:ffff", true},
		{"benchmarking v6 first", "2001:2::", true},
		{"benchmarking v6 last", "2001:2:0:ffff:ffff:ffff:ffff:ffff", true},
		{"documentation first", "2001:db8::", true},
		{"documentation last", "2001:db8:ffff:ffff:ffff:ffff:ffff:ffff", true},
		{"documentation 3fff first", "3fff::", true},
		{"documentation 3fff last", "3fff:fff:ffff:ffff:ffff:ffff:ffff:ffff", true},

		{"azure wireserver", "168.63.129.16", true},
		{"v4-compatible loopback", "::7f00:1", true},
		{"v4-compatible metadata", "::a9fe:a9fe", true},
		{"v4-compatible last", "::ffff:ffff", true},
		{"v4-translated loopback", "::ffff:0:7f00:1", true},
		{"v4-translated first", "::ffff:0:0:0", true},
		{"v4-translated last", "::ffff:0:ffff:ffff", true},

		// Their neighbours stay reachable.
		{"below azure wireserver", "168.63.129.15", false},
		{"above azure wireserver", "168.63.129.17", false},
		{"above v4-compatible", "::1:0:0:0", false},
		{"above v4-translated", "::ffff:1:0:0", false},
		{"below 6to4 relay", "192.88.98.255", false},
		{"above 6to4 relay", "192.88.100.0", false},
		{"above local nat64", "64:ff9b:2::1", false},
		{"between the nat64 prefixes", "64:ff9b:0:1::1", false},
		{"above teredo", "2001:1::1", false},
		{"below benchmarking v6", "2001:1:ffff:ffff:ffff:ffff:ffff:ffff", false},
		{"above benchmarking v6", "2001:2:1::1", false},
		{"amt", "2001:3::1", false},
		{"below documentation", "2001:db7:ffff:ffff:ffff:ffff:ffff:ffff", false},
		{"above documentation", "2001:db9::1", false},
		{"google dns v6", "2001:4860:4860::8888", false},
		{"above 6to4", "2003::1", false},
		{"below 6to4", "2001:ffff:ffff:ffff:ffff:ffff:ffff:ffff", false},
		{"below site-local", "fe00::1", false},
		{"above discard-only", "100:0:0:1::", false},
		{"below documentation 3fff", "3ffe:ffff:ffff:ffff:ffff:ffff:ffff:ffff", false},
		{"above documentation 3fff", "3fff:1000::1", false},

		// Forms that reach an IPv4 host through an IPv6 spelling.
		{"v4-mapped loopback", "::ffff:127.0.0.1", true},
		{"v4-mapped cgnat", "::ffff:100.100.100.200", true},
		{"v4-mapped metadata", "::ffff:169.254.169.254", true},

		// Neighbours just outside each range stay reachable.
		{"below cgnat", "100.63.255.255", false},
		{"above cgnat", "100.128.0.0", false},
		{"above 0/8", "1.0.0.1", false},
		{"below benchmarking", "198.17.255.255", false},
		{"above benchmarking", "198.20.0.0", false},
		{"top of multicast", "239.255.255.255", true},
		{"just outside ietf block", "192.0.1.1", false},
		{"outside nat64", "64:ff9a::1", false},
		{"outside nat64 above", "64:ff9c::1", false},

		// Ordinary public addresses.
		{"google dns", "8.8.8.8", false},
		{"cloudflare dns", "1.1.1.1", false},
		{"github", "140.82.112.3", false},
		{"public v6", "2606:4700:4700::1111", false},
		{"v4-mapped public", "::ffff:8.8.8.8", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			if ip == nil {
				t.Fatalf("bad test address %q", tt.ip)
			}
			if got := Forbidden(ip); got != tt.want {
				t.Errorf("Forbidden(%s) = %v, want %v", tt.ip, got, tt.want)
			}
		})
	}
}

func TestForbiddenFailsClosedOnAnUnparseableAddress(t *testing.T) {
	for _, ip := range []net.IP{nil, {}, {1, 2, 3}} {
		if !Forbidden(ip) {
			t.Errorf("Forbidden(%v) = false, want true", []byte(ip))
		}
	}
}
