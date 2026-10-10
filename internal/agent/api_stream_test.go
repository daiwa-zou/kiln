package agent

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// connTracker records which server-side connections have closed, keyed by the
// client's remote address.
type connTracker struct {
	mu     sync.Mutex
	closed map[string]bool
}

func (c *connTracker) track(conn net.Conn, state http.ConnState) {
	if state != http.StateClosed && state != http.StateHijacked {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed == nil {
		c.closed = map[string]bool{}
	}
	c.closed[conn.RemoteAddr().String()] = true
}

func (c *connTracker) isClosed(addr string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed[addr]
}

func sse(eventType, payload string) string {
	return "event: " + eventType + "\ndata: " + payload + "\n\n"
}

func TestAPIRunnerClosesFailedStreamAndCountsItsUsage(t *testing.T) {
	// The first attempt delivers message_start (which carries the usage billed
	// so far) and then an event the SDK cannot accumulate, while the server
	// keeps its side of the response open. Only the client closing the stream
	// can release that connection, so a leak shows up as a connection that never
	// closes. The second attempt succeeds.
	const (
		failedInput  = 120
		failedOutput = 7
	)
	failedStart := `{"type":"message_start","message":{"id":"msg_fail","type":"message","role":"assistant","model":"claude-sonnet-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":120,"output_tokens":7,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`
	// A delta for a block that was never started fails Accumulate.
	badDelta := `{"type":"content_block_delta","index":3,"delta":{"type":"text_delta","text":"x"}}`

	var (
		tracker   connTracker
		attempts  atomic.Int32
		firstAddr atomic.Value

		firstClosedByRetry atomic.Bool
	)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")

		if attempts.Add(1) == 1 {
			firstAddr.Store(r.RemoteAddr)
			_, _ = io.WriteString(w, sse("message_start", failedStart))
			_, _ = io.WriteString(w, sse("content_block_delta", badDelta))
			w.(http.Flusher).Flush()
			// Hold the response open until the client hangs up.
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
			return
		}
		// The retry is already underway. Run's own deadline cancels the request
		// context when it returns, which would hide a leak, so the first
		// connection has to be closed by now, before this response completes.
		addr, _ := firstAddr.Load().(string)
		deadline := time.Now().Add(3 * time.Second)
		for !tracker.isClosed(addr) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		firstClosedByRetry.Store(tracker.isClosed(addr))
		for _, ev := range sseEvents(validGeneration) {
			_, _ = io.WriteString(w, ev)
		}
	}))
	srv.Config.ConnState = tracker.track
	srv.Start()
	defer srv.Close()

	r := NewAPIRunner(APIOptions{APIKey: "test-key", BaseURL: srv.URL})
	req := generateReq()
	req.MaxTokens = 32_000 // above streamingThreshold, so the call streams
	res, err := r.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("server saw %d attempts, want 2", got)
	}
	if res.Generation == nil {
		t.Fatalf("second attempt did not produce a result: %+v", res)
	}

	// The abandoned attempt's connection must be released by the time the retry
	// is served, not left open until the server or the call's deadline ends it.
	if !firstClosedByRetry.Load() {
		t.Error("connection of the failed attempt was still open while the retry ran; the stream was not closed")
	}

	// The successful attempt reports 100 in / 50 out (see sseEvents); the failed
	// one was billed too and must be added on top.
	wantUsage := Usage{InputTokens: 100 + failedInput, OutputTokens: 50 + failedOutput}
	if res.Usage.InputTokens != wantUsage.InputTokens || res.Usage.OutputTokens != wantUsage.OutputTokens {
		t.Errorf("usage = %+v, want input %d output %d including the failed attempt",
			res.Usage, wantUsage.InputTokens, wantUsage.OutputTokens)
	}
	wantCost := EstimateCostUSD(ModelSonnet5, wantUsage)
	single := EstimateCostUSD(ModelSonnet5, Usage{InputTokens: 100, OutputTokens: 50})
	if res.TotalCostUSD <= single {
		t.Errorf("cost %.8f does not exceed a single clean attempt (%.8f); the failed attempt was not priced", res.TotalCostUSD, single)
	}
	if diff := res.TotalCostUSD - wantCost; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("cost = %.8f, want %.8f", res.TotalCostUSD, wantCost)
	}
}
