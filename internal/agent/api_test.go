package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// capturedRequest is what the fake API server saw.
type capturedRequest struct {
	Model     string          `json:"model"`
	MaxTokens int64           `json:"max_tokens"`
	System    json.RawMessage `json:"system"`
	Messages  json.RawMessage `json:"messages"`
	Output    struct {
		Effort string `json:"effort"`
		Format struct {
			Type   string         `json:"type"`
			Schema map[string]any `json:"schema"`
		} `json:"format"`
	} `json:"output_config"`
}

// apiServer stands in for the Anthropic API. Tests assert on the request the
// SDK actually produced, so wire-level mistakes surface here rather than at
// runtime against a real endpoint.
func apiServer(t *testing.T, respond func(w http.ResponseWriter, req capturedRequest)) (*APIRunner, *capturedRequest) {
	t.Helper()

	var seen capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		if err := json.Unmarshal(body, &seen); err != nil {
			t.Errorf("decode body: %v (%s)", err, body)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		respond(w, seen)
	}))
	t.Cleanup(srv.Close)

	return NewAPIRunner(APIOptions{APIKey: "test-key", BaseURL: srv.URL}), &seen
}

// messageJSON builds a minimal successful API response.
func messageJSON(text, stopReason string, usage map[string]any) string {
	base := map[string]any{
		"id":            "msg_test",
		"type":          "message",
		"role":          "assistant",
		"model":         "claude-sonnet-5",
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"content":       []any{map[string]any{"type": "text", "text": text}},
		"usage": map[string]any{
			"input_tokens": 100, "output_tokens": 50,
			"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
		},
	}
	if usage != nil {
		base["usage"] = usage
	}
	b, _ := json.Marshal(base)
	return string(b)
}

const validGeneration = `{"pages":[{"path":"entities/ripple.md","type":"entity","title":"Ripple","body":"# Ripple\n\nDispatches tasks."}]}`

// generateReq keeps MaxTokens under streamingThreshold so these tests exercise
// the non-streaming path against a plain JSON server. Streaming has its own test.
func generateReq() Request {
	return Request{
		Step: StepGenerate, Prompt: "write the pages",
		SystemPrompt: "you are a technical writer",
		MaxTokens:    4_000,
		Timeout:      10 * time.Second,
	}
}

func TestAPIRunnerParsesStructuredGeneration(t *testing.T) {
	r, _ := apiServer(t, func(w http.ResponseWriter, _ capturedRequest) {
		io.WriteString(w, messageJSON(validGeneration, "end_turn", nil))
	})

	res, err := r.Run(context.Background(), generateReq())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Succeeded() {
		t.Fatalf("Succeeded() = false: %+v", res)
	}

	// The whole point of structured output: page content arrives as data, so no
	// filesystem is involved and there is nothing to collect or path-check.
	if res.Generation == nil || len(res.Generation.Pages) != 1 {
		t.Fatalf("Generation = %+v, want one page", res.Generation)
	}
	page := res.Generation.Pages[0]
	if page.Path != "entities/ripple.md" || page.Type != "entity" {
		t.Errorf("page = %+v", page)
	}
	if !strings.Contains(page.Body, "# Ripple") {
		t.Errorf("body = %q", page.Body)
	}
}

func TestAPIRunnerSendsStructuredOutputSchema(t *testing.T) {
	r, seen := apiServer(t, func(w http.ResponseWriter, _ capturedRequest) {
		io.WriteString(w, messageJSON(validGeneration, "end_turn", nil))
	})

	if _, err := r.Run(context.Background(), generateReq()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if seen.Output.Format.Type != "json_schema" {
		t.Errorf("format type = %q, want json_schema", seen.Output.Format.Type)
	}
	if seen.Output.Format.Schema == nil {
		t.Fatal("no schema was sent; the model could return anything")
	}
	// additionalProperties:false is what stops the model inventing fields that
	// silently vanish on decode.
	if allow, ok := seen.Output.Format.Schema["additionalProperties"].(bool); !ok || allow {
		t.Error("schema should set additionalProperties:false")
	}
}

func TestAPIRunnerStreamsAboveThreshold(t *testing.T) {
	// A large output ceiling must stream: a non-streaming request at that size
	// risks an HTTP timeout, and a whole batch of page bodies is exactly that
	// size. Serving plain JSON here would yield an empty message.
	var streamed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var seen struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &seen)
		streamed = seen.Stream

		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range sseEvents(validGeneration) {
			io.WriteString(w, ev)
		}
	}))
	defer srv.Close()

	r := NewAPIRunner(APIOptions{APIKey: "test-key", BaseURL: srv.URL})

	req := generateReq()
	req.MaxTokens = 32_000 // above streamingThreshold
	res, err := r.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !streamed {
		t.Error("a 32k-token request was sent non-streaming; it risks an HTTP timeout")
	}
	if res.Generation == nil || len(res.Generation.Pages) != 1 {
		t.Fatalf("streamed response did not accumulate: %+v", res.Generation)
	}
	if res.Usage.OutputTokens == 0 {
		t.Error("usage was not accumulated from the stream")
	}
}

