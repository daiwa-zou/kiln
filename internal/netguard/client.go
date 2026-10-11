package netguard

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"time"
)

// Outbound limits every caller shares.
const (
	defaultTimeout      = 30 * time.Second
	dialTimeout         = 10 * time.Second
	tlsHandshakeTimeout = 10 * time.Second
	// idleConnTimeout backstops CloseIdleConnections: a connection parked
	// between two requests must not outlive its caller indefinitely.
	idleConnTimeout = 30 * time.Second
	// maxDialAttempts bounds how many vetted addresses one dial tries, so a
	// name with a long answer list cannot stretch a dial across the budget.
	maxDialAttempts = 4
)

// ErrForbiddenAddress is wrapped by every refusal to connect to an address
// Forbidden rejects.
var ErrForbiddenAddress = errors.New("netguard: forbidden address")

// ErrTooLarge is returned by ReadCapped when the body exceeds its cap.
var ErrTooLarge = errors.New("netguard: body exceeds the size cap")

// Options shapes a client. The zero value is the strict one: public
// addresses only, https only, a 30s timeout, and redirects not followed.
type Options struct {
	// AllowPrivate turns the address guard off and AllowHTTP permits plain
	// http, on the first request and on redirect hops. Both exist for tests that must reach an httptest
	// server on loopback; production callers leave them false unless an
	// operator (never connector config) has said otherwise.
	AllowPrivate bool
	AllowHTTP    bool
	// Timeout bounds a whole request, redirects included. Zero means 30s.
	Timeout time.Duration
	// MaxRedirects is the number of requests a redirect chain may make
	// before it is refused, counted the way net/http's default policy
	// counts. Zero means redirects are not followed and the 3xx is returned.
	MaxRedirects int
	// RootCAs replaces the system roots. Test-only: it lets a test trust an
	// httptest TLS server. Production callers leave it nil.
	RootCAs *x509.CertPool
}

// NewClient builds the pinned-dial HTTP client for requests to addresses a
// user supplied.
//
// SECURITY: the guard lives in the transport's DialContext, not in a
// pre-flight check, so it applies to every connection the client ever opens,
// including redirect hops and fresh DNS answers. Each dial resolves, vets,
// then connects to the vetted IP directly: a host that re-resolves to
// something private mid-session never gets connected to.
//
// Each call builds its own transport, so the caller must CloseIdleConnections
// when done or its keep-alive connections are stranded until the remote
// hangs up.
func NewClient(o Options) *http.Client {
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	transport := &http.Transport{
		// No proxy, explicitly. ProxyFromEnvironment would hand every
		// request to whatever HTTPS_PROXY names, and the dialer would then
		// vet the proxy's address instead of the destination's.
		Proxy:       nil,
		DialContext: newDialer(o).DialContext,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    o.RootCAs,
		},
		TLSHandshakeTimeout: tlsHandshakeTimeout,
		IdleConnTimeout:     idleConnTimeout,
		// HTTP/1.1 only, as before the client moved here: a custom dialer
		// and TLS config already disable h2 unless this forces it.
		ForceAttemptHTTP2: false,
	}
	return &http.Client{
		Transport: &httpsOnly{next: transport, allowHTTP: o.AllowHTTP},
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if o.MaxRedirects <= 0 {
				return http.ErrUseLastResponse
			}
			if len(via) >= o.MaxRedirects {
				return fmt.Errorf("netguard: stopped after %d redirects", o.MaxRedirects)
			}
			// The scheme is not checked here: every hop goes through
			// httpsOnly, which refuses a non-https one.
			//
			// Go strips Authorization only on cross-host redirects and keeps
			// a hand-set Cookie; strip both on every hop. Callers should not
			// be setting either, so this is belt and braces.
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
			return nil
		},
	}
}

// httpsOnly refuses a non-https request, the first one or any redirect hop,
// before it reaches the transport. Callers validate their URLs too, but the
// client must not depend on every caller remembering to.
type httpsOnly struct {
	next      *http.Transport
	allowHTTP bool
}

