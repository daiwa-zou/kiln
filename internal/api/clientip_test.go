package api

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func prefixes(t *testing.T, ss ...string) []netip.Prefix {
	t.Helper()
	var out []netip.Prefix
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

func TestClientIP(t *testing.T) {
	lb := prefixes(t, "10.0.0.0/8", "fd00::/8")

	// thirtyTwoTrusted is 32 trusted hops, so a client to their left is the
	// 33rd entry from the right and must never be examined.
	thirtyTwoTrusted := strings.TrimSuffix(strings.Repeat("10.9.9.9, ", 31), ", ") + ", 10.0.0.32"

	cases := []struct {
		name    string
		peer    string
		xff     []string
		trusted []netip.Prefix
		want    string // "" means no address could be resolved
	}{
		{"no trusted list ignores headers", "203.0.113.7:4000", []string{"198.51.100.1"}, nil, "203.0.113.7"},
		{"untrusted peer ignores headers", "203.0.113.7:4000", []string{"198.51.100.1"}, lb, "203.0.113.7"},
		{"untrusted peer ignores a header naming a trusted hop", "203.0.113.7:4000", []string{"10.0.0.5"}, lb, "203.0.113.7"},
		{"trusted peer without a header is the client", "10.0.0.1:4000", nil, lb, "10.0.0.1"},
		{"one hop", "10.0.0.1:4000", []string{"198.51.100.1"}, lb, "198.51.100.1"},
		{"right-to-left skips trusted hops", "10.0.0.1:4000", []string{"198.51.100.1, 10.0.0.2, 10.0.0.3"}, lb, "198.51.100.1"},
		{"a spoofed prefix left of the real client is not reached", "10.0.0.1:4000", []string{"1.1.1.1, 198.51.100.1, 10.0.0.2"}, lb, "198.51.100.1"},
		{"multiple header lines are joined in order", "10.0.0.1:4000", []string{"1.1.1.1, 198.51.100.1", "10.0.0.2"}, lb, "198.51.100.1"},
		{"the last header line is walked first", "10.0.0.1:4000", []string{"10.0.0.2", "198.51.100.1"}, lb, "198.51.100.1"},
		{"entry with a port", "10.0.0.1:4000", []string{"198.51.100.1:5555"}, lb, "198.51.100.1"},
		{"bracketed v6 entry with a port", "10.0.0.1:4000", []string{"[2001:db8::1]:5555"}, lb, "2001:db8::1"},
		{"bare v6 entry", "10.0.0.1:4000", []string{"2001:db8::1"}, lb, "2001:db8::1"},
		{"bracketed v6 entry without a port", "10.0.0.1:4000", []string{"[2001:db8::1]"}, lb, "2001:db8::1"},
		{"v6 peer and v6 trusted hop", "[fd00::1]:4000", []string{"2001:db8::9, fd00::2"}, lb, "2001:db8::9"},
		{"mapped peer matches an IPv4 prefix", "[::ffff:10.0.0.1]:4000", []string{"198.51.100.1"}, lb, "198.51.100.1"},
		{"mapped peer resolves unmapped", "[::ffff:203.0.113.7]:4000", nil, lb, "203.0.113.7"},
		{"mapped hop matches an IPv4 prefix", "10.0.0.1:4000", []string{"198.51.100.1, ::ffff:10.0.0.2"}, lb, "198.51.100.1"},
		{"mapped client is unmapped", "10.0.0.1:4000", []string{"::ffff:198.51.100.1"}, lb, "198.51.100.1"},
		{"zone dropped from the peer", "[fe80::1%eth0]:4000", nil, prefixes(t, "fe80::/10"), "fe80::1"},
		{"zone dropped from an entry", "10.0.0.1:4000", []string{"fe80::7%eth0"}, lb, "fe80::7"},
		{"whitespace around entries", "10.0.0.1:4000", []string{"  198.51.100.1 ,\t10.0.0.2  "}, lb, "198.51.100.1"},
		{"empty tokens are skipped", "10.0.0.1:4000", []string{"198.51.100.1,, ,10.0.0.2,"}, lb, "198.51.100.1"},
		{"an empty header line is skipped", "10.0.0.1:4000", []string{"198.51.100.1", ""}, lb, "198.51.100.1"},
		{"garbage stops at the last trusted hop", "10.0.0.1:4000", []string{"198.51.100.1, not-an-ip, 10.0.0.2"}, lb, "10.0.0.2"},
		{"garbage straight after the peer is the peer", "10.0.0.1:4000", []string{"unknown"}, lb, "10.0.0.1"},
		{"a hostname is garbage", "10.0.0.1:4000", []string{"example.com"}, lb, "10.0.0.1"},
		{"all trusted resolves to the leftmost", "10.0.0.1:4000", []string{"10.0.0.4, 10.0.0.3, 10.0.0.2"}, lb, "10.0.0.4"},
		{"the 33rd entry is not examined", "10.0.0.1:4000", []string{"198.51.100.1, " + thirtyTwoTrusted}, lb, "10.9.9.9"},
		{"the 32nd entry is examined", "10.0.0.1:4000", []string{"198.51.100.1, " + strings.SplitN(thirtyTwoTrusted, ", ", 2)[1]}, lb, "198.51.100.1"},
		{"the cap counts across header lines", "10.0.0.1:4000", []string{"198.51.100.1", thirtyTwoTrusted}, lb, "10.9.9.9"},
		{"peer without a port", "10.0.0.1", []string{"198.51.100.1"}, lb, "198.51.100.1"},
		{"untrusted peer without a port", "203.0.113.7", nil, lb, "203.0.113.7"},
		{"bare-IP trusted entry", "192.0.2.10:4000", []string{"198.51.100.1"}, prefixes(t, "192.0.2.10/32"), "198.51.100.1"},
		{"unparseable peer resolves nothing", "pipe", []string{"198.51.100.1"}, lb, ""},
		{"empty peer resolves nothing", "", nil, lb, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := clientIP(tc.peer, tc.xff, tc.trusted)
			if tc.want == "" {
				if got.IsValid() {
					t.Fatalf("clientIP = %v, want no address", got)
				}
				return
			}
			if !got.IsValid() || got.String() != tc.want {
				t.Fatalf("clientIP = %v, want %s", got, tc.want)
			}
		})
	}
}

// The resolved address rides in the context; RemoteAddr is what the
// connection said and stays that way for every other consumer.
func TestClientIPMiddlewareStoresWithoutRewriting(t *testing.T) {
	var gotAddr netip.Addr
	var gotOK bool
	var gotRemote string
	h := clientIPMiddleware(prefixes(t, "10.0.0.0/8"))(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotAddr, gotOK = clientAddrFrom(r.Context())
		gotRemote = r.RemoteAddr
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:4000"
	req.Header.Set("X-Forwarded-For", "198.51.100.1")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !gotOK || gotAddr.String() != "198.51.100.1" {
		t.Errorf("context address = %v (ok=%v), want 198.51.100.1", gotAddr, gotOK)
	}
	if gotRemote != "10.0.0.1:4000" {
		t.Errorf("RemoteAddr = %q, want it untouched", gotRemote)
	}

	// An unparseable peer must clear anything an outer dispatch left in the
	// context rather than inherit it.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "pipe"
	req = req.WithContext(withClientAddr(req.Context(), netip.MustParseAddr("198.51.100.99")))
	h.ServeHTTP(httptest.NewRecorder(), req)
	if gotOK {
		t.Errorf("unparseable peer kept an inherited address %v", gotAddr)
	}
}
