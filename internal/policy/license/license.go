// Package license implements an SPDX-allowlist supply-chain control.
//
// The rule's allowlist replaces (never merges with) less-specific
// rules — per plans/supply-chain-policy-engine.md §6.2 — so an operator
// can both relax and tighten the policy at a more-specific scope.
//
// The package_versions.license column is consulted (via
// Subject.Attrs["license"]) per evaluation. Versions without a known
// license are governed by the rule's on_unknown option.
package license

import (
	"context"
	"fmt"
	"strings"

	"github.com/astockwell/pkgmirror/internal/policy"
)

// Kind is the rule kind handled by this evaluator.
const Kind = "license_allow"

// Config is the JSON shape stored in policy_rules.config_json.
type Config struct {
	// Allow is the list of SPDX identifiers permitted by the rule.
	// Matching is case-insensitive and ignores surrounding whitespace.
	// Empty Allow with on_unknown != "allow" denies everything.
	Allow []string `json:"allow"`

	// OnUnknown controls the verdict when the artifact's license is
	// missing or empty. Valid values: "warn" (default), "deny", "allow".
	OnUnknown string `json:"on_unknown,omitempty"`
}

// Evaluator implements policy.Evaluator for license_allow rules.
type Evaluator struct{}

// New returns a ready-to-register evaluator.
func New() *Evaluator { return &Evaluator{} }

// Kind reports the rule kind handled by this evaluator.
func (*Evaluator) Kind() string { return Kind }

// Evaluate applies the most-specific matching rule per plan §6.2.
func (*Evaluator) Evaluate(_ context.Context, s policy.Subject, _ policy.Action, rules []policy.Rule) policy.Result {
	if len(rules) == 0 {
		return policy.Result{Decision: policy.Allow}
	}
	winner := rules[0]

	var cfg Config
	if err := winner.DecodeConfig(&cfg); err != nil {
		// Misconfigured rule should not lock the registry.
		return policy.Result{
			Decision: policy.Allow,
			Reason:   fmt.Sprintf("license_allow: bad config in rule %q: %v", winner.Name, err),
			RuleID:   winner.ID,
		}
	}

	got := strings.TrimSpace(licenseString(s))
	if got == "" {
		return handleUnknown(winner, cfg)
	}

	if matchesAny(got, cfg.Allow) {
		return policy.Result{Decision: policy.Allow, RuleID: winner.ID}
	}
	return policy.Result{
		Decision: decisionFromAction(winner.Action),
		Reason: fmt.Sprintf("license_allow: license %q not in allowlist %v (rule %q)",
			got, cfg.Allow, winner.Name),
		RuleID: winner.ID,
	}
}

func handleUnknown(rule policy.Rule, cfg Config) policy.Result {
	mode := strings.ToLower(strings.TrimSpace(cfg.OnUnknown))
	if mode == "" {
		mode = "warn"
	}
	switch mode {
	case "allow":
		return policy.Result{Decision: policy.Allow, RuleID: rule.ID}
	case "deny":
		return policy.Result{
			Decision: decisionFromAction(rule.Action),
			Reason:   fmt.Sprintf("license_allow: license unknown (rule %q: on_unknown=deny)", rule.Name),
			RuleID:   rule.ID,
		}
	default: // "warn" or anything else
		return policy.Result{
			Decision: policy.Warn,
			Reason:   fmt.Sprintf("license_allow: license unknown (rule %q)", rule.Name),
			RuleID:   rule.ID,
		}
	}
}

func licenseString(s policy.Subject) string {
	v, ok := s.Attrs["license"]
	if !ok {
		return ""
	}
	if str, ok := v.(string); ok {
		return str
	}
	return ""
}

func matchesAny(license string, allow []string) bool {
	lower := strings.ToLower(strings.TrimSpace(license))
	for _, candidate := range allow {
		if strings.EqualFold(lower, strings.TrimSpace(candidate)) {
			return true
		}
	}
	return false
}

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
