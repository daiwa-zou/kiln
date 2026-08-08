package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"

	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/mapper"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

// DefaultUnitConcurrency keeps the pipeline sequential unless a deployment
// asks otherwise. Fan-out multiplies in-flight model calls, and the cost of
// that decision belongs to whoever operates the bench, not to a default.
const DefaultUnitConcurrency = 1

// joinViolations renders the first n violations for a message a human reads.
// Bounded because a page can fail many rules at once and this ends up in a
// database column and a log line; the count alongside it carries the rest.
func joinViolations(vs []wiki.Violation, n int) string {
	if len(vs) < n {
		n = len(vs)
	}
	parts := make([]string, 0, n+1)
	for _, v := range vs[:n] {
		parts = append(parts, v.String())
	}
	if len(vs) > n {
		parts = append(parts, fmt.Sprintf("(+%d more)", len(vs)-n))
	}
	return strings.Join(parts, "; ")
}

// unitOutcome is one unit's result, held in the run's planned order so the
// records a run produces never depend on which unit happened to finish
// first. attempted is false for a unit the scheduler never launched --
// stopped by cancellation or by an exhausted budget -- which earns no item,
// exactly as breaking out of the sequential loop did.
type unitOutcome struct {
	key       diff.Key
	res       unitResult
	attempted bool
}

func (p *Pipeline) unitConcurrency() int {
	if p.UnitConcurrency > 1 {
		return p.UnitConcurrency
	}
	return DefaultUnitConcurrency
}

// generateUnits runs the planned units, up to conc at a time, and returns
// their outcomes in plan order along with a status if scheduling was cut
// short. Errors are not returned: a unit's failure is recorded against that
// unit, and the run still imports whatever else landed.
//
// Cancellation and budget exhaustion both stop the launching of new work
// without disturbing what is already in flight. Units already running keep
// their context, so a drain lets paid-for calls finish rather than throwing
// them away.
func (p *Pipeline) generateUnits(
	ctx context.Context,
	req BuildRequest,
	dirty []diff.Key,
	units map[string]mapper.Unit,
	conc int,
	base unitScope,
	log *slog.Logger,
) (outcomes []unitOutcome, halted string) {
	outcomes = make([]unitOutcome, len(dirty))
	for i, key := range dirty {
		outcomes[i].key = key
	}

	var (
		mu   sync.Mutex
		stop bool
		sem  = make(chan struct{}, conc)
		wg   sync.WaitGroup
	)
	// halt records the first reason scheduling stopped and stops it. When a
	// run is both canceled and out of budget the winner is whichever was
	// noticed first, which the sequential pipeline decided by check order
	// and this cannot; either way the run stops early and imports what
	// landed, so the difference is the label on an already-stopped run.
	halt := func(status string) {
		mu.Lock()
		defer mu.Unlock()
		stop = true
		if halted == "" {
			halted = status
		}
	}
	halting := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return stop
	}

	for i, key := range dirty {
		// The slot is taken before the checks, not after. Acquiring first is
		// what makes a free slot mean "enough earlier work has finished to
		// start more" -- checking first would read a cancellation or a budget
		// halt that had not happened yet, then block on the slot and launch
		// anyway, which at concurrency 1 would run a unit the sequential
		// pipeline would never have started.
		sem <- struct{}{}

		if ctx.Err() != nil {
			// Stop scheduling rather than fail: units already generated are
			// still valid and are imported on an uncancelable context, so a
			// Ctrl-C costs the remainder of the run, not the work paid for.
			log.Warn("run canceled; importing what completed", "cause", ctx.Err())
			halt(StatusCanceled)
			<-sem
			break
		}
		if halting() {
			<-sem
			break
		}

		wg.Add(1)
		go func(i int, key diff.Key) {
			defer wg.Done()
			defer func() { <-sem }()

			p.markItem(ctx, req.RunID, ItemSummary{Key: key, Status: StatusRunning}, log)

			out := p.generateUnit(ctx, req, key, units[string(key)], base)

			// Settled here rather than only in mergeOutcomes, which runs after
			// every unit has finished: at concurrency 1 a twelve-unit build
			// would otherwise show nothing until the last one landed. What is
			// written here is provisional -- mergeOutcomes still applies the
			// cross-unit checks and RecordRun remains the authority -- but it
			// is right for the common case and visible immediately.
			p.markItem(ctx, req.RunID, ItemSummary{
				Key: key, Status: itemStatus(out), CostUSD: out.CostUSD,
				Turns: out.Turns, Tokens: out.Tokens, Err: errText(out.Err),
			}, log)

			if errors.Is(out.Err, errRunBudgetExhausted) {
				log.Warn("run budget exhausted; remaining units stay stale",
					"key", key, "budget", p.Budget.RunUSD)
				halt(StatusOverBudget)
				// A unit that never got to spend anything is not a failure;
				// one that ran out after paying for its analysis is, and
				// keeps its item so the cost is visible.
				if out.CostUSD <= 0 {
					return
				}
			}
			outcomes[i].res = out
			outcomes[i].attempted = true
		}(i, key)
	}

	wg.Wait()
	return outcomes, halted
}

