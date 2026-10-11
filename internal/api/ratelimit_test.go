package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/daiwa-zou/kiln/internal/auth"
)

func TestLimiterBurstAndRefill(t *testing.T) {
	now := time.Unix(0, 0)
	l := newLimiter()
	l.now = func() time.Time { return now }

	// The full burst is available immediately; the next take is refused.
	for i := 0; i < writeBurst; i++ {
		if !l.take("u:1") {
			t.Fatalf("take %d refused inside the burst", i+1)
		}
	}
	if l.take("u:1") {
		t.Fatal("burst-exhausted bucket still granted a token")
	}

	// One refill interval buys exactly one token.
	now = now.Add(writeRefillEach)
	if !l.take("u:1") {
		t.Error("refilled token not granted")
	}
	if l.take("u:1") {
		t.Error("second token granted after a single refill interval")
	}

	// Buckets are per caller: another key is untouched.
	if !l.take("u:2") {
		t.Error("a different caller was throttled by the first one's bucket")
	}
}

func TestWriteLimiterAnswers429(t *testing.T) {
	l := newLimiter()
	handler := writeLimiter(l)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	var last *httptest.ResponseRecorder
	for i := 0; i <= writeBurst; i++ {
		last = httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.RemoteAddr = "10.1.2.3:5555"
		handler.ServeHTTP(last, req)
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("request %d = %d, want 429", writeBurst+1, last.Code)
	}
	if last.Header().Get("Retry-After") == "" {
		t.Error("429 carries no Retry-After")
	}
}

func TestLimiterKeyPrefersIdentity(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "10.1.2.3:5555"

	if got := limiterKey(req); got != "a:10.1.2.3" {
		t.Errorf("anonymous key = %q, want a:10.1.2.3", got)
	}

	// With an identity, the bucket follows the user across addresses. The
	// planted identity needs the read scope or the middleware 403s before the
	// context is ever populated.
	ctx := authContext(req.Context(), auth.Identity{UserID: "u-9", Scopes: []string{"read"}})
	if got := limiterKey(req.WithContext(ctx)); got != "u:u-9" {
		t.Errorf("authenticated key = %q, want u:u-9", got)
	}
}

// authContext plants an identity the way auth.Middleware does. The context
// key is unexported, so this round-trips through a real middleware pass.
func authContext(ctx context.Context, id auth.Identity) context.Context {
	var out context.Context
	m := &auth.Middleware{Source: plantSource{id}}
	h := m.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		out = r.Context()
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer kiln_"+repeatHex64())
	h.ServeHTTP(httptest.NewRecorder(), req)
	return out
}

type plantSource struct{ id auth.Identity }

func (p plantSource) IdentityForToken(context.Context, string) (auth.Identity, error) {
	return p.id, nil
}

func repeatHex64() string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}

// With no trusted proxies the request path must be exactly today's: no extra
// middleware, and an IPv4 key that is the RemoteAddr host whatever headers
// the caller wrote.
func TestLimiterKeyIdenticalWithoutTrustedProxies(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "10.1.2.3:5555"
	req.Header.Set("X-Forwarded-For", "198.51.100.1")
	req.Header.Set("X-Real-IP", "198.51.100.2")
	if got := limiterKey(req); got != "a:10.1.2.3" {
		t.Errorf("key = %q, want a:10.1.2.3", got)
	}

	// The fallback for a RemoteAddr that is not an address is unchanged too.
	req.RemoteAddr = "pipe"
	if got := limiterKey(req); got != "a:pipe" {
		t.Errorf("key = %q, want a:pipe", got)
	}

	without := (&Server{Store: &fakeReadStore{}}).Router().(*chi.Mux)
	with := (&Server{Store: &fakeReadStore{}, TrustedProxies: prefixes(t, "10.0.0.0/8")}).Router().(*chi.Mux)
	if got, want := len(with.Middlewares()), len(without.Middlewares())+1; got != want {
		t.Fatalf("middleware count with trusted proxies = %d, want %d", got, want)
	}
	// The same server with the list cleared must lose that middleware again,
	// or "empty means today's behaviour" is not what the router does.
	if n := len((&Server{Store: &fakeReadStore{}, TrustedProxies: []netip.Prefix{}}).Router().(*chi.Mux).Middlewares()); n != len(without.Middlewares()) {
		t.Errorf("empty non-nil list installed %d middleware, want %d", n, len(without.Middlewares()))
	}
}

