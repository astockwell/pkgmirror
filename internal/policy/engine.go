package policy

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
)

// ChainEngine evaluates a Subject by dispatching to per-kind evaluators
// against a cached rule set. It implements Engine.
//
// Rule cache semantics:
//
//   - The rule slice is loaded once from the database (typically at boot
//     via PullFromStore) and may be refreshed atomically via Refresh.
//   - The hot Evaluate path takes an atomic load of the current snapshot;
//     readers never block writers and writers never block readers.
//   - For correctness in the presence of concurrent Refresh calls,
//     callers should treat each invocation of Evaluate as observing a
//     consistent point-in-time rule set.
type ChainEngine struct {
	evaluators []Evaluator
	rules      atomic.Pointer[ruleSnapshot]

	// failClosedOnIngest controls the behavior when an Evaluator panics
	// or returns an error-state result; per the plan, Ingest fails closed
	// (Deny) while Read fails open (Allow). For step 4 this is enforced
	// by callers; the engine itself only catches evaluator panics.
}

// ruleSnapshot is a kind-partitioned, pre-sorted view of the rule set.
type ruleSnapshot struct {
	byKind map[string][]Rule
}

// NewChainEngine constructs an engine with the supplied evaluators and
// an empty rule set. Use Refresh / PullFromStore to populate.
func NewChainEngine(evaluators ...Evaluator) *ChainEngine {
	e := &ChainEngine{evaluators: evaluators}
	e.rules.Store(&ruleSnapshot{byKind: map[string][]Rule{}})
	return e
}

// Refresh atomically replaces the rule cache. Rules are partitioned by
// Kind and sorted (specificity DESC, priority ASC) so evaluators can fold
// them without further work.
func (e *ChainEngine) Refresh(rules []Rule) {
	snap := &ruleSnapshot{byKind: map[string][]Rule{}}
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		snap.byKind[r.Kind] = append(snap.byKind[r.Kind], r)
	}
	for kind := range snap.byKind {
		sortRules(snap.byKind[kind])
	}
	e.rules.Store(snap)
}

// PullFromStore loads enabled rules from store and calls Refresh.
func (e *ChainEngine) PullFromStore(ctx context.Context, store *RuleStore) error {
	rs, err := store.ListEnabled(ctx)
	if err != nil {
		return err
	}
	e.Refresh(rs)
	return nil
}

// Evaluate runs every registered evaluator over the rules whose selector
// matches s, then returns the strictest decision.
func (e *ChainEngine) Evaluate(ctx context.Context, s Subject, a Action) Result {
	snap := e.rules.Load()
	deepest := Result{Decision: Allow}
	for _, ev := range e.evaluators {
		kindRules := snap.byKind[ev.Kind()]
		matched := filterMatching(kindRules, s)
		if len(matched) == 0 {
			// Evaluators may still return a decision when no rules apply
			// (e.g. license_allow with `on_unknown=deny` and no config).
			// Pass an empty slice; they handle it.
		}
		r := safeEvaluate(ctx, ev, s, a, matched)
		if r.Decision > deepest.Decision {
			deepest = r
		}
		if deepest.Decision == Deny {
			break // hard short-circuit
		}
	}
	return deepest
}

// safeEvaluate guards an evaluator from panic, returning an empty Allow
// result if it crashes (so the rest of the chain still runs).
func safeEvaluate(ctx context.Context, ev Evaluator, s Subject, a Action, rules []Rule) (r Result) {
	defer func() {
		if rec := recover(); rec != nil {
			r = Result{Decision: Allow, Reason: ""}
		}
	}()
	return ev.Evaluate(ctx, s, a, rules)
}

func filterMatching(rules []Rule, s Subject) []Rule {
	if len(rules) == 0 {
		return rules
	}
	out := rules[:0:0]
	for _, r := range rules {
		if r.Matches(s) {
			out = append(out, r)
		}
	}
	return out
}

func sortRules(rs []Rule) {
	sort.Slice(rs, func(i, j int) bool {
		si, sj := rs[i].Specificity(), rs[j].Specificity()
		if si != sj {
			return si > sj // higher specificity first
		}
		return rs[i].Priority < rs[j].Priority // lower priority value wins
	})
}

// Compile-time interface check.
var _ Engine = (*ChainEngine)(nil)

// for tests
var _ = sync.RWMutex{}
