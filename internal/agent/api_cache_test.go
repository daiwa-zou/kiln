package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// These tests pin the wire layout of the API runner's prompt cache. A cache is a
// prefix match on the rendered request, so what matters is which bytes are
// identical from one call to the next and where the breakpoints sit; the fake
// server below is only a way to read the request the SDK actually produced.

const (
	cacheSystem  = "you are a technical writer"
	cacheMap     = "REPO MAP: modules, entry points, dependency edges"
	cacheUnit    = "\n## Source material\n\n### \"main.go\"\n\n```\npackage main\n```\n"
	validPlan    = `{"pages":[{"path":"entities/ripple.md","type":"entity","title":"Ripple","summary":"dispatcher"}]}`
	cacheTimeout = 10 * time.Second
)

// wireCall is one request as the server received it.
type wireCall struct {
	system   []byte
	messages []byte
	raw      []byte
	schema   map[string]any
}

type userBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text"`
	CacheControl json.RawMessage `json:"cache_control"`
}

type wireMessage struct {
	Role    string      `json:"role"`
	Content []userBlock `json:"content"`
}

func (w wireCall) userBlocks(t *testing.T) []userBlock {
	t.Helper()
	var msgs []wireMessage
	if err := json.Unmarshal(w.messages, &msgs); err != nil {
		t.Fatalf("messages: %v (%s)", err, w.messages)
	}
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Fatalf("want one user message, got %s", w.messages)
	}
	return msgs[0].Content
}

// breakpoints counts cache_control markers anywhere in the request.
func (w wireCall) breakpoints(t *testing.T) int {
	t.Helper()
	var doc any
	if err := json.Unmarshal(w.raw, &doc); err != nil {
		t.Fatal(err)
	}
	var count func(v any) int
	count = func(v any) int {
		n := 0
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				if k == "cache_control" {
					n++
				}
				n += count(child)
			}
		case []any:
			for _, child := range x {
				n += count(child)
			}
		}
		return n
	}
	return count(doc)
}

// recordingAPI answers every request with whatever *reply holds and records each
// request. Each call decodes into a fresh struct and copies what it keeps, so
// no slot aliases another.
func recordingAPI(t *testing.T, reply *string) (*APIRunner, *[]wireCall) {
	t.Helper()
	var calls []wireCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		var seen capturedRequest
		if err := json.Unmarshal(body, &seen); err != nil {
			t.Errorf("decode body: %v", err)
			return
		}
		calls = append(calls, wireCall{
			system:   append([]byte(nil), seen.System...),
			messages: append([]byte(nil), seen.Messages...),
			raw:      body,
			schema:   seen.Output.Format.Schema,
		})
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, messageJSON(*reply, "end_turn", nil))
	}))
	t.Cleanup(srv.Close)
	return NewAPIRunner(APIOptions{APIKey: "test-key", BaseURL: srv.URL}), &calls
}

func cacheReq(step Step, prompt string) Request {
	return Request{
		Step: step, Prompt: prompt,
		SystemPrompt:     cacheSystem,
		CacheableContext: cacheMap,
		CacheableUnit:    cacheUnit,
		MaxTokens:        4_000, // under streamingThreshold: plain JSON server
		Timeout:          cacheTimeout,
	}
}

// run sends the three calls a unit that needs one retry makes.
func runUnitCalls(t *testing.T) []wireCall {
	t.Helper()
	var reply string
	r, calls := recordingAPI(t, &reply)

	steps := []struct {
		req   Request
		reply string
	}{
		{cacheReq(StepAnalyze, "Analyze this unit.\n\nUnit: \"module:ripple\"\n"), validPlan},
		{cacheReq(StepGenerate, "Write the pages.\n\n## The plan from your analysis\n\n"+validPlan), validGeneration},
		{cacheReq(StepGenerate, "Write the pages.\n\n## Your previous attempt failed validation\n\n- bad type\n"), validGeneration},
	}
	for _, s := range steps {
		reply = s.reply
		res, err := r.Run(context.Background(), s.req)
		if err != nil {
			t.Fatalf("%s: %v", s.req.Step, err)
		}
		if !res.Succeeded() {
			t.Fatalf("%s: %+v", s.req.Step, res)
		}
	}
	if len(*calls) != 3 {
		t.Fatalf("server saw %d requests, want 3", len(*calls))
	}
	return *calls
}

