package jobs

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"

	"github.com/daiwa-zou/kiln/internal/agent"
	"github.com/daiwa-zou/kiln/internal/mapper"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

// ResearchSchema constrains the research step's output, single-sourced from the
// agent package for the same reason AnalysisSchema is.
var ResearchSchema = agent.ResearchSchemaText()

// Question is one review item, as the thing being researched rather than as a
// database row.
type Question struct {
	// Kind is the review kind: contradiction, uncertain, or gap.
	Kind string
	// Title and Detail are what the build wrote when it raised the flag.
	Title  string
	Detail string
	// PageSlug is the page the flag concerns, empty when it concerns the bench.
	PageSlug string
}

// ResearchRequest is one review item answered against the bench's sources.
type ResearchRequest struct {
	RunID       string
	WorkspaceID string
	Question    Question
	Source      SourceSpec

	// Progress receives human-readable sync narration; nil discards it.
	Progress io.Writer
}

// ResearchOutcome is what a research pass found.
type ResearchOutcome struct {
	Findings string
	Resolved bool
	Evidence []string
	CostUSD  float64
	Tokens   int
}

// Research answers one review item and writes nothing to the wiki.
//
// The premise is that the question exists because of how builds are shaped,
// not because the answer is unavailable. A build generates unit by unit, and
// each unit sees its own slice of the material: a module that contradicts a
// document, an assertion corroborated two directories away, a page referred to
// whose subject is described somewhere the referring unit never looked. The
// unit raises the flag because from where it stood the question was genuinely
// unanswerable. This is the read that does not stand there.
//
// It costs one agent call, and it produces prose for a human rather than pages
// for the wiki. Turning findings into pages is the next build's job, through
// steering or a correction a human writes after reading this -- a step that
// both answered questions and wrote pages would be a second generation path
// into the wiki with none of the validation the first one has.
func (p *Pipeline) Research(ctx context.Context, req ResearchRequest) (*ResearchOutcome, error) {
	log := p.logger().With("run", req.RunID, "workspace", req.WorkspaceID)
	out := req.Progress
	if out == nil {
		out = io.Discard
	}
	if strings.TrimSpace(req.Question.Title) == "" {
		return nil, fmt.Errorf("jobs: research needs a question")
	}

	summary := RunSummary{
		RunID: req.RunID, WorkspaceID: req.WorkspaceID,
		Trigger: "research", Status: StatusRunning,
	}

	mat, err := materialize(ctx, out, req.Source)
	if err != nil {
		return nil, p.failResearch(ctx, summary, err)
	}
	defer mat.Close()

	pages, err := p.Store.LoadPages(ctx, req.WorkspaceID)
	if err != nil {
		return nil, p.failResearch(ctx, summary, fmt.Errorf("jobs: load pages: %w", err))
	}
	steering, err := p.Store.LoadSteering(ctx, req.WorkspaceID)
	if err != nil {
		return nil, p.failResearch(ctx, summary, fmt.Errorf("jobs: load steering: %w", err))
	}

	// The whole corpus does not fit in a prompt and would not be worth paying
	// for if it did, so Go picks the material the question is about before the
	// model is involved. Selection is deterministic and inspectable here,
	// exactly as routing and planning are.
	units := relevantUnits(req.Question, mat.Map)
	fmt.Fprintf(out, "researching %q against %d unit(s)\n", req.Question.Title, len(units))

	res, aerr := p.Runner.Run(ctx, agent.Request{
		Step:             agent.StepResearch,
		WorkDir:          req.Source.Path,
		Model:            p.researchModel(),
		BudgetUSD:        p.Budget.AnalyzeUSD,
		Timeout:          p.Timeout,
		SystemPrompt:     researchSystemPrompt(steering),
		CacheableContext: mat.Map.Summary,
		Prompt:           researchPrompt(req.Question, req.Source.Path, units, relevantPages(req.Question, pages)),
		JSONSchema:       ResearchSchema,
	})

	// Charged whether or not the call succeeded: a failed research pass that
	// still burned tokens must show up in the budget window like any other
	// spend, or the window undercounts it forever.
	if res != nil {
		summary.CostUSD = res.TotalCostUSD
		summary.Tokens = res.Usage.Total()
		p.noteAgentEvents("research", res)
	}
	if aerr != nil {
		return nil, p.failResearch(ctx, summary, aerr)
	}
	if err := res.Err(); err != nil {
		return nil, p.failResearch(ctx, summary, err)
	}

	found := res.Research
	if found == nil {
		// The CLI runner returns the schema-constrained JSON as envelope text.
		parsed, perr := agent.ParseResearch(res.Result)
		if perr != nil {
			return nil, p.failResearch(ctx, summary,
				fmt.Errorf("jobs: research returned no readable findings: %w", perr))
		}
		found = parsed
	}
	if strings.TrimSpace(found.Findings) == "" {
		return nil, p.failResearch(ctx, summary, fmt.Errorf("jobs: research returned empty findings"))
	}

	summary.Status = StatusSucceeded
	if err := p.Store.RecordRun(context.WithoutCancel(ctx), summary); err != nil {
		// The findings are already paid for and are what the caller writes back;
		// losing the run record must not lose them too.
		log.Error("research run could not be ledgered", "err", err)
	}

	return &ResearchOutcome{
		Findings: strings.TrimSpace(found.Findings),
		Resolved: found.Resolved,
		Evidence: found.Evidence,
		CostUSD:  summary.CostUSD,
		Tokens:   summary.Tokens,
	}, nil
}

