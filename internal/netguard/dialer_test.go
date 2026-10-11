package netguard

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeNet scripts the dialer's two seams: each lookup returns the next
// answer, and each dial is recorded and answered by dialFn.
type fakeNet struct {
	mu      sync.Mutex
	answers [][]net.IP
	lookErr error
	dialed  []string
	dialFn  func(ctx context.Context, addr string) (net.Conn, error)
}

func (f *fakeNet) dialer(allowPrivate bool) *dialer {
	d := newDialer(Options{AllowPrivate: allowPrivate})
	d.lookup = func(_ context.Context, _, _ string) ([]net.IP, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.lookErr != nil {
			return nil, f.lookErr
		}
		if len(f.answers) == 0 {
			return nil, nil
		}
		ips := f.answers[0]
		if len(f.answers) > 1 {
			f.answers = f.answers[1:]
		}
		return ips, nil
	}
	d.dial = func(ctx context.Context, _, addr string) (net.Conn, error) {
		f.mu.Lock()
		f.dialed = append(f.dialed, addr)
		fn := f.dialFn
		f.mu.Unlock()
		if fn == nil {
			return pipeConn(), nil
		}
		return fn(ctx, addr)
	}
	return d
}

func (f *fakeNet) dials() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.dialed...)
}

func ips(ss ...string) []net.IP {
	out := make([]net.IP, len(ss))
	for i, s := range ss {
		out[i] = net.ParseIP(s)
	}
	return out
}

// pipeConn is a connected net.Conn with nothing behind it.
func pipeConn() net.Conn {
	a, b := net.Pipe()
	_ = b.Close()
	return a
}

func TestDialPinsTheVettedAddressAndRechecksEveryDial(t *testing.T) {
	// DNS rebinding: the first answer is public, the second loopback. The
	// first dial must go to the literal that was vetted (not the name, which
	// would re-resolve), and the second must be refused before any dial.
	f := &fakeNet{answers: [][]net.IP{ips("8.8.8.8"), ips("127.0.0.1")}}
	d := f.dialer(false)

	conn, err := d.DialContext(context.Background(), "tcp", "rebind.example:443")
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	_ = conn.Close()
	if got := f.dials(); len(got) != 1 || got[0] != "8.8.8.8:443" {
		t.Fatalf("dialed %v, want the vetted literal 8.8.8.8:443", got)
	}

	_, err = d.DialContext(context.Background(), "tcp", "rebind.example:443")
	if !errors.Is(err, ErrForbiddenAddress) {
		t.Fatalf("second dial = %v, want ErrForbiddenAddress", err)
	}
	if got := f.dials(); len(got) != 1 {
		t.Fatalf("dialed %v after the rebind, want no new dial", got)
	}
}

func TestDialRefusesAMixedAnswer(t *testing.T) {
	// One forbidden answer refuses the host, even with a public one first
	// that would have been dialed under an "any public" rule.
	for name, answer := range map[string][]net.IP{
		"public then private":  ips("8.8.8.8", "10.0.0.1"),
		"public A, link-local": ips("8.8.8.8", "fe80::1"),
		"private first":        ips("169.254.169.254", "1.1.1.1"),
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeNet{answers: [][]net.IP{answer}}
			_, err := f.dialer(false).DialContext(context.Background(), "tcp", "mixed.example:443")
			if !errors.Is(err, ErrForbiddenAddress) {
				t.Fatalf("dial = %v, want ErrForbiddenAddress", err)
			}
			if !strings.Contains(err.Error(), "non-public") {
				t.Errorf("error %q lacks the non-public wording callers match on", err)
			}
			for _, ip := range answer {
				if strings.Contains(err.Error(), ip.String()) {
					t.Errorf("error %q echoes the resolved address %s to the caller", err, ip)
				}
			}
			if got := f.dials(); len(got) != 0 {
				t.Fatalf("dialed %v, want no dial", got)
			}
		})
	}
}

func TestDialFailsClosedWithoutAnAnswer(t *testing.T) {
	t.Run("empty answer", func(t *testing.T) {
		f := &fakeNet{}
		if _, err := f.dialer(false).DialContext(context.Background(), "tcp", "empty.example:443"); err == nil {
			t.Fatal("dial with no addresses succeeded")
		}
		if got := f.dials(); len(got) != 0 {
			t.Fatalf("dialed %v, want no dial", got)
		}
	})
	t.Run("resolver error", func(t *testing.T) {
		boom := errors.New("servfail")
		f := &fakeNet{lookErr: boom}
		_, err := f.dialer(false).DialContext(context.Background(), "tcp", "broken.example:443")
		if !errors.Is(err, boom) {
			t.Fatalf("dial = %v, want the resolver error", err)
		}
		if got := f.dials(); len(got) != 0 {
			t.Fatalf("dialed %v, want no dial", got)
		}
	})
	t.Run("no port", func(t *testing.T) {
		f := &fakeNet{answers: [][]net.IP{ips("8.8.8.8")}}
		if _, err := f.dialer(false).DialContext(context.Background(), "tcp", "noport.example"); err == nil {
			t.Fatal("dial without a port succeeded")
		}
	})
}