func TestLimiterKeyIPv6Bucketing(t *testing.T) {
	trusted := prefixes(t, "10.0.0.0/8")
	keyOf := func(peer, xff string, proxies []netip.Prefix) string {
		t.Helper()
		var key string
		h := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { key = limiterKey(r) })
		var handler http.Handler = h
		if len(proxies) > 0 {
			handler = clientIPMiddleware(proxies)(h)
		}
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.RemoteAddr = peer
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		handler.ServeHTTP(httptest.NewRecorder(), req)
		return key
	}

	// Without proxies: the peer itself.
	a := keyOf("[2001:db8:1:2::1]:5555", "", nil)
	b := keyOf("[2001:db8:1:2:ffff:ffff:ffff:9]:6666", "", nil)
	c := keyOf("[2001:db8:1:3::1]:5555", "", nil)
	if a != b {
		t.Errorf("same /64 without proxies: %q != %q", a, b)
	}
	if a == c {
		t.Errorf("different /64 without proxies share key %q", a)
	}
	if a != "a:2001:db8:1:2::/64" {
		t.Errorf("v6 key = %q, want a:2001:db8:1:2::/64", a)
	}
	// A mapped IPv4 peer is an IPv4 caller, not a member of the ::/64 that
	// every mapped address would otherwise fall into.
	if got := keyOf("[::ffff:192.0.2.1]:5555", "", nil); got != "a:192.0.2.1" {
		t.Errorf("mapped v4 key = %q, want a:192.0.2.1", got)
	}
	if keyOf("[::ffff:192.0.2.1]:5555", "", nil) == keyOf("[::ffff:192.0.2.2]:5555", "", nil) {
		t.Error("two mapped IPv4 peers share a bucket")
	}

	// Behind a trusted proxy: the resolved client.
	a = keyOf("10.0.0.1:4000", "2001:db8:1:2::1", trusted)
	b = keyOf("10.0.0.1:4000", "2001:db8:1:2::abcd", trusted)
	c = keyOf("10.0.0.1:4000", "2001:db8:1:3::1", trusted)
	if a != b {
		t.Errorf("same /64 behind a proxy: %q != %q", a, b)
	}
	if a == c {
		t.Errorf("different /64 behind a proxy share key %q", a)
	}
	if got := keyOf("10.0.0.1:4000", "198.51.100.1", trusted); got != "a:198.51.100.1" {
		t.Errorf("v4 key behind a proxy = %q, want a:198.51.100.1", got)
	}
}