// sseEvents builds a minimal well-formed message stream.
func sseEvents(text string) []string {
	start := `{"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","model":"claude-sonnet-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":100,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`
	blockStart := `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
	delta, _ := json.Marshal(map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
	blockStop := `{"type":"content_block_stop","index":0}`
	msgDelta := `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":50}}`
	msgStop := `{"type":"message_stop"}`

	var out []string
	for _, payload := range []string{start, blockStart, string(delta), blockStop, msgDelta, msgStop} {
		var typed struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(payload), &typed)
		out = append(out, "event: "+typed.Type+"\ndata: "+payload+"\n\n")
	}
	return out
}

func TestAPIRunnerAnalyzeUsesAnalysisSchema(t *testing.T) {
	analysis := `{"pages":[{"path":"concepts/dispatch.md","type":"concept","title":"Dispatch","summary":"how work is routed"}],"findings":["uses a work queue"]}`

	r, _ := apiServer(t, func(w http.ResponseWriter, _ capturedRequest) {
		io.WriteString(w, messageJSON(analysis, "end_turn", nil))
	})

	res, err := r.Run(context.Background(), Request{
		Step: StepAnalyze, Prompt: "analyze",
		MaxTokens: 4_000, Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Analysis == nil || len(res.Analysis.Pages) != 1 {
		t.Fatalf("Analysis = %+v", res.Analysis)
	}
	if res.Generation != nil {
		t.Error("analyze step should not produce a generation payload")
	}
}

func TestAPIRunnerPlacesCacheBreakpointOnSharedContext(t *testing.T) {
	r, seen := apiServer(t, func(w http.ResponseWriter, _ capturedRequest) {
		io.WriteString(w, messageJSON(validGeneration, "end_turn", nil))
	})

	req := generateReq()
	req.CacheableContext = "REPO MAP: modules, entry points, dependency edges"
	if _, err := r.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var blocks []struct {
		Text         string          `json:"text"`
		CacheControl json.RawMessage `json:"cache_control"`
	}
	if err := json.Unmarshal(seen.System, &blocks); err != nil {
		t.Fatalf("system should be a block array: %v (%s)", err, seen.System)
	}
	if len(blocks) != 2 {
		t.Fatalf("got %d system blocks, want 2", len(blocks))
	}

	// Caching is a prefix match, so the breakpoint must sit on the last block
	// shared across units. On the first block it would cache nothing useful.
	if blocks[0].CacheControl != nil {
		t.Error("breakpoint on the first block caches only the system prompt")
	}
	if blocks[1].CacheControl == nil {
		t.Error("no breakpoint on the shared context; every unit would re-pay full input price")
	}
	if !strings.Contains(blocks[1].Text, "REPO MAP") {
		t.Errorf("shared context is not last: %q", blocks[1].Text)
	}
}

func TestAPIRunnerReportsCacheSavings(t *testing.T) {
	usage := map[string]any{
		"input_tokens": 500, "output_tokens": 200,
		"cache_creation_input_tokens": 0, "cache_read_input_tokens": 20000,
	}
	r, _ := apiServer(t, func(w http.ResponseWriter, _ capturedRequest) {
		io.WriteString(w, messageJSON(validGeneration, "end_turn", usage))
	})

	res, err := r.Run(context.Background(), generateReq())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Usage.CacheReadInputTokens != 20000 {
		t.Errorf("cache reads = %d, want 20000", res.Usage.CacheReadInputTokens)
	}

	// 20k cached tokens must cost far less than 20k fresh ones, or the caching
	// design is not actually paying for itself.
	cached := EstimateCostUSD(ModelSonnet5, res.Usage)
	fresh := EstimateCostUSD(ModelSonnet5, Usage{InputTokens: 20500, OutputTokens: 200})
	if cached >= fresh {
		t.Errorf("cached run cost %.4f, uncached %.4f; caching saved nothing", cached, fresh)
	}
}

func TestAPIRunnerHandlesRefusal(t *testing.T) {
	// A refusal is a successful HTTP response, so checking transport alone would
	// treat a declined request as a completed one.
	r, _ := apiServer(t, func(w http.ResponseWriter, _ capturedRequest) {
		io.WriteString(w, messageJSON("", "refusal", nil))
	})

	res, err := r.Run(context.Background(), generateReq())
	if err != nil {
		t.Fatalf("Run returned a transport error for a refusal: %v", err)
	}
	if res.Succeeded() {
		t.Error("a refusal was reported as success")
	}
	if res.Generation != nil {
		t.Error("a refusal should carry no pages")
	}
	if !strings.Contains(res.Err().Error(), "declined") {
		t.Errorf("Err() should explain the refusal, got %v", res.Err())
	}
}

func TestAPIRunnerHandlesTruncation(t *testing.T) {
	// Truncation mid-JSON produces unparseable output; reporting it as a ceiling
	// problem is what tells the caller to raise MaxTokens rather than retry blind.
	r, _ := apiServer(t, func(w http.ResponseWriter, _ capturedRequest) {
		io.WriteString(w, messageJSON(`{"pages":[{"path":"entities/a.md"`, "max_tokens", nil))
	})

	res, err := r.Run(context.Background(), generateReq())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Succeeded() {
		t.Error("a truncated response was reported as success")
	}
	if !strings.Contains(res.Result, "output ceiling") {
		t.Errorf("Result should name the ceiling, got %q", res.Result)
	}
}

func TestAPIRunnerHandlesMalformedJSON(t *testing.T) {
	r, _ := apiServer(t, func(w http.ResponseWriter, _ capturedRequest) {
		io.WriteString(w, messageJSON("this is not json at all", "end_turn", nil))
	})

	res, err := r.Run(context.Background(), generateReq())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Succeeded() {
		t.Error("undecodable output was reported as success")
	}
	if res.Subtype != "decode_error" {
		t.Errorf("Subtype = %q, want decode_error", res.Subtype)
	}
}

func TestAPIRunnerUsesDefaultModel(t *testing.T) {
	r, seen := apiServer(t, func(w http.ResponseWriter, _ capturedRequest) {
		io.WriteString(w, messageJSON(validGeneration, "end_turn", nil))
	})

	if _, err := r.Run(context.Background(), generateReq()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if seen.Model != DefaultModel {
		t.Errorf("model = %q, want the %q default", seen.Model, DefaultModel)
	}
}

func TestAPIRunnerRequestModelOverrides(t *testing.T) {
	r, seen := apiServer(t, func(w http.ResponseWriter, _ capturedRequest) {
		io.WriteString(w, messageJSON(validGeneration, "end_turn", nil))
	})

	req := generateReq()
	req.Model = ModelOpus5
	if _, err := r.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if seen.Model != ModelOpus5 {
		t.Errorf("model = %q, want the per-request override %q", seen.Model, ModelOpus5)
	}
}

func TestAPIRunnerRejectsInvalidRequests(t *testing.T) {
	r, _ := apiServer(t, func(w http.ResponseWriter, _ capturedRequest) {
		io.WriteString(w, messageJSON(validGeneration, "end_turn", nil))
	})

	for _, tt := range []struct {
		name string
		req  Request
	}{
		{"empty prompt", Request{Step: StepGenerate}},
		{"unknown step", Request{Step: "invent", Prompt: "x"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := r.Run(context.Background(), tt.req); err == nil {
				t.Error("Run accepted an invalid request")
			}
		})
	}
}

func TestEstimateCostUSD(t *testing.T) {
	// 1M input + 1M output on Sonnet 5 is $3 + $15.
	got := EstimateCostUSD(ModelSonnet5, Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	if want := 18.0; got != want {
		t.Errorf("Sonnet 5 = %.2f, want %.2f", got, want)
	}

	// Opus 5 is $5 + $25.
	got = EstimateCostUSD(ModelOpus5, Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	if want := 30.0; got != want {
		t.Errorf("Opus 5 = %.2f, want %.2f", got, want)
	}

	// An unknown model reports zero rather than a wrong number; budget code
	// treats that as unknown, not free.
	if got := EstimateCostUSD("claude-invented", Usage{InputTokens: 1_000_000}); got != 0 {
		t.Errorf("unknown model = %.2f, want 0", got)
	}
	if KnownModel("claude-invented") {
		t.Error("KnownModel accepted an invented model")
	}
}

func TestDefaultModelIsSonnet(t *testing.T) {
	// The cheap model is the default and the expensive one is opt-in; flipping
	// this would silently multiply every workspace's bill.
	if DefaultModel != ModelSonnet5 {
		t.Errorf("DefaultModel = %q, want %q", DefaultModel, ModelSonnet5)
	}
	if EscalationModel != ModelOpus5 {
		t.Errorf("EscalationModel = %q, want %q", EscalationModel, ModelOpus5)
	}
	if modelPricing[ModelSonnet5].InputPerMTok >= modelPricing[ModelOpus5].InputPerMTok {
		t.Error("the default model should be the cheaper one")
	}
}

func TestSchemasConstrainPagePaths(t *testing.T) {
	for name, schema := range map[string]map[string]any{
		"analysis":   AnalysisSchemaJSON(),
		"generation": GenerationSchemaJSON(),
	} {
		t.Run(name, func(t *testing.T) {
			pages := schema["properties"].(map[string]any)["pages"].(map[string]any)
			props := pages["items"].(map[string]any)["properties"].(map[string]any)

			// The schema rejects an out-of-bounds path before it costs anything,
			// though Go re-checks it since a schema is not a security boundary.
			pattern, _ := props["path"].(map[string]any)["pattern"].(string)
			if !strings.Contains(pattern, "entities|concepts") {
				t.Errorf("path pattern does not constrain the directory: %q", pattern)
			}

			types := props["type"].(map[string]any)["enum"].([]any)
			for _, ty := range types {
				if ty == "overview" {
					t.Error("overview is derived from frontmatter and must not be agent-writable")
				}
			}
		})
	}
}
