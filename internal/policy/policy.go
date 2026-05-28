// Package policy is the supply-chain policy engine for pkgmirror.
//
// Every supply-chain control (cooldown, license allowlist, blocklist,
// future evaluators like OSV/typosquat/sigstore) plugs into the same
// Engine interface, which receives a Subject + Action and returns a
// Decision. Handlers know only about the Engine; per-kind logic lives in
// Evaluator implementations.
//
// See plans/supply-chain-policy-engine.md for the design.
package policy

import "context"

// Action identifies what the caller is trying to do with the Subject.
type Action int

const (
	// ActionIngest covers both direct uploads (PUT/POST) and pull-through
	// fetches from upstream registries.
	ActionIngest Action = iota
	// ActionRead covers serving file bytes AND including a version in
	// index listings (/simple/<name>/, /@v/list, …).
	ActionRead
)

// String renders an Action for logs and audit rows.
func (a Action) String() string {
	switch a {
	case ActionIngest:
		return "ingest"
	case ActionRead:
		return "read"
	}
	return "unknown"
}

// Decision is the outcome of a policy evaluation. Decisions compare in
// strictness order: Allow < Warn < Quarantine < Deny. When multiple
// evaluators contribute to a single Evaluate() call, the strictest
// decision wins.
type Decision int

const (
	// Allow lets the action proceed normally.
	Allow Decision = iota
	// Warn lets the action proceed but records it for review.
	Warn
	// Quarantine stores the artifact (on Ingest) but hides it from indices
	// and downloads (on Read). Admin tooling can promote a quarantined
	// version to Allow.
	Quarantine
	// Deny refuses the action outright (403 for Read, 403 for Ingest).
	Deny
)

// String renders a Decision for logs and audit rows.
func (d Decision) String() string {
	switch d {
	case Allow:
		return "allow"
	case Warn:
		return "warn"
	case Quarantine:
		return "quarantine"
	case Deny:
		return "deny"
	}
	return "unknown"
}

// Subject is the thing being acted on. Handlers populate the structural
// fields; the Engine populates Attrs lazily via registered enrichers.
//
// Format is a string (e.g. "go", "pypi") rather than models.Type so this
// package has no upward dependency on internal/models.
type Subject struct {
	TenantID int64
	Format   string
	Package  string // canonical lookup key (PEP-503-normalized for PyPI, etc.)
	Version  string
	Filename string

	// Attrs holds computed / enriched facts used by evaluators.
	// Standard keys (when populated):
	//
	//	"ingest_age_seconds"  int64
	//	"license"             string  (SPDX expression)
	//	"created_unix"        int64
	//
	// Future evaluators may add: "risk_score", "osv_severity",
	// "sigstore_verified", "maintainers_count", …
	Attrs map[string]any
}

// Result is the outcome of an evaluation.
type Result struct {
	Decision Decision
	Reason   string // human-readable; surfaced in HTTP body + audit log
	RuleID   int64  // 0 if no specific rule fired
}

// IsAllowed reports whether the result lets the action proceed unmodified.
func (r Result) IsAllowed() bool { return r.Decision == Allow }

// IsBlocked reports whether the result hides the subject from listings
// or refuses access. Quarantine and Deny both count.
func (r Result) IsBlocked() bool { return r.Decision >= Quarantine }

// Engine is the single seam that handlers depend on. Implementations may
// be the no-op NoopEngine, a real evaluator chain, or a decorated engine
// that adds audit logging.
type Engine interface {
	Evaluate(ctx context.Context, s Subject, a Action) Result
}