func TestTwoCallersBehindProxyGetSeparateBuckets(t *testing.T) {
	l := newLimiter()
	handler := clientIPMiddleware(prefixes(t, "10.0.0.0/8"))(writeLimiter(l)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	post := func(peer, xff string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.RemoteAddr = peer
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	// Caller A exhausts its bucket through the proxy.
	for i := 0; i < writeBurst; i++ {
		if code := post("10.0.0.1:4000", "198.51.100.1"); code != http.StatusOK {
			t.Fatalf("caller A request %d = %d", i+1, code)
		}
	}
	if code := post("10.0.0.1:4000", "198.51.100.1"); code != http.StatusTooManyRequests {
		t.Fatalf("caller A past its burst = %d, want 429", code)
	}
	// Caller B, same proxy, is untouched.
	if code := post("10.0.0.1:4001", "198.51.100.2"); code != http.StatusOK {
		t.Errorf("caller B behind the same proxy = %d, want 200", code)
	}

	// An untrusted peer cannot buy fresh buckets by rotating the header.
	for i := 0; i < writeBurst; i++ {
		if code := post("203.0.113.9:5000", "198.51.100."+strconv.Itoa(100+i)); code != http.StatusOK {
			t.Fatalf("untrusted peer request %d = %d", i+1, code)
		}
	}
	if code := post("203.0.113.9:5000", "198.51.100.250"); code != http.StatusTooManyRequests {
		t.Errorf("spoofed X-Forwarded-For from an untrusted peer = %d, want 429 (shared peer bucket)", code)
	}
}

func TestLimiterCapOverflow(t *testing.T) {
	now := time.Unix(0, 0)
	l := newLimiter()
	l.now = func() time.Time { return now }

	for i := 0; i < maxBuckets; i++ {
		if !l.take("a:" + strconv.Itoa(i)) {
			t.Fatalf("fresh key %d refused", i)
		}
	}
	if len(l.buckets) != maxBuckets {
		t.Fatalf("buckets = %d, want %d", len(l.buckets), maxBuckets)
	}

	// Past the cap the map stops growing and new keys share overflow shards.
	// Collect fresh keys that hash to one shard, and one that hashes elsewhere.
	target := l.shard("a:new-0")
	var same []string
	other := ""
	for i := 0; (len(same) <= writeBurst || other == "") && i < 1<<16; i++ {
		k := "a:new-" + strconv.Itoa(i)
		if l.shard(k) == target {
			same = append(same, k)
		} else if other == "" {
			other = k
		}
	}
	if len(same) <= writeBurst || other == "" {
		t.Fatalf("keys do not spread across overflow shards (%d in shard %d, other=%q)", len(same), target, other)
	}
	// A burst's worth of distinct new keys in one shard, then 429: they share it.
	for i, k := range same[:writeBurst] {
		if !l.take(k) {
			t.Fatalf("overflow take %d refused inside the shared burst", i+1)
		}
	}
	if l.take(same[writeBurst]) {
		t.Error("overflow shard granted past its burst: new keys in one shard are not sharing it")
	}
	// A drained shard does not block newcomers that land in another.
	if !l.take(other) {
		t.Error("a fresh key in an untouched shard was refused: one drained shard blocks every newcomer")
	}
	if len(l.buckets) > maxBuckets {
		t.Errorf("buckets = %d, exceeds the cap of %d", len(l.buckets), maxBuckets)
	}
	// A key that already had a bucket keeps it.
	if !l.take("a:7") {
		t.Error("an existing caller was pushed into the overflow bucket")
	}

	// Prune is amortised: a new key inside the window does not run it (only a
	// new key can, since an existing one needs no room)...
	pruned := l.lastPrune
	now = now.Add(30 * time.Second)
	l.take("a:probe")
	if !l.lastPrune.Equal(pruned) {
		t.Errorf("prune ran %s after the last one, want at most once a minute", now.Sub(pruned))
	}
	// ...and an hour later the stale buckets go, including ones that were
	// drained on their last take, which are full again by now.
	now = now.Add(2 * time.Hour)
	if !l.take("a:fresh") {
		t.Fatal("take after the prune window refused")
	}
	if !l.lastPrune.Equal(now) {
		t.Errorf("lastPrune = %v, want %v", l.lastPrune, now)
	}
	if len(l.buckets) != 1 {
		t.Errorf("buckets after prune = %d, want 1 (only a:fresh)", len(l.buckets))
	}
	if _, ok := l.buckets["a:fresh"]; !ok {
		t.Error("the key that triggered the prune went to the overflow bucket despite the room it freed")
	}
}

// A bucket emptied by its last take must still be pruned once it has been
// idle long enough to be full again; otherwise a caller who drained 10k keys
// would pin the map at the cap forever.
func TestLimiterPrunesDrainedBuckets(t *testing.T) {
	now := time.Unix(0, 0)
	l := newLimiter()
	l.now = func() time.Time { return now }
	for i := 0; i <= pruneAbove; i++ {
		key := "a:" + strconv.Itoa(i)
		for range writeBurst {
			l.take(key)
		}
	}
	now = now.Add(2 * time.Hour)
	l.take("a:fresh")
	if len(l.buckets) != 1 {
		t.Errorf("buckets after prune = %d, want 1", len(l.buckets))
	}
}

// The MCP endpoint dispatches its reads back through the router with a
// synthesized loopback peer. The client-address middleware must leave that
// address alone and key the inner request on it, not on whatever the outer
// request resolved to.
func TestMCPSyntheticAddrNotRewritten(t *testing.T) {
	var remote, key string
	inner := clientIPMiddleware(prefixes(t, "10.0.0.0/8", "127.0.0.0/8"))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remote, key = r.RemoteAddr, limiterKey(r)
		w.WriteHeader(http.StatusOK)
	}))
	rt := loopback{handler: func() http.Handler { return inner }}

	outer := withClientAddr(context.Background(), netip.MustParseAddr("198.51.100.1"))
	req, err := http.NewRequestWithContext(outer, http.MethodGet, "http://kiln.internal/api/v1/workspaces", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()

	if remote != "127.0.0.1:0" {
		t.Errorf("RemoteAddr = %q, want the synthesized 127.0.0.1:0", remote)
	}
	if key != "a:127.0.0.1" {
		t.Errorf("key = %q, want a:127.0.0.1", key)
	}
}
