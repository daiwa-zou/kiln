package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Default output ceilings. Generation returns whole page bodies as JSON, so it
// needs considerably more room than a plan.
const (
	defaultAnalyzeMaxTokens  int64 = 8_000
	defaultGenerateMaxTokens int64 = 32_000
	// streamingThreshold is where non-streaming requests start risking an HTTP
	// timeout, so anything above it is streamed.
	streamingThreshold int64 = 16_000
)

// APIRunner calls the Anthropic API directly.
//
// It asks for structured output rather than letting the model write files: the
// model returns page content as JSON, Go validates it and writes the files. That
// removes the need for a sandbox, a write-guard hook, path-escape checks, and a
// post-run cleanliness verification -- the model has no filesystem to reach.
type APIRunner struct {
	client anthropic.Client

	// Model is the default; a request may override it per unit.
	Model string
	// Effort tunes thinking depth and overall spend.
	Effort anthropic.OutputConfigEffort
	// FallbackModel survives an overload without failing the run.
	FallbackModel string
}

// Capabilities: page content comes back as structured data, so no scratch
// directory is needed; cost is computed from the local pricing table in
// pricing.go, so budget enforcement must treat it as an estimate.
func (r *APIRunner) Capabilities() Capabilities {
	return Capabilities{WritesFiles: false, EstimatesCost: true}
}

// APIOptions configures an APIRunner.
type APIOptions struct {
	APIKey  string
	BaseURL string
	Model   string
	Effort  string
}

// NewAPIRunner builds a runner. An empty APIKey falls back to the SDK's own
// credential resolution, so ambient configuration keeps working.
func NewAPIRunner(opts APIOptions) *APIRunner {
	var clientOpts []option.RequestOption
	if opts.APIKey != "" {
		clientOpts = append(clientOpts, option.WithAPIKey(opts.APIKey))
	}
	if opts.BaseURL != "" {
		clientOpts = append(clientOpts, option.WithBaseURL(opts.BaseURL))
	}

	model := opts.Model
	if model == "" {
		model = DefaultModel
	}
	effort := anthropic.OutputConfigEffort(opts.Effort)
	if effort == "" {
		effort = anthropic.OutputConfigEffortHigh
	}

	return &APIRunner{
		client: anthropic.NewClient(clientOpts...),
		Model:  model,
		Effort: effort,
	}
}

// Run executes one request.
func (r *APIRunner) Run(ctx context.Context, req Request) (*Result, error) {
	if err := req.validateAPI(); err != nil {
		return nil, err
	}

	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	model := req.Model
	if model == "" {
		model = r.Model
	}

	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultAnalyzeMaxTokens
		if req.Step == StepGenerate {
			maxTokens = defaultGenerateMaxTokens
		}
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: maxTokens,
		System:    r.buildSystem(req),
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(req.Prompt)),
		},
		OutputConfig: anthropic.OutputConfigParam{
			Effort: r.Effort,
			Format: anthropic.JSONOutputFormatParam{Schema: schemaFor(req)},
		},
	}

	start := time.Now()
	msg, err := r.send(ctx, params, maxTokens)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("agent: %s step exceeded its %s timeout", req.Step, req.Timeout)
		}
		return nil, fmt.Errorf("agent: %s step: %w", req.Step, err)
	}

	return r.buildResult(req, msg, model, start)
}

// buildSystem splits the system prompt into a cacheable prefix and a volatile
// tail.
//
// Ordering is load-bearing: caching is a prefix match, so the content shared
// across units must come first and carry the breakpoint. Putting anything
// unit-specific ahead of it silently disables caching for the whole run.
func (r *APIRunner) buildSystem(req Request) []anthropic.TextBlockParam {
	var blocks []anthropic.TextBlockParam

	if strings.TrimSpace(req.SystemPrompt) != "" {
		blocks = append(blocks, anthropic.TextBlockParam{Text: req.SystemPrompt})
	}
	if strings.TrimSpace(req.CacheableContext) != "" {
		blocks = append(blocks, anthropic.TextBlockParam{Text: req.CacheableContext})
	}
	if len(blocks) == 0 {
		return nil
	}

	// The breakpoint goes on the last shared block, so tools and system cache
	// together and every later unit in the run reads rather than writes.
	blocks[len(blocks)-1].CacheControl = anthropic.NewCacheControlEphemeralParam()
	return blocks
}

func schemaFor(req Request) map[string]any {
	switch req.Step {
	case StepAnalyze:
		return AnalysisSchemaJSON()
	case StepResearch:
		return ResearchSchemaJSON()
	}
	return GenerationSchemaJSON()
}

