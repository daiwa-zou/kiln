package agent

import (
	"context"
	"encoding/json"
	"path"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/daiwa-zou/kiln/internal/config"
)

var pathRe = regexp.MustCompile(pagePathPattern)

// analyzeReq mirrors the shape internal/jobs renders for an analyze step.
func fakeAnalyzeReq(unit, title string) Request {
	prompt := "Analyze this unit and plan its wiki pages.\n\nUnit: " + unit + "\n"
	if title != "" {
		prompt += "Title: " + title + "\n"
	}
	prompt += "\n## Sources\n\nsome rendered unit content\n"
	return Request{Step: StepAnalyze, SessionID: "s-" + unit, Prompt: prompt}
}

// generateReq mirrors the generate prompt, optionally embedding a plan the
// way the pipeline does (a ```json fence built from the AnalysisResult).
func fakeGenReq(t *testing.T, unit string, plan *AnalysisResult) Request {
	t.Helper()
	prompt := "Write the wiki pages you planned for unit " + unit + ".\n"
	if plan != nil {
		encoded, err := json.Marshal(plan)
		if err != nil {
			t.Fatal(err)
		}
		prompt += "\n## The plan from your analysis\n\nWrite exactly the pages this plan lists:\n\n```json\n" +
			string(encoded) + "\n```\n"
	}
	prompt += "\n## Sources\n\nsome rendered unit content\n"
	return Request{Step: StepGenerate, SessionID: "s-" + unit, Prompt: prompt}
}

func TestFakeAnalyzeProposesValidPage(t *testing.T) {
	f := NewFakeRunner(FakeOptions{})

	cases := map[string]struct{ wantDir, wantType string }{
		"module:internal-agent": {"entities", "entity"},
		"entry:kiln":            {"entities", "entity"},
		"doc:README.md":         {"sources", "source"},
		"arch:overview":         {"synthesis", "synthesis"},
		"doc:guide.md#setup":    {"sources", "source"},
		"weird key":             {"concepts", "concept"},
	}
	for unit, want := range cases {
		res, err := f.Run(context.Background(), fakeAnalyzeReq(unit, "A Title"))
		if err != nil || !res.Succeeded() {
			t.Fatalf("%s: run failed: %v %+v", unit, err, res)
		}
		if res.Analysis == nil || len(res.Analysis.Pages) != 1 {
			t.Fatalf("%s: analysis = %+v, want one page", unit, res.Analysis)
		}
		p := res.Analysis.Pages[0]
		if !pathRe.MatchString(p.Path) {
			t.Errorf("%s: path %q fails the schema pattern", unit, p.Path)
		}
		if path.Dir(p.Path) != want.wantDir || p.Type != want.wantType {
			t.Errorf("%s: got %s in %s, want %s in %s", unit, p.Type, path.Dir(p.Path), want.wantType, want.wantDir)
		}
		if p.Title != "A Title" {
			t.Errorf("%s: title %q, want the prompt's Title line", unit, p.Title)
		}
	}
}

func TestFakeGenerateHonorsThePlanExactly(t *testing.T) {
	f := NewFakeRunner(FakeOptions{})
	plan := &AnalysisResult{Pages: []AnalysisPage{
		{Path: "entities/alpha.md", Type: "entity", Title: "Alpha"},
		{Path: "concepts/beta.md", Type: "concept", Title: "Beta"},
	}}

	res, err := f.Run(context.Background(), fakeGenReq(t, "module:alpha", plan))
	if err != nil || !res.Succeeded() {
		t.Fatalf("run: %v %+v", err, res)
	}
	if res.Generation == nil || len(res.Generation.Pages) != 2 {
		t.Fatalf("generation = %+v, want the plan's two pages", res.Generation)
	}
	for i, want := range plan.Pages {
		got := res.Generation.Pages[i]
		if got.Path != want.Path || got.Type != want.Type || got.Title != want.Title {
			t.Errorf("page %d = %s/%s/%s, want the plan's %s/%s/%s",
				i, got.Path, got.Type, got.Title, want.Path, want.Type, want.Title)
		}
		if len(strings.TrimSpace(got.Body)) < 120 {
			t.Errorf("page %d body is %d bytes, under wiki.MinBodyBytes", i, len(got.Body))
		}
		if !strings.HasPrefix(got.Body, "# ") {
			t.Errorf("page %d body has no heading", i)
		}
		if strings.Contains(got.Body, "[[") {
			t.Errorf("page %d body contains wikilinks; the fake must not create gaps", i)
		}
	}
}

func TestFakeGenerateFallbackAgreesWithAnalyze(t *testing.T) {
	f := NewFakeRunner(FakeOptions{})
	const unit = "module:internal-store"

	an, err := f.Run(context.Background(), fakeAnalyzeReq(unit, ""))
	if err != nil {
		t.Fatal(err)
	}
	gen, err := f.Run(context.Background(), fakeGenReq(t, unit, nil)) // no plan block
	if err != nil {
		t.Fatal(err)
	}
	if got, want := gen.Generation.Pages[0].Path, an.Analysis.Pages[0].Path; got != want {
		t.Errorf("fallback path %q != analyze path %q; the two routes must agree", got, want)
	}
}

func TestFakeIsDeterministicAndContentSensitive(t *testing.T) {
	f := NewFakeRunner(FakeOptions{CostPerCall: 0.01})
	req := fakeGenReq(t, "module:x", nil)

	a, err := f.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Error("identical requests produced different results")
	}

	changed := req
	changed.Prompt += "a source file changed\n"
	c, err := f.Run(context.Background(), changed)
	if err != nil {
		t.Fatal(err)
	}
	if a.Generation.Pages[0].Body == c.Generation.Pages[0].Body {
		t.Error("changed sources did not change the page fingerprint")
	}
}

func TestFakeFailureInjection(t *testing.T) {
	f := NewFakeRunner(FakeOptions{FailUnits: []string{"module:bad"}})

	res, err := f.Run(context.Background(), fakeAnalyzeReq("module:bad-service", ""))
	if err != nil {
		t.Fatal(err)
	}
	if res.Succeeded() {
		t.Error("matching unit succeeded; want injected failure")
	}
	if res.Subtype != "error_during_execution" || !strings.Contains(res.Result, "module:bad-service") {
		t.Errorf("failure envelope: %+v", res)
	}

	ok, err := f.Run(context.Background(), fakeAnalyzeReq("module:good", ""))
	if err != nil || !ok.Succeeded() {
		t.Errorf("non-matching unit failed: %v %+v", err, ok)
	}
}

func TestFakeEnvelopeFields(t *testing.T) {
	f := NewFakeRunner(FakeOptions{CostPerCall: 0.05})
	res, err := f.Run(context.Background(), fakeAnalyzeReq("module:x", ""))
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalCostUSD != 0.05 || res.NumTurns != 1 || res.Usage.Total() == 0 {
		t.Errorf("envelope: cost=%v turns=%d usage=%d", res.TotalCostUSD, res.NumTurns, res.Usage.Total())
	}
	if res.SessionID != "s-module:x" {
		t.Errorf("session id not echoed: %q", res.SessionID)
	}
}

func TestFakeLatencyRespectsContext(t *testing.T) {
	f := NewFakeRunner(FakeOptions{Latency: time.Minute})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := f.Run(ctx, fakeAnalyzeReq("module:x", ""))
	if err == nil {
		t.Error("canceled context did not abort the latency sleep")
	}
	if time.Since(start) > 5*time.Second {
		t.Error("latency ignored the context deadline")
	}
}

func TestFactorySelectsFakeRunner(t *testing.T) {
	cfg := &config.Config{Agent: config.Agent{Runner: config.RunnerFake, FakeCostUSD: 0.02}}

	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	fake, ok := r.(*FakeRunner)
	if !ok {
		t.Fatalf("runner = %T, want *FakeRunner", r)
	}
	if fake.opts.CostPerCall != 0.02 {
		t.Errorf("cost not wired: %+v", fake.opts)
	}
	// The two pipeline seams must both answer false: no scratch dir, and no
	// KnownModel budget guard.
	if WritesFiles(r) {
		t.Error("fake runner claims to write files")
	}
	if EstimatesCost(r) {
		t.Error("fake runner claims estimated cost")
	}
}
