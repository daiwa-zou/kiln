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