// failResearch ledgers a failed pass and returns the cause. Recording happens
// on a detached context because the usual reason a research call fails is that
// the worker is draining, and a run row left 'running' blocks its workspace
// until the stale deadline.
func (p *Pipeline) failResearch(ctx context.Context, summary RunSummary, cause error) error {
	summary.Status = StatusFailed
	summary.Err = cause.Error()
	if err := p.Store.RecordRun(context.WithoutCancel(ctx), summary); err != nil {
		p.logger().Error("failed research run could not be ledgered", "err", err)
	}
	return cause
}

// researchModel prefers the analyze model: research is the same shape of work
// -- read widely, return structure, write nothing -- and a bench that has
// chosen a cheaper model for reading meant it here too.
func (p *Pipeline) researchModel() string {
	if p.AnalyzeModel != "" {
		return p.AnalyzeModel
	}
	return p.Model
}

// Research context budgets. Smaller than a unit's, and for a different reason:
// a unit renders the material it is definitely about, while research renders
// material it is only probably about. Paying unit rates for a guess is how a
// feature that answers questions becomes a feature nobody enables.
const (
	maxResearchUnits     = 6
	maxResearchPages     = 4
	maxResearchPageBytes = 8_000
)

// relevantUnits picks the units a question is most likely answered by, ranked
// by how much of the question's vocabulary each one's identity carries.
//
// Crude on purpose. The alternative -- asking a model which units to read, then
// reading them -- doubles the calls and the cost to sharpen a choice that a
// wrong answer degrades rather than breaks: an off-target unit costs tokens,
// and the pass reports that it could not settle the question, which is the
// same thing it would have reported without researching at all.
func relevantUnits(q Question, wm *mapper.WorkspaceMap) []mapper.Unit {
	if wm == nil || len(wm.Units) == 0 {
		return nil
	}
	terms := terms(q.Title + " " + q.Detail + " " + q.PageSlug)
	if len(terms) == 0 {
		return nil
	}

	type scored struct {
		unit  mapper.Unit
		score int
	}
	ranked := make([]scored, 0, len(wm.Units))
	for _, u := range wm.Units {
		if u.Empty {
			continue
		}
		s := overlap(terms, u.Key+" "+u.Title+" "+u.Dir+" "+u.Slug)
		if s == 0 {
			continue
		}
		ranked = append(ranked, scored{unit: u, score: s})
	}
	// Key breaks ties so an identical question renders an identical prompt,
	// which is what lets the prompt cache hit across repeated research.
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].unit.Key < ranked[j].unit.Key
	})

	out := make([]mapper.Unit, 0, maxResearchUnits)
	for _, r := range ranked {
		if len(out) == maxResearchUnits {
			break
		}
		out = append(out, r.unit)
	}
	return out
}

// relevantPages picks the wiki's own pages that bear on the question. They
// matter as much as the sources do: a contradiction is between two things the
// wiki already says, and a gap is a page other pages have declared they need,
// so the pages naming it are the statement of what is missing.
func relevantPages(q Question, pages []wiki.Page) []wiki.Page {
	terms := terms(q.Title + " " + q.Detail)
	slug := strings.ToLower(strings.TrimSpace(q.PageSlug))

	type scored struct {
		page  wiki.Page
		score int
	}
	var ranked []scored
	for _, pg := range pages {
		s := overlap(terms, pg.Slug+" "+pg.Meta.Title)
		// The page the flag was raised against, and any page linking to the
		// subject of a gap, are relevant whether or not their titles echo the
		// question's wording.
		if slug != "" && (strings.EqualFold(pg.Slug, slug) || strings.Contains(strings.ToLower(pg.Body), "[["+slug+"]]")) {
			s += 10
		}
		if s == 0 {
			continue
		}
		ranked = append(ranked, scored{page: pg, score: s})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].page.Slug < ranked[j].page.Slug
	})

	out := make([]wiki.Page, 0, maxResearchPages)
	for _, r := range ranked {
		if len(out) == maxResearchPages {
			break
		}
		out = append(out, r.page)
	}
	return out
}

