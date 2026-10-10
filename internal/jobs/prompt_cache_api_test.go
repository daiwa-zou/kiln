package jobs

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/daiwa-zou/kiln/internal/agent"
)

// The API-shaped half of the prompt-cache layout: the pipeline asks a runner that
// caches the unit for its source material in Request.CacheableUnit. The wire
// half is asserted in internal/agent.

// cachingRunner stands in for the API runner: it asks for the unit's source
// material in Request.CacheableUnit, records every request, and returns a page
// that fails validation so the unit retries.
type cachingRunner struct {
	mu    sync.Mutex
	calls []agent.Request
}

func (r *cachingRunner) CachesUnit() bool { return true }

func (r *cachingRunner) Run(_ context.Context, req agent.Request) (*agent.Result, error) {
	if err := req.Validate(agent.StandInCapabilities); err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.calls = append(r.calls, req)
	r.mu.Unlock()

	res := &agent.Result{Subtype: "success", TerminalReason: "completed", NumTurns: 1, TotalCostUSD: 0.01}
	switch req.Step {
	case agent.StepAnalyze:
		res.Analysis = &agent.AnalysisResult{Pages: []agent.AnalysisPage{{
			Path: "entities/ripple.md", Type: "entity", Title: "Ripple", Summary: "dispatcher",
		}}}
	case agent.StepGenerate:
		res.Generation = &agent.GenerationResult{Pages: []agent.GeneratedPage{{
			Path: "entities/ripple.md", Type: "concept", Title: "Ripple",
			Body: "# Ripple\n\n" + strings.Repeat("Dispatches tasks across the fleet. ", 8),
		}}}
	}
	return res, nil
}

// TestCachingRunnerGetsOneSystemPromptAndOneUnitBlockForEveryCall: the pipeline
// half of the API runner's cache layout. The wire half is asserted in
// internal/agent against the request it produces from these fields.
func TestCachingRunnerGetsOneSystemPromptAndOneUnitBlockForEveryCall(t *testing.T) {
	runner := &cachingRunner{}
	p := testPipeline(newMemStore(), runner)
	p.MaxRetries = 2
	req := cacheTestRequest(t)
	req.ScratchDir = ""

	if _, err := p.Build(context.Background(), req); err != nil {
		t.Fatalf("Build: %v", err)
	}

	var unitCalls []agent.Request
	for _, c := range runner.calls {
		if strings.HasSuffix(c.SessionID, sanitize("module:ripple")) {
			unitCalls = append(unitCalls, c)
		}
	}
	if len(unitCalls) != 4 {
		t.Fatalf("got %d calls for the unit, want analyze plus three generate attempts", len(unitCalls))
	}

	first := unitCalls[0]
	if first.CacheableUnit == "" || !strings.Contains(first.CacheableUnit, sourceMarker) {
		t.Fatalf("the unit's source material is not in CacheableUnit:\n%q", first.CacheableUnit)
	}
	for i, c := range unitCalls {
		if c.SystemPrompt != first.SystemPrompt {
			t.Errorf("call %d (%s) has a different system prompt from analyze", i, c.Step)
		}
		if c.CacheableContext != first.CacheableContext {
			t.Errorf("call %d (%s) has a different shared map block", i, c.Step)
		}
		if c.CacheableUnit != first.CacheableUnit {
			t.Errorf("call %d (%s) has a different unit block, so nothing after the system prefix would cache", i, c.Step)
		}
		// The material is sent once, in the block. Repeating it in the prompt
		// would bill it at full rate on every call and defeat the breakpoint.
		if strings.Contains(c.Prompt, sourceMarker) || strings.Contains(c.Prompt, "## Source material") {
			t.Errorf("call %d (%s) repeats the cached material in its prompt:\n%s", i, c.Step, c.Prompt)
		}
		if !strings.Contains(c.Prompt, "first block of this message") {
			t.Errorf("call %d (%s) does not say where the material is", i, c.Step)
		}
	}
	// What differs between steps is the instructions, after the block.
	if !strings.Contains(unitCalls[0].Prompt, analyzeRuleMarker) || strings.Contains(unitCalls[0].Prompt, generateRuleMarker) {
		t.Error("analyze prompt does not carry exactly the analyze rules")
	}
	for i, c := range unitCalls[1:] {
		if !strings.Contains(c.Prompt, generateRuleMarker) || strings.Contains(c.Prompt, analyzeRuleMarker) {
			t.Errorf("generate attempt %d does not carry exactly the generate rules", i)
		}
	}
	if !strings.Contains(unitCalls[3].Prompt, "previous attempt failed validation") {
		t.Error("the retry feedback is not in the instructions after the cached block")
	}
}

// A runner that has not said it caches the unit is handed the material in the
// prompt of both steps, as before: moving it to a field the runner ignores
// would send it no source at all.
func TestRunnerThatDoesNotCacheTheUnitIsSentTheMaterialInThePrompt(t *testing.T) {
	runner := newScriptedRunner()
	runner.filesByAttempt[0] = map[string]string{"entities/ripple.md": validPage("entity", "Ripple")}

	if _, err := testPipeline(newMemStore(), runner).Build(context.Background(), cacheTestRequest(t)); err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, c := range runner.calls {
		if !strings.HasSuffix(c.SessionID, sanitize("module:ripple")) {
			continue
		}
		if c.CacheableUnit != "" {
			t.Errorf("%s: CacheableUnit set for a runner that does not cache it", c.Step)
		}
		if !strings.Contains(c.Prompt, sourceMarker) {
			t.Errorf("%s: prompt carries no source material", c.Step)
		}
	}
}

// The system prompt is one function of the steering, not one per step.
func TestBuildSystemPromptIsStepNeutralAndKeepsTheSteering(t *testing.T) {
	s := Steering{Purpose: "Document on-call.", Schema: "Pages are short."}
	got := buildSystemPrompt(s)
	for _, want := range []string{"UNTRUSTED DATA", "Document on-call.", "Pages are short."} {
		if !strings.Contains(got, want) {
			t.Errorf("system prompt lost %q", want)
		}
	}
	for _, marker := range []string{generateRuleMarker, analyzeRuleMarker} {
		if strings.Contains(got, marker) {
			t.Errorf("system prompt carries step rules (%q)", marker)
		}
	}
	if buildSystemPrompt(s) != got {
		t.Error("system prompt is not deterministic")
	}
}
