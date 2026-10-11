package api

import (
	"hash/maphash"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/daiwa-zou/kiln/internal/auth"
)

// The write surface carries a small in-process token bucket per caller. This
// is a monolith writing to its own Postgres, so a shared limiter store would
// be infrastructure without a payoff; the goal is only that a runaway script
// cannot flood the review queue or steering docs, not fair global QoS.
//
// Sizing: writes here are human actions (pin a correction, resolve a review).
// A burst of 20 covers any real editing session; the refill of one every
// three seconds caps a sustained writer at 20/minute. Uploads get their own
// bucket: dragging a folder of documents into the browser is one user action
// that arrives as dozens of requests.
const (
	writeBurst       = 20
	writeRefillEach  = 3 * time.Second
	uploadBurst      = 60
	uploadRefillEach = time.Second
)

// Bounds on the bucket map. Unauthenticated keys are chosen by the caller --
// any address it can source from, and behind a trusted proxy any address it
// can get the proxy to append -- so the map must not grow with what an
// attacker sends. Past maxBuckets the limiter is in a degraded mode: new
// keys share overflowShards buckets, picked by a per-limiter seeded hash, so a
// flood of fresh addresses throttles only other newcomers, never a caller who
// already holds a bucket. Sharding means one attacker draining the buckets its
// keys land in still leaves most newcomers a bucket of their own to share, and
// the seed means it cannot aim its keys at the shard a given victim lands in. Pruning walks the whole map, so it runs
// at most once per pruneEvery, and only once the map is worth pruning.
const (
	maxBuckets     = 10000
	overflowShards = 64
	pruneAbove     = 1024
	pruneEvery     = time.Minute
	// pruneIdle is how long a bucket must sit untouched before it goes. Every
	// bucket refills in well under this, so an idle one is full and dropping
	// it forgets nothing.
	pruneIdle = time.Hour
)

// bucket is a classic token bucket, refilled lazily on take.
type bucket struct {
	tokens float64
	last   time.Time
}

// limiter holds one bucket per caller key, up to maxBuckets, plus the shared
// overflow buckets for keys that arrive once the map is full.
type limiter struct {
	mu        sync.Mutex
	buckets   map[string]*bucket
	overflow  [overflowShards]*bucket
	seed      maphash.Seed
	lastPrune time.Time
	burst     float64
	refill    time.Duration
	// now is injectable for tests.
	now func() time.Time
}

func newLimiter() *limiter {
	return newLimiterSized(writeBurst, writeRefillEach)
}

func newLimiterSized(burst float64, refill time.Duration) *limiter {
	return &limiter{buckets: map[string]*bucket{}, seed: maphash.MakeSeed(), burst: burst, refill: refill, now: time.Now}
}

// take reports whether the caller may proceed, consuming one token if so.
func (l *limiter) take(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		// Prune before deciding on overflow, so a newcomer gets the room an
		// idle bucket was holding.
		l.prune(now)
		if len(l.buckets) < maxBuckets {
			b = &bucket{tokens: l.burst, last: now}
			l.buckets[key] = b
		} else {
			i := l.shard(key)
			if l.overflow[i] == nil {
				l.overflow[i] = &bucket{tokens: l.burst, last: now}
			}
			b = l.overflow[i]
		}
	}

	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()/l.refill.Seconds())
	b.last = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// shard picks the overflow bucket a key shares once the map is full.
func (l *limiter) shard(key string) int {
	return int(maphash.String(l.seed, key) % overflowShards)
}

// prune drops buckets idle for pruneIdle, at most once per pruneEvery. Idle
// is judged by last use rather than stored tokens: a bucket drained on its
// last take still shows zero tokens, though it refilled long ago, and keying
// on that would let a caller who drained many keys pin the map at the cap.
func (l *limiter) prune(now time.Time) {
	if len(l.buckets) <= pruneAbove || now.Sub(l.lastPrune) < pruneEvery {
		return
	}
	l.lastPrune = now
	for k, old := range l.buckets {
		if now.Sub(old.last) > pruneIdle {
			delete(l.buckets, k)
		}
	}
}

// limiterKey identifies the caller: the authenticated user when there is one,
// otherwise the client address -- the one clientIPMiddleware resolved behind
// a trusted proxy, else the remote host, which is the best available boundary
// on an auth-disabled localhost deployment.
//
// IPv6 addresses are keyed by their /64. One host is routinely handed a whole
// /64, so per-address keys would give a single caller 2^64 fresh buckets.
func limiterKey(r *http.Request) string {
	if id, ok := auth.FromContext(r.Context()); ok && id.UserID != "" {
		return "u:" + id.UserID
	}
	if a, ok := clientAddrFrom(r.Context()); ok {
		return "a:" + addrKey(a)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if a, ok := canonicalAddr(host); ok {
		return "a:" + addrKey(a)
	}
	return "a:" + host
}

// addrKey renders a canonical address as a bucket key: IPv4 as the address,
// IPv6 as its /64.
func addrKey(a netip.Addr) string {
	if a.Is6() {
		return netip.PrefixFrom(a, 64).Masked().String()
	}
	return a.String()
}

// writeLimiter rejects callers who have exhausted the server's bucket with a
// 429. The limiter lives on the Server rather than in a package global so
// parallel test servers do not drain each other's buckets.
func writeLimiter(l *limiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !l.take(limiterKey(r)) {
				w.Header().Set("Retry-After", "3")
				writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "write rate limit exceeded"})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
