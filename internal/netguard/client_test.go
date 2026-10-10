package netguard_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daiwa-zou/kiln/internal/netguard"
)

// newTLSServer starts an httptest TLS server and returns it with the options
// a client needs to trust it and reach it on loopback. The server certificate
// also covers example.com, which the redirect tests use as a "public" name.
func newTLSServer(t *testing.T, h http.Handler) (*httptest.Server, netguard.Options) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	return srv, netguard.Options{AllowPrivate: true, RootCAs: rootsFor(srv)}
}

func rootsFor(srv *httptest.Server) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return pool
}

// response is what a test needs from a reply, so the body can be closed
// inside get and no *http.Response escapes it.
type response struct {
	StatusCode int
	Proto      string
	ProtoMajor int
	Header     http.Header
}

// get issues a GET with the given headers, drains and closes the body, and
// returns the reply's status line and headers.
func get(t *testing.T, c *http.Client, url string, header http.Header) (*response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return &response{StatusCode: resp.StatusCode, Proto: resp.Proto, ProtoMajor: resp.ProtoMajor, Header: resp.Header}, nil
}

// routeExampleCom points example.com:443 at srv while leaving every other
// address to the guarded dialer, so a test can stand a "public" first hop in
// front of a redirect the guard must still refuse.
func routeExampleCom(c *http.Client, srv *httptest.Server) {
	tr := netguard.TransportOf(c)
	guarded := tr.DialContext
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr == "example.com:443" {
			return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
		}
		return guarded(ctx, network, addr)
	}
}

func TestClientRefusesLoopback(t *testing.T) {
	var hits atomic.Int64
	srv, opts := newTLSServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	opts.AllowPrivate = false
	c := netguard.NewClient(opts)
	defer c.CloseIdleConnections()

	if _, err := get(t, c, srv.URL, nil); !errors.Is(err, netguard.ErrForbiddenAddress) {
		t.Fatalf("GET loopback = %v, want ErrForbiddenAddress", err)
	}
	if hits.Load() != 0 {
		t.Fatal("the loopback server was reached")
	}
}

func TestClientRefusesARedirectToLoopback(t *testing.T) {
	var internal atomic.Int64
	target, _ := newTLSServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { internal.Add(1) }))
	front, opts := newTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/admin", http.StatusFound)
	}))
	opts.AllowPrivate = false
	opts.MaxRedirects = 5
	c := netguard.NewClient(opts)
	defer c.CloseIdleConnections()
	routeExampleCom(c, front)

	_, err := get(t, c, "https://example.com/", nil)
	if !errors.Is(err, netguard.ErrForbiddenAddress) {
		t.Fatalf("redirect to loopback = %v, want ErrForbiddenAddress", err)
	}
	if internal.Load() != 0 {
		t.Fatal("the redirect target on loopback was reached")
	}
}

func TestClientRefusesHTTPUnlessAllowed(t *testing.T) {
	// The first request, not only a redirect hop: a caller that forgot to
	// validate its URL still gets https only.
	var hits atomic.Int64
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer plain.Close()

	c := netguard.NewClient(netguard.Options{AllowPrivate: true})
	defer c.CloseIdleConnections()
	if _, err := get(t, c, plain.URL, nil); err == nil || !strings.Contains(err.Error(), "non-https") {
		t.Fatalf("GET http = %v, want the non-https refusal", err)
	}
	if hits.Load() != 0 {
		t.Fatal("the http server was reached")
	}

	c = netguard.NewClient(netguard.Options{AllowPrivate: true, AllowHTTP: true})
	defer c.CloseIdleConnections()
	if resp, err := get(t, c, plain.URL, nil); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET http with AllowHTTP = %v, %v; want 200", resp, err)
	}
}

func TestClientRefusesARedirectToHTTPUnlessAllowed(t *testing.T) {
	var plainHits atomic.Int64
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { plainHits.Add(1) }))
	defer plain.Close()
	srv, opts := newTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/", http.StatusFound)
	}))
	opts.MaxRedirects = 5

	c := netguard.NewClient(opts)
	defer c.CloseIdleConnections()
	if _, err := get(t, c, srv.URL, nil); err == nil || !strings.Contains(err.Error(), "non-https") {
		t.Fatalf("redirect to http = %v, want the non-https refusal", err)
	}
	if plainHits.Load() != 0 {
		t.Fatal("the http redirect target was reached")
	}

	opts.AllowHTTP = true
	c = netguard.NewClient(opts)
	defer c.CloseIdleConnections()
	resp, err := get(t, c, srv.URL, nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("redirect to http with AllowHTTP = %v, %v; want 200", resp, err)
	}
	if plainHits.Load() != 1 {
		t.Fatalf("http target hits = %d, want 1", plainHits.Load())
	}
}

func TestClientWithoutRedirectsReturnsThe3xx(t *testing.T) {
	var followed atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/end", http.StatusFound)
	})
	mux.HandleFunc("/end", func(http.ResponseWriter, *http.Request) { followed.Add(1) })
	srv, opts := newTLSServer(t, mux)

	c := netguard.NewClient(opts) // MaxRedirects 0
	defer c.CloseIdleConnections()
	resp, err := get(t, c, srv.URL+"/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/end" {
		t.Fatalf("got %d Location %q, want the 302 itself", resp.StatusCode, resp.Header.Get("Location"))
	}
	if followed.Load() != 0 {
		t.Fatal("the redirect was followed")
	}
}