// publishSpend republishes a unit's running totals while it is still working,
// so what an ingest is spending is visible as it spends it rather than only
// once the unit lands.
//
// The status stays "running", which is what keeps this reporting rather than
// settling: finished_at is left null, the progress counts are unchanged, and
// the settle in generateUnits still writes the authoritative figures. Only the
// numbers move.
//
// Called after an agent call settles and the unit has more to do -- there is no
// point publishing on the way out, where the settle follows immediately.
func (p *Pipeline) publishSpend(ctx context.Context, runID string, key diff.Key, res *unitResult) {
	p.markItem(ctx, runID, ItemSummary{
		Key: key, Status: StatusRunning,
		CostUSD: res.CostUSD, Turns: res.Turns, Tokens: res.Tokens,
	}, p.logger())
}

// markItem publishes one unit's state mid-run, for anything watching the
// build. Failures are logged and swallowed: this is reporting, and a build
// whose pages are correct must not fail because its progress was not recorded.
func (p *Pipeline) markItem(ctx context.Context, runID string, item ItemSummary, log *slog.Logger) {
	// context.WithoutCancel so the final state of a unit still lands when the
	// run is being canceled. Otherwise a Ctrl-C leaves its last unit stuck
	// reading "running" forever.
	if err := p.Store.MarkRunItem(context.WithoutCancel(ctx), runID, item); err != nil {
		log.Warn("could not record unit progress", "key", item.Key, "err", err)
	}
}