func TestAPIRunnerSharesOneCachedPrefixAcrossStepsAndRetries(t *testing.T) {
	calls := runUnitCalls(t)
	first := calls[0]

	for i, c := range calls {
		// System: one block array, byte-identical in every call. Tools and system
		// render first, so any difference here discards everything after it.
		if !bytes.Equal(c.system, first.system) {
			t.Errorf("call %d has a different system block:\n got: %s\nwant: %s", i, c.system, first.system)
		}

		blocks := c.userBlocks(t)
		if len(blocks) != 2 {
			t.Fatalf("call %d: %d user content blocks, want [unit material][instructions]", i, len(blocks))
		}
		// The unit block leads, carries the breakpoint, and is the same bytes in
		// analyze, generate and every retry.
		if blocks[0].Text != cacheUnit || blocks[0].CacheControl == nil {
			t.Errorf("call %d: leading block = %+v, want the unit material with cache_control", i, blocks[0])
		}
		if !reflect.DeepEqual(blocks[0], first.userBlocks(t)[0]) {
			t.Errorf("call %d: leading user block differs from analyze's", i)
		}
		// The instructions follow it, unmarked: they are what differs.
		if blocks[1].CacheControl != nil {
			t.Errorf("call %d: the instruction block carries a breakpoint; it differs every call and would be a pure write surcharge", i)
		}
		if strings.Contains(blocks[1].Text, "package main") {
			t.Errorf("call %d: the instructions repeat the unit material", i)
		}
	}

	// Retries keep the prefix and change only what follows it.
	if calls[1].userBlocks(t)[1].Text == calls[2].userBlocks(t)[1].Text {
		t.Error("the retry's instructions are the same as the first attempt's")
	}
}

func TestAPIRunnerStaysWithinTheBreakpointLimit(t *testing.T) {
	for i, c := range runUnitCalls(t) {
		// The system prefix and the unit block: two of the four the API allows.
		if got := c.breakpoints(t); got != 2 || got > maxCacheBreakpoints {
			t.Errorf("call %d carries %d breakpoints, want 2 (limit %d)", i, got, maxCacheBreakpoints)
		}
	}
}

func TestAPIRunnerSendsNoUnitBreakpointWithoutUnitMaterial(t *testing.T) {
	var reply = validGeneration
	r, calls := recordingAPI(t, &reply)

	req := cacheReq(StepGenerate, "Write the pages.")
	req.CacheableUnit = ""
	if _, err := r.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	c := (*calls)[0]
	blocks := c.userBlocks(t)
	if len(blocks) != 1 || blocks[0].CacheControl != nil || blocks[0].Text != "Write the pages." {
		t.Errorf("user content = %+v, want the prompt alone and unmarked", blocks)
	}
	// Only the system prefix is cached.
	if got := c.breakpoints(t); got != 1 {
		t.Errorf("%d breakpoints, want 1", got)
	}
}

// The two steps ask for different structured-output schemas. That is a documented
// reason cross-step cache hits may not happen on the API (see the note on
// APIRunner.buildUserMessage); this test records that the difference exists, so
// anyone who unifies the schemas knows the premise has changed.
func TestAPIRunnerStepsDifferInOutputSchema(t *testing.T) {
	calls := runUnitCalls(t)
	if reflect.DeepEqual(calls[0].schema, calls[1].schema) {
		t.Error("analyze and generate now share a schema; revisit the cross-step caching note")
	}
	if !reflect.DeepEqual(calls[1].schema, calls[2].schema) {
		t.Error("generate retries must share a schema, or the retry cannot read the first attempt's cache")
	}
}
