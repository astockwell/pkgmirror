// Package cooldown implements the "release-age" supply-chain control.
//
// A cooldown rule denies (or quarantines / warns about) a version until
// it has been on the mirror for at least N days. The intent is to give
// the upstream ecosystem time to react to compromised or buggy releases
// before they propagate into your build pipeline. This is the same
// concept as uv's --exclude-newer and Renovate's minimumReleaseAge.
//
// Per plans/supply-chain-policy-engine.md §6.1, the default age source
// is Subject.Attrs["ingest_age_seconds"] — wall-clock minus the local
// row's created_unix. As of PR G (plans/upstream-pull-through.md §6.1),
// a rule may set time_source: upstream_publish to instead read
// Subject.Attrs["upstream_published_unix"]; this defends against
// upstream just-released compromises by gating on when the version
// appeared upstream rather than when we happened to first ingest it.
// When upstream_publish is requested but the attribute is absent (a
// locally-uploaded version, or a format whose adapter doesn't populate
// it), the evaluator falls back to ingest_age with a Reason that calls
// out the fallback so the audit row is searchable.
package cooldown

import (
	"context"
	"fmt"
	"time"

	"github.com/astockwell/pkgmirror/internal/policy"
)

// Kind is the rule kind that this evaluator handles.
const Kind = "cooldown"

// TimeSource selects which timestamp the cooldown is measured from.
type TimeSource string

const (
	// TimeSourceIngest is the default: cooldown measured from the
	// local row's created_unix. Equivalent to "how long has this been
	// on our mirror".
	TimeSourceIngest TimeSource = "ingest"
	// TimeSourceUpstreamPublish measures from upstream_published_unix
	// (e.g. PyPI's upload_time_iso_8601). Defends against upstream
	// just-released compromises - a 0-day attack landed via mirror
	// pull-through gets blocked even though we ingested it seconds ago.
	TimeSourceUpstreamPublish TimeSource = "upstream_publish"
)

// Config is the JSON shape stored in policy_rules.config_json.
type Config struct {
	// MinAgeDays is the minimum age a version must reach before the
	// rule's action releases. A value of 0 (or negative) is treated as
	// "no cooldown" and the evaluator returns Allow even with a
	// matching rule — this is how operators express a fast-path
	// exemption ("requests is always OK").
	MinAgeDays int `json:"min_age_days"`

	// TimeSource selects the source of "age". Empty string defaults to
	// "ingest" (the only behavior before PR G). When set to
	// "upstream_publish" and the subject lacks upstream_published_unix,
	// the evaluator falls back to ingest age and surfaces the fallback
	// in Result.Reason so audits stay legible.
	TimeSource TimeSource `json:"time_source,omitempty"`
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

	age, source, ok := ageSecondsFor(s, cfg.TimeSource)
	if !ok {
		// Caller didn't supply an age via any acceptable source.
		// Don't block — log it so the audit row tells us why.
		return policy.Result{
			Decision: policy.Allow,
			Reason:   "cooldown: no age available (ingest_age_seconds and upstream_published_unix both missing)",
			RuleID:   winner.ID,
		}
	}

	threshold := int64(cfg.MinAgeDays) * 86400
	if age >= threshold {
		// Pass-through. Note which source we used so the audit log
		// captures it for fallback cases.
		reason := ""
		if cfg.TimeSource == TimeSourceUpstreamPublish && source == TimeSourceIngest {
			reason = "cooldown: time_source=upstream_publish requested but upstream_published_unix unavailable; fell back to ingest age"
		}
		return policy.Result{Decision: policy.Allow, RuleID: winner.ID, Reason: reason}
	}

	decision := decisionFromAction(winner.Action)
	suffix := ""
	if cfg.TimeSource == TimeSourceUpstreamPublish && source == TimeSourceIngest {
		suffix = " (fell back to ingest age - upstream_published_unix unavailable)"
	}
	return policy.Result{
		Decision: decision,
		Reason: fmt.Sprintf("cooldown: version is %d days old (source=%s), rule %q requires %d (%s)%s",
			age/86400, source, winner.Name, cfg.MinAgeDays, winner.Action, suffix),
		RuleID: winner.ID,
	}
}

// ageSeconds extracts the age from Subject.Attrs. Accepts int64, int, or
// float64 (encoders sometimes coerce). Returns (0, false) if missing.
//
// Kept for backwards compatibility with callers that don't know about
// TimeSource. New code should call ageSecondsFor.
func ageSeconds(s policy.Subject) (int64, bool) {
	v, ok := s.Attrs["ingest_age_seconds"]
	if !ok {
		return 0, false
	}
	return coerceInt64(v)
}

// ageSecondsFor resolves the age according to the configured TimeSource.
// Returns (age_seconds, actually_used_source, ok).
//
// When source == TimeSourceUpstreamPublish:
//   - if upstream_published_unix present and > 0, compute wall-clock - it
//   - if absent or 0, fall back to ingest_age_seconds and report the
//     fallback via the returned source.
//
// When source is empty or TimeSourceIngest:
//   - read ingest_age_seconds directly.
func ageSecondsFor(s policy.Subject, source TimeSource) (int64, TimeSource, bool) {
	if source == TimeSourceUpstreamPublish {
		if v, ok := s.Attrs["upstream_published_unix"]; ok {
			if pub, ok := coerceInt64(v); ok && pub > 0 {
				return time.Now().Unix() - pub, TimeSourceUpstreamPublish, true
			}
		}
		// Fall through to ingest.
	}
	if age, ok := ageSeconds(s); ok {
		return age, TimeSourceIngest, true
	}
	return 0, "", false
}

// coerceInt64 handles the int64/int/float64 variance you get from
// different config decoders.
func coerceInt64(v any) (int64, bool) {
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