// itemStatus is the provisional verdict on a unit, from what generateUnit
// alone can see. mergeOutcomes reaches a final one later using the cross-unit
// checks -- page-path and slug collisions, links to concurrently written pages
// -- which no single unit can evaluate. The two agree except where a unit that
// generated cleanly loses a collision, and RecordRun overwrites this with that.
func itemStatus(out unitResult) string {
	if out.Err != nil || len(out.Violations) > 0 {
		return StatusFailed
	}
	return StatusSucceeded
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// mergeOutcomes settles every unit's result into the run: the cross-unit
// checks that per-unit validation cannot see, then the summary items,
// pages, findings, and source records.
//
// It is deliberately serial and ordered by the plan. Two units can collide
// on a page path or a slug, and the loser must be the same one on every
// run regardless of which finished first, or a rerun would produce
// different records from identical inputs.
func (p *Pipeline) mergeOutcomes(
	res *BuildResult,
	req BuildRequest,
	outcomes []unitOutcome,
	baseKnown map[string]bool,
	perUnit float64,
	log *slog.Logger,
) (written []wiki.Page, newSources []diff.SourceRecord, findings []string, spent float64) {
	// The run's final slug set: what existed when the run started plus every
	// page a unit generated cleanly. Deferred link checks resolve against
	// this, which is what lets one unit link to another's page.
	//
	// Units dropped later in this function still contribute here. Removing
	// them would be a fixpoint iteration where one dropped unit dangles a
	// second unit's links and drops it too; a link left dangling by a
	// dropped unit is caught on the next run, when that unit -- still
	// holding its stale hash -- regenerates.
	finalKnown := maps.Clone(baseKnown)
	for _, oc := range outcomes {
		if !oc.attempted || oc.res.Err != nil || len(oc.res.Violations) > 0 {
			continue
		}
		for _, pg := range oc.res.Pages {
			finalKnown[pg.Slug] = true
		}
	}

	// writtenBy and slugOwner carry the two identities a page has: its path
	// on disk and its slug, which is the database's key. The sequential
	// pipeline caught slug collisions by growing existingBySlug as it went;
	// with a snapshot it cannot, so the check is explicit here.
	writtenBy := map[string]diff.Key{}
	slugOwner := map[string]diff.Key{}
	units := unitsByKey(req.Map)

	for _, oc := range outcomes {
		if !oc.attempted {
			continue
		}
		key, out := oc.key, oc.res

		// Summed here from each unit's total rather than read off the
		// ledger. The ledger settles per agent call, so totalling it would
		// add the same money in a different order and drift in the last
		// bits; the ledger's job is to gate spend, not to account for it.
		spent += out.CostUSD

		item := ItemSummary{
			Key: key, Status: StatusPending, EstCostUSD: perUnit,
			CostUSD: out.CostUSD, Turns: out.Turns, Tokens: out.Tokens,
		}
		res.Summary.Tokens += out.Tokens

		if out.Err == nil && len(out.Violations) == 0 {
			out.Violations = append(out.Violations, p.crossUnitViolations(key, out.Pages, writtenBy, slugOwner)...)
		}
		// Links last: a unit that already failed has nothing to resolve, and
		// checking here is what makes a link to a concurrently written page
		// valid rather than dangling.
		if out.Err == nil && len(out.Violations) == 0 {
			pages := make([]*wiki.Page, len(out.Pages))
			for i := range out.Pages {
				pages[i] = &out.Pages[i]
			}
			out.Violations = append(out.Violations, wiki.CheckLinks(pages, finalKnown, 0)...)
		}

		switch {
		case out.Err != nil:
			item.Status = StatusFailed
			item.Err = out.Err.Error()
			log.Error("unit failed", "key", key, "err", out.Err)
		case len(out.Violations) > 0:
			item.Status = StatusFailed
			// The reasons, not just the count. "1 validation violations" is the
			// whole of what an operator used to get for a unit that ran, cost
			// money, and produced pages that were then thrown away -- with the
			// one fact needed to fix it discarded here. The detail already
			// exists; only reporting it was missing.
			item.Err = fmt.Sprintf("%d validation violation(s): %s",
				len(out.Violations), joinViolations(out.Violations, 3))
			res.Violations = append(res.Violations, out.Violations...)
			log.Error("unit failed validation", "key", key,
				"violations", len(out.Violations), "detail", joinViolations(out.Violations, 5))
		default:
			item.Status = StatusSucceeded
			written = append(written, out.Pages...)
			findings = append(findings, out.Findings...)
			for _, pg := range out.Pages {
				writtenBy[pg.Path] = key
				slugOwner[pg.Slug] = key
			}

			// Review flags ride the run summary and are persisted with it.
			// Only successful units contribute: a failed unit retries next run
			// and will raise its flags again alongside content that landed.
			for _, rf := range out.Reviews {
				res.Summary.Reviews = append(res.Summary.Reviews, ReviewNote{
					Kind: rf.Kind, Title: rf.Title, Detail: rf.Detail, Unit: key,
				})
			}

			// The source record is only updated on success, so a failed unit
			// keeps its old hash and is retried on the next run. The
			// architecture synthesis has no unit of its own; the whole-map
			// hash is its input, and recording it is what makes an unchanged
			// repeat build genuinely free.
			hash := req.Map.Hash
			if key != diff.ArchOverview {
				hash = units[string(key)].Hash
			}
			newSources = append(newSources, diff.SourceRecord{
				Key:          key,
				InputHash:    hash,
				FilesWritten: pagePaths(out.Pages),
				ConnectorID:  req.Connectors.For(key),
				BlobKeys:     req.BlobKeys[parentDocID(key)],
			})
		}

		res.Summary.Items = append(res.Summary.Items, item)
	}
	return written, newSources, findings, spent
}

// crossUnitViolations rejects a unit whose pages collide with one an earlier
// unit already claimed. Either collision is silent data loss otherwise: two
// pages at one path means the second import overwrites the first, and two
// pages with one slug means the same thing a level down, since the database
// keys pages by (wiki, slug).
//
// The earlier unit in plan order keeps its page; the later one fails and
// retries on the next run, exactly as any other validation failure would.
func (p *Pipeline) crossUnitViolations(
	key diff.Key,
	pages []wiki.Page,
	writtenBy map[string]diff.Key,
	slugOwner map[string]diff.Key,
) []wiki.Violation {
	var out []wiki.Violation
	for _, pg := range pages {
		if owner, taken := writtenBy[pg.Path]; taken {
			out = append(out, wiki.Violation{
				Path:   pg.Path,
				Reason: fmt.Sprintf("already written by unit %s in this run; two units must not claim one page", owner),
			})
		}
		if owner, taken := slugOwner[pg.Slug]; taken {
			out = append(out, wiki.Violation{
				Path: pg.Path,
				Reason: fmt.Sprintf(
					"slug %q was already written by unit %s in this run; page names must be unique across all type directories", pg.Slug, owner),
			})
		}
	}
	return out
}