func TestDialTriesVettedAddressesInOrder(t *testing.T) {
	refused := errors.New("connection refused")
	f := &fakeNet{answers: [][]net.IP{ips("8.8.8.8", "2606:4700:4700::1111", "1.1.1.1")}}
	f.dialFn = func(_ context.Context, addr string) (net.Conn, error) {
		if addr == "8.8.8.8:443" {
			return nil, refused
		}
		return pipeConn(), nil
	}
	conn, err := f.dialer(false).DialContext(context.Background(), "tcp", "two.example:443")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Close()
	want := []string{"8.8.8.8:443", "[2606:4700:4700::1111]:443"}
	if got := f.dials(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("dialed %v, want %v", got, want)
	}
}

func TestDialTriesAtMostFourAddressesAndJoinsTheErrors(t *testing.T) {
	f := &fakeNet{answers: [][]net.IP{ips("1.0.0.1", "1.0.0.2", "1.0.0.3", "1.0.0.4", "1.0.0.5", "1.0.0.6")}}
	f.dialFn = func(_ context.Context, addr string) (net.Conn, error) {
		return nil, errors.New("refused " + addr)
	}
	_, err := f.dialer(false).DialContext(context.Background(), "tcp", "many.example:443")
	if err == nil {
		t.Fatal("dial succeeded with every address refusing")
	}
	if got := f.dials(); len(got) != maxDialAttempts {
		t.Fatalf("dialed %d addresses (%v), want %d", len(got), got, maxDialAttempts)
	}
	for _, addr := range []string{"1.0.0.1:443", "1.0.0.4:443"} {
		if !strings.Contains(err.Error(), "refused "+addr) {
			t.Errorf("error %q is missing the attempt on %s", err, addr)
		}
	}
}

func TestDialSplitsTheBudgetSoABlackholedAddressCannotSpendIt(t *testing.T) {
	// The first address never answers. With the whole budget handed to it,
	// the second would never be tried; with an even share, it is.
	const budget = time.Second
	f := &fakeNet{answers: [][]net.IP{ips("8.8.8.8", "1.1.1.1")}}
	f.dialFn = func(ctx context.Context, addr string) (net.Conn, error) {
		if addr == "8.8.8.8:443" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return pipeConn(), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	conn, err := f.dialer(false).DialContext(ctx, "tcp", "blackhole.example:443")
	if err != nil {
		t.Fatalf("dial = %v, want the second address to be reached", err)
	}
	_ = conn.Close()
	if elapsed := time.Since(start); elapsed > budget*3/4 {
		t.Errorf("reaching the second address took %v, want about half the %v budget", elapsed, budget)
	}
	if got := f.dials(); len(got) != 2 || got[1] != "1.1.1.1:443" {
		t.Fatalf("dialed %v, want both addresses in order", got)
	}
}

func TestDialAttemptDeadlineIsCappedAndShared(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name      string
		budget    time.Duration // 0: no deadline on the context
		remaining int
		want      time.Duration
	}{
		{"no deadline gets the dial timeout", 0, 2, dialTimeout},
		{"a long budget is capped at the dial timeout", time.Minute, 2, dialTimeout},
		{"a 10s budget over two addresses", 10 * time.Second, 2, 5 * time.Second},
		{"the last address gets what is left", 3 * time.Second, 1, 3 * time.Second},
		{"four addresses share a 30s budget", 30 * time.Second, 4, 7500 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.budget > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, now.Add(tt.budget))
				defer cancel()
			}
			if got := attemptDeadline(ctx, now, tt.remaining).Sub(now); got != tt.want {
				t.Errorf("attempt gets %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDialStopsWhenTheContextIsDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeNet{answers: [][]net.IP{ips("8.8.8.8", "1.1.1.1")}}
	f.dialFn = func(_ context.Context, _ string) (net.Conn, error) {
		cancel() // the caller gives up while the first attempt is failing
		return nil, errors.New("refused")
	}
	_, err := f.dialer(false).DialContext(ctx, "tcp", "gone.example:443")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("dial = %v, want it to report the cancellation", err)
	}
	if got := f.dials(); len(got) != 1 {
		t.Fatalf("dialed %v after cancellation, want only the first attempt", got)
	}
}

func TestDialAllowPrivateReachesLoopback(t *testing.T) {
	// The real resolver and dialer, end to end: the test seam lets a
	// loopback listener through, and the default refuses it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	conn, err := DialContext(Options{AllowPrivate: true})(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("AllowPrivate dial to loopback: %v", err)
	}
	_ = conn.Close()

	if _, err := DialContext(Options{})(context.Background(), "tcp", ln.Addr().String()); !errors.Is(err, ErrForbiddenAddress) {
		t.Fatalf("guarded dial to loopback = %v, want ErrForbiddenAddress", err)
	}
}
