// Package cooldown implements the "release-age" supply-chain control.
//
// A cooldown rule denies (or quarantines / warns about) a version until
// it has been on the mirror for at least N days. The intent is to give
// the upstream ecosystem time to react to compromised or buggy releases
// before they propagate into your build pipeline. This is the same
// concept as uv's --exclude-newer and Renovate's minimumReleaseAge.
//
// Per plans/supply-chain-policy-engine.md §6.1, the age source is
// Subject.Attrs["ingest_age_seconds"], populated by handlers from
// package_versions.created_unix. A future pull-through mode may add
// upstream_published_unix and select the source via a per-rule
// time_source config knob; that's an additive change.
package cooldown

import (
	"context"
	"fmt"

	"github.com/astockwell/pkgmirror/internal/policy"
)

// Kind is the rule kind that this evaluator handles.
const Kind = "cooldown"

// Config is the JSON shape stored in policy_rules.config_json.
type Config struct {
	// MinAgeDays is the minimum age a version must reach before the
	// rule's action releases. A value of 0 (or negative) is treated as
	// "no cooldown" and the evaluator returns Allow even with a
	// matching rule — this is how operators express a fast-path
	// exemption ("requests is always OK").
	MinAgeDays int `json:"min_age_days"`
}

// Evaluator implements policy.Evaluator for cooldown rules.
type Evaluator struct{}

// New returns a ready-to-register evaluator.
func New() *Evaluator { return &Evaluator{} }

// Kind reports the rule kind handled by this evaluator.
func (*Evaluator) Kind() string { return Kind }

// Evaluate applies the most-specific matching cooldown rule to the
// subject (per plan §6.1 "highest specificity wins"). Rules arrive
// already sorted by (specificity DESC, priority ASC) so the first
// element is the winner.
func (*Evaluator) Evaluate(_ context.Context, s policy.Subject, _ policy.Action, rules []policy.Rule) policy.Result {
	if len(rules) == 0 {
		return policy.Result{Decision: policy.Allow}
	}
	winner := rules[0]

	var cfg Config
	if err := winner.DecodeConfig(&cfg); err != nil {
		// Fail-safe: a misconfigured rule should not crash the engine.
		// Return Allow with a Reason so the audit log captures it.
		return policy.Result{
			Decision: policy.Allow,
			Reason:   fmt.Sprintf("cooldown: bad config in rule %q: %v", winner.Name, err),
			RuleID:   winner.ID,
		}
	}

	if cfg.MinAgeDays <= 0 {
		// Explicit "no cooldown" override.
		return policy.Result{Decision: policy.Allow, RuleID: winner.ID}
	}

	age, ok := ageSeconds(s)
	if !ok {
		// Caller didn't supply an age. Don't block — log it.
		return policy.Result{
			Decision: policy.Allow,
			Reason:   "cooldown: ingest_age_seconds missing from Subject",
			RuleID:   winner.ID,
		}
	}

	threshold := int64(cfg.MinAgeDays) * 86400
	if age >= threshold {
		return policy.Result{Decision: policy.Allow, RuleID: winner.ID}
	}

	decision := decisionFromAction(winner.Action)
	return policy.Result{
		Decision: decision,
		Reason: fmt.Sprintf("cooldown: version is %d days old, rule %q requires %d (%s)",
			age/86400, winner.Name, cfg.MinAgeDays, winner.Action),
		RuleID: winner.ID,
	}
}

// ageSeconds extracts the age from Subject.Attrs. Accepts int64, int, or
// float64 (encoders sometimes coerce). Returns (0, false) if missing.
func ageSeconds(s policy.Subject) (int64, bool) {
	v, ok := s.Attrs["ingest_age_seconds"]
	if !ok {
		return 0, false
	}
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case float64:
		return int64(x), true
	}
	return 0, false
}

// decisionFromAction maps the rule.Action string ("deny" / "quarantine"
// / "warn") to a Decision. Anything unrecognized falls back to Warn so
// a typo doesn't accidentally lock the registry.
func decisionFromAction(a string) policy.Decision {
	switch a {
	case "deny":
		return policy.Deny
	case "quarantine":
		return policy.Quarantine
	case "warn":
		return policy.Warn
	default:
		return policy.Warn
	}
}