// researchStopwords are the words a question is made of rather than about.
// Left in, they match every unit in the bench and the ranking becomes noise.
var researchStopwords = map[string]bool{
	"and": true, "are": true, "but": true, "cannot": true, "does": true,
	"for": true, "from": true, "has": true, "have": true, "into": true,
	"its": true, "not": true, "page": true, "should": true, "than": true,
	"that": true, "the": true, "their": true, "there": true, "these": true,
	"they": true, "this": true, "was": true, "were": true, "what": true,
	"when": true, "where": true, "which": true, "with": true, "wiki": true,
}

// terms splits text into the lowercase words worth matching on: three
// characters or more, and not a word every question contains.
func terms(text string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len(w) < 3 || researchStopwords[w] {
			continue
		}
		out[w] = true
	}
	return out
}

// overlap counts how many of the question's terms appear in a candidate's
// identity. Substring rather than word matching, because unit keys and slugs
// glue words together with hyphens and colons.
func overlap(terms map[string]bool, candidate string) int {
	if len(terms) == 0 {
		return 0
	}
	lower := strings.ToLower(candidate)
	n := 0
	for t := range terms {
		if strings.Contains(lower, t) {
			n++
		}
	}
	return n
}

func researchSystemPrompt(s Steering) string {
	var b strings.Builder
	b.WriteString(untrustedFraming)
	b.WriteString(`

You are answering one open question about this knowledge base. Investigate
read-only and report what you find. Do not write or plan any pages.

Rules:

- Ground every statement in material included below. Cite the file path, page
  slug, or url it came from.
- If the material does not settle the question, say so plainly and say what
  would settle it. An honest "not answerable from these sources" is a useful
  answer; a plausible guess is not.
- Only report the question resolved when the material actually settles it and
  no human judgement is left to make. A question about what someone wants, or
  about whether to remove something, is never resolved by reading.
- Write for a human reading a review queue: a short answer first, then the
  evidence behind it.`)
	appendSteering(&b, s)
	return b.String()
}

// researchPrompt renders the question and the material selected for it. The
// "Question:" line leads so the fake runner can recover it the same way it
// recovers a unit key from the other two prompts.
func researchPrompt(q Question, root string, units []mapper.Unit, pages []wiki.Page) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Answer this review item.\n\nQuestion: %s\n", q.Title)
	fmt.Fprintf(&b, "Kind: %s\n", q.Kind)
	if q.PageSlug != "" {
		fmt.Fprintf(&b, "Raised against page: %s\n", q.PageSlug)
	}
	if strings.TrimSpace(q.Detail) != "" {
		b.WriteString("\n## What the build recorded\n\n")
		// Fenced: the detail is prose a previous model wrote, and it is read
		// here as the question rather than as instruction.
		fmt.Fprintf(&b, "```\n%s\n```\n", strings.TrimSpace(q.Detail))
	}

	switch {
	case len(units) == 0:
		b.WriteString("\n## Source material\n\nNo source unit matched this question.\n")
	default:
		b.WriteString("\n## Source material\n\n")
		b.WriteString("These are the units whose subject matter is closest to the question.\n")
		for _, u := range units {
			fmt.Fprintf(&b, "\n### unit %s", u.Key)
			if u.Title != "" {
				fmt.Fprintf(&b, " — %s", u.Title)
			}
			b.WriteString("\n")
			if ctx := renderUnitContext(root, u); ctx != "" {
				b.WriteString(ctx)
			} else {
				b.WriteString("\n_No readable inputs._\n")
			}
		}
	}

	if len(pages) > 0 {
		b.WriteString("\n## What the wiki currently says\n\n")
		b.WriteString("These pages bear on the question. They may be the source of it.\n")
		for _, pg := range pages {
			fmt.Fprintf(&b, "\n### %s (%s)\n\n", pg.Meta.Title, pg.Slug)
			body := pg.Body
			if len(body) > maxResearchPageBytes {
				body = body[:maxResearchPageBytes] + "\n…truncated…"
			}
			fmt.Fprintf(&b, "```\n%s\n```\n", body)
		}
	}

	return b.String()
}