// send streams when the output ceiling is high enough that a non-streaming
// request would risk an HTTP timeout.
//
// The SDK retries HTTP-level failures (429s, 5xxs) itself; what it cannot
// retry is a stream that dies mid-response, because partial output has no
// resume point. Those are retried here from scratch -- a network blip should
// cost one extra call, not the whole unit.
func (r *APIRunner) send(ctx context.Context, params anthropic.MessageNewParams, maxTokens int64) (*anthropic.Message, error) {
	if maxTokens <= streamingThreshold {
		return r.client.Messages.New(ctx, params)
	}

	const streamAttempts = 3
	var lastErr error
	for attempt := range streamAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, lastErr
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}

		stream := r.client.Messages.NewStreaming(ctx, params)
		msg := anthropic.Message{}
		accumulated := true
		for stream.Next() {
			if err := msg.Accumulate(stream.Current()); err != nil {
				lastErr = fmt.Errorf("accumulate stream: %w", err)
				accumulated = false
				break
			}
		}
		if !accumulated {
			continue
		}
		if err := stream.Err(); err != nil {
			if ctx.Err() != nil {
				// Cancellation and timeouts are not transient; stop retrying.
				return nil, err
			}
			lastErr = err
			continue
		}
		return &msg, nil
	}
	return nil, fmt.Errorf("stream failed after %d attempts: %w", streamAttempts, lastErr)
}

func (r *APIRunner) buildResult(req Request, msg *anthropic.Message, model string, start time.Time) (*Result, error) {
	res := &Result{
		Subtype:        "success",
		TerminalReason: "completed",
		StopReason:     string(msg.StopReason),
		Model:          string(msg.Model),
		NumTurns:       1,
		DurationMS:     time.Since(start).Milliseconds(),
		Usage: Usage{
			InputTokens:              int(msg.Usage.InputTokens),
			OutputTokens:             int(msg.Usage.OutputTokens),
			CacheCreationInputTokens: int(msg.Usage.CacheCreationInputTokens),
			CacheReadInputTokens:     int(msg.Usage.CacheReadInputTokens),
		},
	}
	// Price the model that actually served the request; a fallback-served
	// response priced at the requested model's rate would be wrong in both
	// directions. The requested id is only used when the served id is missing
	// or unknown to the table.
	priced := string(msg.Model)
	if !KnownModel(priced) {
		priced = model
	}
	res.TotalCostUSD = EstimateCostUSD(priced, res.Usage)
	if req.BudgetUSD > 0 && res.TotalCostUSD > req.BudgetUSD {
		res.OverBudget = true
	}

	// A refusal arrives as a successful HTTP response, so checking the transport
	// alone would treat a declined request as a completed one.
	if msg.StopReason == anthropic.StopReasonRefusal {
		res.IsError = true
		res.Subtype = "refusal"
		res.TerminalReason = "refusal"
		res.Result = refusalMessage(msg)
		return res, nil
	}

	// Truncation mid-JSON yields output that cannot be parsed. Reporting it as a
	// truncation rather than a decode failure is what tells the caller to raise
	// the ceiling instead of blaming the model.
	if msg.StopReason == anthropic.StopReasonMaxTokens {
		res.IsError = true
		res.Subtype = "max_tokens"
		res.Result = "response hit the output ceiling before completing; raise MaxTokens or split the unit"
		return res, nil
	}

	text := extractText(msg)
	if strings.TrimSpace(text) == "" {
		res.IsError = true
		res.Subtype = "empty_response"
		res.Result = "model returned no text content"
		return res, nil
	}
	res.Result = text

	switch req.Step {
	case StepAnalyze:
		parsed, err := ParseAnalysis(text)
		if err != nil {
			res.IsError = true
			res.Subtype = "decode_error"
			res.Result = fmt.Sprintf("decode analysis: %v", err)
			return res, nil
		}
		res.Analysis = parsed
	case StepGenerate:
		parsed, err := ParseGeneration(text)
		if err != nil {
			res.IsError = true
			res.Subtype = "decode_error"
			res.Result = fmt.Sprintf("decode generation: %v", err)
			return res, nil
		}
		res.Generation = parsed
	case StepResearch:
		parsed, err := ParseResearch(text)
		if err != nil {
			res.IsError = true
			res.Subtype = "decode_error"
			res.Result = fmt.Sprintf("decode research: %v", err)
			return res, nil
		}
		res.Research = parsed
	}

	return res, nil
}

func extractText(msg *anthropic.Message) string {
	var b strings.Builder
	for _, block := range msg.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

func refusalMessage(msg *anthropic.Message) string {
	if c := msg.StopDetails.Category; c != "" {
		return fmt.Sprintf("request declined by safety classifiers (category: %s)", c)
	}
	return "request declined by safety classifiers"
}

func (req Request) validateAPI() error {
	switch {
	case strings.TrimSpace(req.Prompt) == "":
		return errors.New("agent: Prompt is required")
	case req.Step != StepAnalyze && req.Step != StepGenerate && req.Step != StepResearch:
		return fmt.Errorf("agent: unknown step %q", req.Step)
	}
	return nil
}
