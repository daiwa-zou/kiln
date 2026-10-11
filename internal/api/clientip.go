package api

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
)

// Behind a load balancer every request arrives from the balancer's address,
// so keying unauthenticated rate limits on the TCP peer puts every anonymous
// caller in one bucket. The client's address is in X-Forwarded-For, but that
// header is whatever the sender wrote: only the entries appended by proxies
// we run are evidence. So the header is read only when the peer is one of
// the configured trusted proxies, and then walked from the right, past our
// own hops, to the first address a trusted proxy vouched for receiving from.
//
// X-Real-IP and Forwarded are not read. Each additional header is another
// one a misconfigured proxy might pass through from the client untouched.
//
// The result rides in the request context and never replaces RemoteAddr:
// the auth-none refusal and the local guard reason about the real
// connection, and nothing here should be able to change what they see.

// maxForwardedHops bounds how much of X-Forwarded-For is examined. Real
// chains are a handful of hops; the rest of a longer header is attacker
// padding, and walking it costs CPU on the unauthenticated path.
const maxForwardedHops = 32

type clientAddrKey struct{}

// withClientAddr stores the resolved client address. An invalid address is
// stored deliberately: it clears a value inherited from an outer request (the
// MCP endpoint re-dispatches through the router with the outer context).
func withClientAddr(ctx context.Context, a netip.Addr) context.Context {
	return context.WithValue(ctx, clientAddrKey{}, a)
}

// clientAddrFrom returns the address clientIPMiddleware resolved, if any.
func clientAddrFrom(ctx context.Context) (netip.Addr, bool) {
	a, ok := ctx.Value(clientAddrKey{}).(netip.Addr)
	return a, ok && a.IsValid()
}

// clientIPMiddleware resolves the client address against the trusted proxy
// list. Router installs it only when the list is non-empty, so a deployment
// without one runs exactly the code it ran before the list existed.
func clientIPMiddleware(trusted []netip.Prefix) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			a := clientIP(r.RemoteAddr, r.Header.Values("X-Forwarded-For"), trusted)
			next.ServeHTTP(w, r.WithContext(withClientAddr(r.Context(), a)))
		})
	}
}

// clientIP resolves the caller's address from the TCP peer and the
// X-Forwarded-For header lines, in the order they arrived. It returns the
// zero Addr when the peer itself is not an address.
//
//   - An untrusted peer is the client, and every header is ignored.
//   - Otherwise the entries are walked right to left across all lines,
//     skipping trusted hops; the first untrusted entry is the client.
//   - An entry that is not an address stops the walk at the last trusted hop:
//     nothing to its left was written by a proxy we can vouch for.
//   - If every examined entry is trusted, the leftmost of them is the client.
func clientIP(peer string, xff []string, trusted []netip.Prefix) netip.Addr {
	last, ok := canonicalAddr(peer)
	if !ok {
		return netip.Addr{}
	}
	if !isTrusted(last, trusted) {
		return last
	}

	examined := 0
	for i := len(xff) - 1; i >= 0; i-- {
		line := xff[i]
		for line != "" {
			if examined == maxForwardedHops {
				return last
			}
			examined++

			tok := line
			if j := strings.LastIndexByte(line, ','); j >= 0 {
				tok, line = line[j+1:], line[:j]
			} else {
				line = ""
			}
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}

			a, ok := canonicalAddr(tok)
			if !ok {
				return last
			}
			if !isTrusted(a, trusted) {
				return a
			}
			last = a
		}
	}
	return last
}

// canonicalAddr parses a peer address or header entry into the one form
// both prefix matching and bucket keys use: any port stripped, an IPv4-mapped
// IPv6 address unmapped (so it matches IPv4 prefixes and keys like the IPv4
// caller it is), and any zone dropped.
func canonicalAddr(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	var a netip.Addr
	if ap, err := netip.ParseAddrPort(s); err == nil {
		a = ap.Addr()
	} else {
		s = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
		if a, err = netip.ParseAddr(s); err != nil {
			return netip.Addr{}, false
		}
	}
	return a.Unmap().WithZone(""), true
}

func isTrusted(a netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
