package jobs

import "sync"

// budgetLedger enforces the run budget across concurrently generating units.
//
// The sequential pipeline could enforce a ceiling by accumulating spend and
// checking it before each unit: with one call in flight, the check and the
// charge could not interleave. Fanning units out breaks that -- C units
// reading one snapshot of spend would each believe the whole remaining
// budget was theirs, and the ceiling would scale with concurrency.
//
// So the reservation is taken *before* each agent call and settled after,
// rather than accumulated after the fact. Outstanding reservations plus
// settled spend never exceed the limit, which makes the limit hold at any
// concurrency. A unit that cannot reserve stops where the sequential
// pipeline would have stopped scheduling: its work stays stale and the next
// run picks it up.
//
// One overshoot survives, and it is the same one the sequential pipeline
// has: a runner that blows past its own per-call budget settles for more
// than it reserved. That is bounded by the per-call budget, not by
// concurrency, and the pipeline already logs it as an over-budget call.
type budgetLedger struct {
	mu sync.Mutex
	// limit is the run ceiling. Zero means unlimited, in which case every
	// reservation succeeds and the ledger only tallies.
	limit    float64
	reserved float64
	spent    float64
}

func newBudgetLedger(limit float64) *budgetLedger {
	return &budgetLedger{limit: limit}
}

// reserve holds want against the ceiling, reporting whether the headroom
// covered it. A zero or negative want is always granted: the caller has no
// per-call budget configured, and refusing would stall the run rather than
// bound it.
func (b *budgetLedger) reserve(want float64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.limit <= 0 || want <= 0 {
		return true
	}
	if b.spent+b.reserved+want > b.limit {
		return false
	}
	b.reserved += want
	return true
}

// settle replaces a granted reservation with what the call actually cost.
// Passing the same want that reserve granted is required: releasing a
// different amount would leak or double-free headroom.
func (b *budgetLedger) settle(want, actual float64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.limit > 0 && want > 0 {
		b.reserved -= want
		if b.reserved < 0 {
			// Defensive: a mismatched settle would otherwise hand out the
			// difference as phantom headroom for the rest of the run.
			b.reserved = 0
		}
	}
	b.spent += actual
}

// totalSpent reports settled spend. Reservations are deliberately excluded:
// this is what the run is billed for and what the ledger records.
func (b *budgetLedger) totalSpent() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent
}

// reserveOr reserves the per-call budget, falling back to est when no
// per-call budget is configured. Without the fallback a deployment that set
// only run_budget_usd would reserve nothing per call and the ceiling would
// stop bounding anything once units run concurrently.
func (b *budgetLedger) reserveOr(callBudget, est float64) (want float64, ok bool) {
	want = callBudget
	if want <= 0 {
		want = est
	}
	return want, b.reserve(want)
}
