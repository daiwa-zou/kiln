package api

import (
	"net"
	"net/http"
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

// bucket is a classic token bucket, refilled lazily on take.
type bucket struct {
	tokens float64
	last   time.Time
}

// limiter holds one bucket per caller key. Entries are pruned opportunistically
// once they are full again and stale, so the map cannot grow without bound.
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	burst   float64
	refill  time.Duration
	// now is injectable for tests.
	now func() time.Time
}

func newLimiter() *limiter {
	return newLimiterSized(writeBurst, writeRefillEach)
}

func newLimiterSized(burst float64, refill time.Duration) *limiter {
	return &limiter{buckets: map[string]*bucket{}, burst: burst, refill: refill, now: time.Now}
}

// take reports whether the caller may proceed, consuming one token if so.
func (l *limiter) take(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}

	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()/l.refill.Seconds())
	b.last = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--

	// Opportunistic prune: full buckets untouched for an hour are dead weight.
	if len(l.buckets) > 1024 {
		for k, old := range l.buckets {
			if old.tokens >= l.burst-0.01 && now.Sub(old.last) > time.Hour {
				delete(l.buckets, k)
			}
		}
	}
	return true
}

// limiterKey identifies the caller: the authenticated user when there is one,
// otherwise the remote host, which is the best available boundary on an
// auth-disabled localhost deployment.
func limiterKey(r *http.Request) string {
	if id, ok := auth.FromContext(r.Context()); ok && id.UserID != "" {
		return "u:" + id.UserID
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return "a:" + host
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