func (h *httpsOnly) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" && !h.allowHTTP {
		// A RoundTripper must close the body even when it fails.
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, fmt.Errorf("netguard: non-https request to %s refused", req.URL.Redacted())
	}
	return h.next.RoundTrip(req)
}

// CloseIdleConnections forwards to the transport; http.Client finds it by
// interface, so without it the caller's CloseIdleConnections would do nothing.
func (h *httpsOnly) CloseIdleConnections() { h.next.CloseIdleConnections() }

// DialContext returns the guarded dial function NewClient's transport uses,
// for a caller that needs a connection rather than an HTTP client.
func DialContext(o Options) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return newDialer(o).DialContext
}

// dialer resolves, vets and dials. lookup and dial are the seams the tests
// replace; production uses the default resolver and a plain net.Dialer.
type dialer struct {
	allowPrivate bool
	lookup       func(ctx context.Context, network, host string) ([]net.IP, error)
	dial         func(ctx context.Context, network, addr string) (net.Conn, error)
}

func newDialer(o Options) *dialer {
	// No Timeout on the net.Dialer: each attempt gets its own deadline on the
	// context instead (see attemptDeadline).
	nd := &net.Dialer{}
	return &dialer{
		allowPrivate: o.AllowPrivate,
		lookup:       net.DefaultResolver.LookupIP,
		dial:         nd.DialContext,
	}
}

// DialContext refuses the host outright if any answer is forbidden, then
// tries the vetted addresses in order.
//
// Any, not all: with a public A and a private AAAA, or a split-horizon answer
// mixing the two, the address the caller ends up talking to would depend on
// dial order and on which family happens to work, and a rebinding attacker
// controls both. A dual-stack host with one forbidden family is refused.
func (d *dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := d.lookup(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("netguard: resolve %s: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("netguard: %s resolves to no addresses", host)
	}
	if !d.allowPrivate {
		for _, ip := range ips {
			if Forbidden(ip) {
				// The address itself stays out of the message: it reaches
				// skip reasons and run errors a workspace owner reads, and
				// echoing it would let a config probe the internal DNS.
				return nil, fmt.Errorf("netguard: %s resolves to a non-public address: %w", host, ErrForbiddenAddress)
			}
		}
	}
	if len(ips) > maxDialAttempts {
		ips = ips[:maxDialAttempts]
	}

	var errs []error
	for i, ip := range ips {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		actx, cancel := context.WithDeadline(ctx, attemptDeadline(ctx, time.Now(), len(ips)-i))
		conn, err := d.dial(actx, network, net.JoinHostPort(ip.String(), port))
		cancel()
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
	}
	return nil, errors.Join(errs...)
}

// attemptDeadline gives one dial attempt the smaller of dialTimeout and an
// even share of what is left of ctx's budget across the remaining addresses,
// as net.Dialer does for its own fallbacks. Without the share, one
// blackholed address would spend the whole request budget and the next
// address would never be tried.
func attemptDeadline(ctx context.Context, now time.Time, remaining int) time.Time {
	deadline := now.Add(dialTimeout)
	if d, ok := ctx.Deadline(); ok {
		if share := now.Add(d.Sub(now) / time.Duration(remaining)); share.Before(deadline) {
			deadline = share
		}
	}
	return deadline
}

// ReadCapped reads r to the end, or fails with ErrTooLarge once it has seen
// more than limit bytes, so a hostile server cannot make the caller buffer an
// unbounded body.
func ReadCapped(r io.Reader, limit int64) ([]byte, error) {
	// limit+1 below would overflow at MaxInt64 and read nothing, which would
	// pass as an empty body.
	if limit < 0 || limit == math.MaxInt64 {
		return nil, fmt.Errorf("netguard: read cap %d out of range", limit)
	}
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, ErrTooLarge
	}
	return b, nil
}
