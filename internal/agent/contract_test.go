package agent

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The request contract is one function (Request.Validate) that every runner
// calls. These tests hold the runners to it from the outside, by running each
// one: a runner that grows its own idea of which steps are valid -- as the CLI
// runner did, rejecting "research" for a week of production -- fails here rather
// than when someone asks a question.

// contractAnalysis and contractResearch are the smallest payloads the API
// runner will parse for those steps, so a pass means the whole call succeeded
// and not merely that validation let it through.
const (
	contractAnalysis = `{"pages":[{"path":"concepts/dispatch.md","type":"concept","title":"Dispatch","summary":"how work is routed"}],"findings":[]}`
	contractResearch = `{"findings":"the answer","resolved":true,"evidence":[]}`
)

// contractRunners builds every Runner implementation behind one function that
// runs a request. current is the step the API stub should answer for.
func contractRunners(t *testing.T) []struct {
	name string
	run  func(Request) (*Result, error)
} {
	t.Helper()

	// The CLI runner is driven through the same fakeclaude binary the rest of the
	// suite uses, so the real validation, argv construction and exec path run.
	setScript(t, map[string]any{})
	cli := &ClaudeRunner{Binary: fakeClaude(t)}

	var current Step
	api, _ := apiServer(t, func(w http.ResponseWriter, _ capturedRequest) {
		body := validGeneration
		switch current {
		case StepAnalyze:
			body = contractAnalysis
		case StepResearch:
			body = contractResearch
		}
		io.WriteString(w, messageJSON(body, "end_turn", nil))
	})

	fake := NewFakeRunner(FakeOptions{})

	return []struct {
		name string
		run  func(Request) (*Result, error)
	}{
		{"cli", func(r Request) (*Result, error) { current = r.Step; return cli.Run(context.Background(), r) }},
		{"api", func(r Request) (*Result, error) { current = r.Step; return api.Run(context.Background(), r) }},
		{"fake", func(r Request) (*Result, error) { current = r.Step; return fake.Run(context.Background(), r) }},
	}
}

// contractRequest is a request valid for every runner at once: it carries the
// WorkDir and ScratchDir the strictest of them requires, which the others
// ignore.
func contractRequest(t *testing.T, step Step) Request {
	t.Helper()
	return Request{
		Step: step, SessionID: "contract-1",
		WorkDir: t.TempDir(), ScratchDir: t.TempDir(),
		Prompt:    "Unit: contract\n",
		MaxTokens: 4_000, Timeout: 30 * time.Second,
	}
}

func TestEveryRunnerAcceptsEveryValidStep(t *testing.T) {
	steps := []Step{StepAnalyze, StepGenerate, StepResearch}
	for _, s := range steps {
		if !s.Valid() {
			t.Fatalf("%q is in this test's list of steps but Step.Valid rejects it", s)
		}
	}

	for _, runner := range contractRunners(t) {
		for _, step := range steps {
			t.Run(runner.name+"/"+string(step), func(t *testing.T) {
				res, err := runner.run(contractRequest(t, step))
				if err != nil {
					t.Fatalf("a valid %s request was rejected: %v", step, err)
				}
				if !res.Succeeded() {
					t.Errorf("a valid %s request did not succeed: %+v", step, res)
				}
			})
		}
	}
}

func TestEveryRunnerRejectsAnUnknownStep(t *testing.T) {
	if Step("invent").Valid() || Step("").Valid() {
		t.Fatal("Step.Valid accepted a step that does not exist")
	}

	for _, runner := range contractRunners(t) {
		for _, step := range []Step{"invent", ""} {
			t.Run(runner.name+"/"+string(step), func(t *testing.T) {
				_, err := runner.run(contractRequest(t, step))
				if err == nil {
					t.Fatalf("%q was accepted", step)
				}
				if !strings.Contains(err.Error(), "unknown step") {
					t.Errorf("rejected for the wrong reason: %v", err)
				}
			})
		}
	}
}

// The requirements that differ between runners are the ones that drifted into
// production bugs, so they are pinned per runner: the file-working CLI runner
// insists on a WorkDir and a generate ScratchDir, the data-returning API runner
// on neither, and the stand-in on a WorkDir only.
func TestRunnersDifferOnlyInWhatTheyDeclare(t *testing.T) {
	for _, tt := range []struct {
		name         string
		caps         Capabilities
		needsWorkDir bool
		needsScratch bool
	}{
		{"cli", (&ClaudeRunner{}).Capabilities(), true, true},
		{"api", (&APIRunner{}).Capabilities(), false, false},
		{"stand-in", StandInCapabilities, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			noWorkDir := Request{Step: StepAnalyze, Prompt: "x"}
			if got := noWorkDir.Validate(tt.caps) != nil; got != tt.needsWorkDir {
				t.Errorf("empty WorkDir rejected = %v, want %v", got, tt.needsWorkDir)
			}
			noScratch := Request{Step: StepGenerate, WorkDir: "/src", Prompt: "x"}
			if got := noScratch.Validate(tt.caps) != nil; got != tt.needsScratch {
				t.Errorf("generate without ScratchDir rejected = %v, want %v", got, tt.needsScratch)
			}
			// Research writes nothing, so it never needs a ScratchDir.
			research := Request{Step: StepResearch, WorkDir: "/src", Prompt: "x"}
			if err := research.Validate(tt.caps); err != nil {
				t.Errorf("research without ScratchDir rejected: %v", err)
			}
		})
	}
}

// The wording is part of the contract: the UI shows these and earlier tests
// and docs quote them.
func TestValidateMessages(t *testing.T) {
	strict := Capabilities{WritesFiles: true, ReadsSources: true}
	for _, tt := range []struct {
		req  Request
		want string
	}{
		{Request{Step: StepAnalyze, Prompt: "x"}, "agent: WorkDir is required"},
		{Request{Step: StepAnalyze, WorkDir: "/src", Prompt: "  "}, "agent: Prompt is required"},
		{Request{Step: StepGenerate, WorkDir: "/src", Prompt: "x"}, "agent: ScratchDir is required for the generate step"},
		{Request{Step: "invent", WorkDir: "/src", Prompt: "x"}, `agent: unknown step "invent"`},
	} {
		err := tt.req.Validate(strict)
		if err == nil || err.Error() != tt.want {
			t.Errorf("Validate(%+v) = %v, want %q", tt.req, err, tt.want)
		}
	}
}