func TestClientCapsARedirectLoop(t *testing.T) {
	var hits atomic.Int64
	srv, opts := newTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "/again", http.StatusFound)
	}))
	opts.MaxRedirects = 3
	c := netguard.NewClient(opts)
	defer c.CloseIdleConnections()

	if _, err := get(t, c, srv.URL, nil); err == nil || !strings.Contains(err.Error(), "redirects") {
		t.Fatalf("redirect loop = %v, want the hop limit", err)
	}
	if hits.Load() != 3 {
		t.Fatalf("server saw %d requests, want 3", hits.Load())
	}
}

func TestClientStripsCredentialsOnRedirect(t *testing.T) {
	// A same-host redirect, where Go itself would forward both headers.
	var auth, cookie atomic.Value
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/end", http.StatusFound)
	})
	mux.HandleFunc("/end", func(_ http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		cookie.Store(r.Header.Get("Cookie"))
	})
	srv, opts := newTLSServer(t, mux)
	opts.MaxRedirects = 5
	c := netguard.NewClient(opts)
	defer c.CloseIdleConnections()

	resp, err := get(t, c, srv.URL+"/start", http.Header{
		"Authorization": {"Bearer secret"},
		"Cookie":        {"session=secret"},
	})
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %v, %v", resp, err)
	}
	if got, _ := auth.Load().(string); got != "" {
		t.Errorf("Authorization forwarded on redirect: %q", got)
	}
	if got, _ := cookie.Load().(string); got != "" {
		t.Errorf("Cookie forwarded on redirect: %q", got)
	}
}

func TestClientIgnoresProxyEnvironment(t *testing.T) {
	// A proxy that accepts and hangs up: any request routed to it fails.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var proxied atomic.Int64
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			proxied.Add(1)
			_ = conn.Close()
		}
	}()
	t.Setenv("HTTPS_PROXY", "http://"+ln.Addr().String())
	t.Setenv("HTTP_PROXY", "http://"+ln.Addr().String())

	srv, opts := newTLSServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	c := netguard.NewClient(opts)
	defer c.CloseIdleConnections()
	if tr := netguard.TransportOf(c); tr.Proxy != nil {
		t.Fatal("Transport.Proxy is set; it must be nil")
	}
	// example.com rather than loopback: ProxyFromEnvironment never proxies
	// loopback, so only a non-loopback name would show a proxy being used.
	routeExampleCom(c, srv)
	resp, err := get(t, c, "https://example.com/", nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET with a proxy in the environment = %v, %v; want a direct 200", resp, err)
	}
	if proxied.Load() != 0 {
		t.Fatalf("the proxy saw %d connections", proxied.Load())
	}
}

func TestClientSpeaksHTTP1ToAnH2Server(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	c := netguard.NewClient(netguard.Options{AllowPrivate: true, RootCAs: rootsFor(srv)})
	defer c.CloseIdleConnections()
	resp, err := get(t, c, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.ProtoMajor != 1 {
		t.Fatalf("negotiated %s, want HTTP/1.x", resp.Proto)
	}
}

func TestClientCloseIdleConnections(t *testing.T) {
	var open atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			open.Add(1)
		case http.StateClosed, http.StateHijacked:
			open.Add(-1)
		}
	}
	srv.StartTLS()
	defer srv.Close()

	c := netguard.NewClient(netguard.Options{AllowPrivate: true, RootCAs: rootsFor(srv)})
	if _, err := get(t, c, srv.URL, nil); err != nil {
		t.Fatal(err)
	}
	if open.Load() != 1 {
		t.Fatalf("open connections = %d after one request, want 1 kept alive", open.Load())
	}
	c.CloseIdleConnections()
	deadline := time.Now().Add(3 * time.Second)
	for open.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("open connections = %d after CloseIdleConnections, want 0", open.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestReadCapped(t *testing.T) {
	const limit = 8
	tests := []struct {
		name    string
		body    string
		wantErr error
	}{
		{"empty", "", nil},
		{"under the cap", "1234567", nil},
		{"exactly the cap", "12345678", nil},
		{"one byte over", "123456789", netguard.ErrTooLarge},
		{"far over", strings.Repeat("x", 1<<20), netguard.ErrTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := netguard.ReadCapped(strings.NewReader(tt.body), limit)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if err == nil && !bytes.Equal(got, []byte(tt.body)) {
				t.Fatalf("got %q, want %q", got, tt.body)
			}
		})
	}

	boom := errors.New("connection reset")
	if _, err := netguard.ReadCapped(io.MultiReader(strings.NewReader("abc"), errReader{boom}), limit); !errors.Is(err, boom) {
		t.Fatalf("read error = %v, want it passed through", err)
	}

	// A cap that limit+1 would overflow, or a negative one, is refused
	// rather than read as "nothing allowed, nothing read, success".
	for _, bad := range []int64{math.MaxInt64, -1} {
		if got, err := netguard.ReadCapped(strings.NewReader("abc"), bad); err == nil {
			t.Errorf("ReadCapped(cap %d) = %q, nil; want an error", bad, got)
		}
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }
