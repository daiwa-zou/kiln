package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// FakeRunner generates deterministic placeholder pages with zero API spend.
//
// It exists so a code change can be exercised through the entire real
// pipeline locally — sync, mapping, planning, validation, import, the queue,
// the budget ledger, the UI — without an API key or a bill. It is API-shaped:
// pages come back as structured data on Result.Analysis / Result.Generation,
// never as files, so WritesFiles reports false and the pipeline creates no
// scratch directory.
//
// The runner is stateless. Everything it returns is a pure function of the
// Request, keyed on the unit name embedded in the prompt, so a rebuilt unit
// overwrites its own page and an unchanged workspace still hits the hash
// gate's no-changes path.
//
// Known accepted limitation: two distinct unit keys that slugify identically
// (module:a/b vs module:a.b) would collide on one page path. Fine for a
// development tool; page names stay readable instead of carrying hash
// suffixes.
type FakeRunner struct {
	opts FakeOptions
}

// Capabilities: the fake returns structured data like the API runner, so it
// needs no scratch directory. Its cost is synthetic but exact -- it reports
// precisely what it charged -- so it is not an estimate in the sense budget
// enforcement means, which is "derived from a pricing table that may not have
// an entry for this model". Claiming otherwise makes every dev build demand a
// pricing entry for whatever placeholder model name is configured.
func (f *FakeRunner) Capabilities() Capabilities {
	return Capabilities{WritesFiles: false, EstimatesCost: false}
}

// FakeOptions tunes the fake's behavior. The zero value is a free, instant,
// always-succeeding runner.
type FakeOptions struct {
	// CostPerCall is the synthetic cost recorded on every call, so the spend
	// ledger and estimates carry realistic numbers in development.
	CostPerCall float64
	// FailUnits fails any unit whose key contains one of these substrings,
	// with the same error envelope a real execution failure produces. This is
	// how partial runs, stale-hash retries, and the dashboard's error display
	// are exercised locally.
	FailUnits []string
	// Latency delays each call (context-aware), for watching queue and UI
	// state transitions happen at human speed.
	Latency time.Duration
}

// NewFakeRunner returns a fake runner.
func NewFakeRunner(opts FakeOptions) *FakeRunner { return &FakeRunner{opts: opts} }

// Run implements Runner.
func (f *FakeRunner) Run(ctx context.Context, req Request) (*Result, error) {
	if f.opts.Latency > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(f.opts.Latency):
		}
	}

	unit := unitFromPrompt(req.Prompt)
	for _, needle := range f.opts.FailUnits {
		if needle != "" && strings.Contains(unit, needle) {
			return &Result{
				IsError:   true,
				Subtype:   "error_during_execution",
				SessionID: req.SessionID,
				NumTurns:  1,
				Result:    fmt.Sprintf("fake runner: injected failure for unit %s", unit),
			}, nil
		}
	}

	res := &Result{
		Subtype:        "success",
		TerminalReason: "completed",
		SessionID:      req.SessionID,
		NumTurns:       1,
		Model:          req.Model,
		TotalCostUSD:   f.opts.CostPerCall,
		DurationMS:     f.opts.Latency.Milliseconds(),
		// Rough token shapes so the dashboard's token column reads plausibly.
		Usage: Usage{
			InputTokens:  len(req.Prompt) / 4,
			OutputTokens: 200,
		},
	}

	switch req.Step {
	case StepAnalyze:
		res.Analysis = f.analyze(unit, req)
	case StepResearch:
		res.Research = f.research(req)
	default:
		res.Generation = f.generate(unit, req)
	}
	return res, nil
}

// research answers the review item the prompt carries, without resolving it:
// the interesting state to exercise locally is findings landing on a card that
// is still waiting for a human, and a fake that closed its own questions would
// leave the reviews inbox permanently empty.
func (f *FakeRunner) research(req Request) *ResearchResult {
	question := questionFromPrompt(req.Prompt)
	if question == "" {
		question = "the review item"
	}
	return &ResearchResult{
		Findings: fmt.Sprintf(
			"Fake-runner research on %q. No corpus was read and no API call was made: "+
				"the run, the queue slot, the spend ledger, and this write-back are all real.",
			question),
		Evidence: []string{"fake-runner"},
	}
}

// analyze proposes exactly one page for the unit, derived from its key so the
// mapping is stable across runs.
func (f *FakeRunner) analyze(unit string, req Request) *AnalysisResult {
	path, pageType := pageFor(unit)
	title := titleFromPrompt(req.Prompt)
	if title == "" {
		title = humanize(unit)
	}
	return &AnalysisResult{
		Pages: []AnalysisPage{{
			Path:    path,
			Type:    pageType,
			Title:   title,
			Summary: fmt.Sprintf("Deterministic fake page for unit %s.", unit),
		}},
		Findings: []string{fmt.Sprintf("fake runner analyzed unit %s with zero API spend", unit)},
	}
}

// generate writes the pages the plan lists. The plan travels embedded in the
// generate prompt as a ```json block; honoring it verbatim is what guarantees
// the generated set is a subset of the planned set, which validation
// enforces. When no plan block is present, the page is re-derived from the
// unit key by the same function analyze used, so both routes agree.
func (f *FakeRunner) generate(unit string, req Request) *GenerationResult {
	pages := plannedPages(req.Prompt)
	if len(pages) == 0 {
		path, pageType := pageFor(unit)
		pages = []AnalysisPage{{Path: path, Type: pageType, Title: humanize(unit)}}
	}

	// The fingerprint makes the body a function of the unit's rendered source
	// context: it changes exactly when the sources change, and stays stable
	// when they do not.
	sum := sha256.Sum256([]byte(req.Prompt))
	fingerprint := hex.EncodeToString(sum[:])[:12]

	out := &GenerationResult{
		Findings: []string{fmt.Sprintf("fake runner generated unit %s", unit)},
	}
	for _, p := range pages {
		title := p.Title
		if title == "" {
			title = humanize(unit)
		}
		body := fmt.Sprintf(`# %s

Fake-runner page for unit %s, generated with zero LLM cost. Every pipeline
stage around this prose was real: the sources were synced and mapped, this
page passed validation, and the import committed it transactionally.

Content fingerprint: %s
`, title, fmt.Sprintf("`%s`", unit), fingerprint)

		out.Pages = append(out.Pages, GeneratedPage{
			Path:  p.Path,
			Type:  p.Type,
			Title: title,
			Body:  body,
			Tags:  []string{"fake-runner"},
		})
	}
	return out
}

// pageFor maps a unit key to its one page. Type and directory come from the
// same switch, so the type-matches-directory validation can never disagree.
func pageFor(unit string) (path, pageType string) {
	prefix, _, _ := strings.Cut(unit, ":")
	var dir string
	switch prefix {
	case "module", "entry":
		dir, pageType = "entities", "entity"
	case "doc":
		dir, pageType = "sources", "source"
	case "arch":
		dir, pageType = "synthesis", "synthesis"
	default:
		dir, pageType = "concepts", "concept"
	}
	return dir + "/" + slugify(unit) + ".md", pageType
}

// unitFromPrompt extracts the unit key from either prompt shape the pipeline
// renders: "Unit: <key>" on analyze, "...planned for unit <key>." on the
// first generate line.
func unitFromPrompt(prompt string) string {
	for line := range strings.Lines(prompt) {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "Unit: "); ok {
			return after
		}
		if _, after, ok := strings.Cut(line, "planned for unit "); ok {
			return strings.TrimSuffix(strings.TrimSpace(after), ".")
		}
	}
	return "unknown"
}

// questionFromPrompt reads the "Question: <q>" line a research prompt opens
// with, mirroring how unitFromPrompt recovers the unit from the other two.
func questionFromPrompt(prompt string) string {
	for line := range strings.Lines(prompt) {
		if after, ok := strings.CutPrefix(strings.TrimSpace(line), "Question: "); ok {
			return after
		}
	}
	return ""
}

// titleFromPrompt reads the optional "Title: <t>" line of an analyze prompt.
func titleFromPrompt(prompt string) string {
	for line := range strings.Lines(prompt) {
		if after, ok := strings.CutPrefix(strings.TrimSpace(line), "Title: "); ok {
			return after
		}
	}
	return ""
}

// plannedPages recovers the analysis plan the generate prompt embeds in a
// ```json fence.
func plannedPages(prompt string) []AnalysisPage {
	_, rest, ok := strings.Cut(prompt, "```json\n")
	if !ok {
		return nil
	}
	block, _, ok := strings.Cut(rest, "\n```")
	if !ok {
		return nil
	}
	plan, err := ParseAnalysis(block)
	if err != nil {
		return nil
	}
	return plan.Pages
}

// slugify collapses a unit key into the schema's slug alphabet:
// lowercase alphanumerics with single hyphens.
func slugify(s string) string {
	var b strings.Builder
	lastHyphen := true // suppress a leading hyphen
	for _, r := range strings.ToLower(s) {
		isAlnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		switch {
		case isAlnum:
			b.WriteRune(r)
			lastHyphen = false
		case !lastHyphen:
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	out := strings.TrimSuffix(b.String(), "-")
	if out == "" {
		return "unit"
	}
	return out
}

// humanize turns a unit key into a readable title.
func humanize(unit string) string {
	_, name, ok := strings.Cut(unit, ":")
	if !ok || name == "" {
		name = unit
	}
	name = strings.NewReplacer("-", " ", "_", " ", "/", " / ").Replace(name)
	return strings.ToUpper(name[:1]) + name[1:]
}
