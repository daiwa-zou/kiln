package agent

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/daiwa-zou/kiln/internal/config"
)

// stubRunner is a provider registered from outside the built-in set, which is
// the whole point of the registry: reaching a different model should not
// require editing kiln.
type stubRunner struct {
	opts Options
	caps Capabilities
}

func (s *stubRunner) Run(context.Context, Request) (*Result, error) {
	return &Result{Subtype: "success", TerminalReason: "completed"}, nil
}

func (s *stubRunner) Capabilities() Capabilities { return s.caps }

// The seam is only real if a provider kiln has never heard of can be selected
// by configuration and reach the pipeline. Everything else here is detail.
func TestRegisteredProviderIsSelectableByConfig(t *testing.T) {
	const name = "test-llm"
	var built *stubRunner

	RegisterProvider(ProviderFunc(name, func(o Options) (Runner, error) {
		built = &stubRunner{opts: o, caps: Capabilities{WritesFiles: true}}
		return built, nil
	}))
	t.Cleanup(func() { unregisterProvider(name) })

	cfg := &config.Config{Agent: config.Agent{
		Runner:  config.AgentRunner(name),
		Model:   "some-other-model",
		BaseURL: "https://llm.example.com",
		Timeout: 90 * time.Second,
		Settings: map[string]any{
			"project": "acme",
			"nucleus": 0.9,
		},
	}}

	r, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if r != Runner(built) {
		t.Fatal("New returned a runner the provider did not build")
	}

	// Provider-neutral configuration arrives without the provider knowing
	// anything about kiln's config struct.
	if built.opts.Model != "some-other-model" || built.opts.BaseURL != "https://llm.example.com" {
		t.Errorf("options not passed through: %+v", built.opts)
	}
	if built.opts.Timeout != 90*time.Second {
		t.Errorf("Timeout = %v, want 90s", built.opts.Timeout)
	}

	// And provider-specific configuration arrives untouched, which is what
	// stops every new provider from widening config.Agent.
	if got, _ := built.opts.Settings.String("project"); got != "acme" {
		t.Errorf("settings string = %q, want acme", got)
	}
	if got, _ := built.opts.Settings.Float("nucleus"); got != 0.9 {
		t.Errorf("settings float = %v, want 0.9", got)
	}
}

// Capabilities were type assertions against the two built-in runners. A third
// could not state either fact, so the pipeline would allocate no scratch
// directory for the files it writes and would disable budget enforcement on
// the assumption its costs were authoritative.
func TestCapabilitiesComeFromTheRunnerNotItsType(t *testing.T) {
	writes := &stubRunner{caps: Capabilities{WritesFiles: true, EstimatesCost: false}}
	if !WritesFiles(writes) {
		t.Error("a runner that says it writes files was not believed")
	}
	if EstimatesCost(writes) {
		t.Error("a runner that reports real costs was treated as estimating")
	}

	estimates := &stubRunner{caps: Capabilities{WritesFiles: false, EstimatesCost: true}}
	if WritesFiles(estimates) {
		t.Error("a runner that returns structured data was given a scratch dir")
	}
	if !EstimatesCost(estimates) {
		t.Error("a runner that estimates cost was treated as authoritative")
	}
}

// The built-ins declare their own capabilities rather than being recognised by
// type. These are the facts the pipeline branches on, so they are asserted
// directly rather than left to the provider tests above.
func TestBuiltInRunnersDeclareTheirCapabilities(t *testing.T) {
	if !WritesFiles(NewClaudeRunner("claude")) {
		t.Error("the CLI runner writes files and must still be recognised")
	}
	if WritesFiles(NewAPIRunner(APIOptions{})) {
		t.Error("the API runner returns data and must not be given a scratch dir")
	}
	if !EstimatesCost(NewAPIRunner(APIOptions{})) {
		t.Error("the API runner's cost is a local estimate and must be flagged")
	}
	if EstimatesCost(NewClaudeRunner("claude")) {
		t.Error("the CLI reports its own cost and must not be treated as estimated")
	}
}

// A runner that does not implement CapableRunner still has to work: the
// pipeline's own test fakes do not, and requiring a method to say "nothing
// special" would be a tax on every one of them.
func TestRunnersThatDeclareNothingGetSaneDefaults(t *testing.T) {
	if WritesFiles(runnerFunc(nil)) {
		t.Error("a silent runner was given a scratch directory")
	}
	if EstimatesCost(runnerFunc(nil)) {
		t.Error("a silent runner's costs were treated as estimates, uncapping the run budget")
	}
}

// runnerFunc is a Runner and deliberately nothing more.
type runnerFunc func(context.Context, Request) (*Result, error)

func (f runnerFunc) Run(ctx context.Context, req Request) (*Result, error) {
	return f(ctx, req)
}

// Registering two providers under one name is a wiring bug, not a runtime
// condition: it means two implementations disagree about who serves a name.
func TestRegisteringTwiceIsAProgrammingError(t *testing.T) {
	const name = "duplicate-llm"
	RegisterProvider(ProviderFunc(name, func(Options) (Runner, error) { return nil, nil }))
	t.Cleanup(func() { unregisterProvider(name) })

	defer func() {
		if recover() == nil {
			t.Error("registering the same provider name twice was allowed")
		}
	}()
	RegisterProvider(ProviderFunc(name, func(Options) (Runner, error) { return nil, nil }))
}

// The error a typo produces should name what this binary actually has, rather
// than a list written down somewhere and left to drift.
func TestUnknownRunnerNamesTheAvailableProviders(t *testing.T) {
	_, err := New(&config.Config{Agent: config.Agent{Runner: "carrier-pigeon"}})
	if err == nil {
		t.Fatal("New accepted an unknown runner")
	}
	for _, want := range []string{"carrier-pigeon", "api", "cli", "fake"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestBuiltInProvidersAreRegistered(t *testing.T) {
	got := Providers()
	for _, want := range []string{"api", "cli", "fake"} {
		if !slices.Contains(got, want) {
			t.Errorf("provider %q is not registered; have %v", want, got)
		}
	}
}

// The fake runner's knobs predate agent.settings and stay on config.Agent, so
// existing configuration files keep working. They reach the provider through
// the same untyped bag a third-party provider reads.
func TestFakeRunnerStillReadsItsTypedConfig(t *testing.T) {
	cfg := &config.Config{Agent: config.Agent{
		Runner:        config.RunnerFake,
		FakeCostUSD:   0.02,
		FakeFailUnits: []string{"module:doomed"},
		FakeLatency:   3 * time.Second,
	}}

	r, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fake, ok := r.(*FakeRunner)
	if !ok {
		t.Fatalf("New returned %T, want *FakeRunner", r)
	}
	if fake.opts.CostPerCall != 0.02 {
		t.Errorf("CostPerCall = %v, want 0.02", fake.opts.CostPerCall)
	}
	if fake.opts.Latency != 3*time.Second {
		t.Errorf("Latency = %v, want 3s", fake.opts.Latency)
	}
	if !slices.Contains(fake.opts.FailUnits, "module:doomed") {
		t.Errorf("FailUnits = %v, want it to carry module:doomed", fake.opts.FailUnits)
	}
}
