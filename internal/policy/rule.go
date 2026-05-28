package policy

import (
	"context"
	"encoding/json"
	"path"
	"strings"
	"time"
)

// Rule is one row of the policy_rules table.
type Rule struct {
	ID   int64
	Name string
	Kind string

	// Selector: zero/empty fields mean "any" for that dimension.
	TenantID         int64
	Format           string
	PackageLowerName string
	VersionPattern   string

	// Decision payload.
	Action     string          // "deny" | "quarantine" | "warn"
	ConfigJSON json.RawMessage // kind-specific config
	Priority   int

	Enabled         bool
	CreatedUnix     int64
	CreatedByUserID int64
	ExpiresUnix     int64
}

// Specificity is the deterministic score from plan §4.
// Higher = more specific.
func (r Rule) Specificity() int {
	s := 0
	if r.VersionPattern != "" {
		s += 16
	}
	if r.PackageLowerName != "" {
		s += 8
	}
	if r.TenantID != 0 {
		s += 4
	}
	if r.Format != "" {
		s += 2
	}
	return s
}

// Matches reports whether the rule's selectors apply to the subject and
// the rule isn't expired. Disabled rules should not be passed to Matches.
func (r Rule) Matches(s Subject) bool {
	if r.ExpiresUnix != 0 && r.ExpiresUnix < time.Now().Unix() {
		return false
	}
	if r.TenantID != 0 && r.TenantID != s.TenantID {
		return false
	}
	if r.Format != "" && !strings.EqualFold(r.Format, s.Format) {
		return false
	}
	if r.PackageLowerName != "" && r.PackageLowerName != s.Package {
		return false
	}
	if r.VersionPattern != "" {
		ok, err := path.Match(r.VersionPattern, s.Version)
		if err != nil || !ok {
			return false
		}
	}
	return true
}

// DecodeConfig decodes the rule's config_json into out (a pointer to a
// kind-specific config struct).
func (r Rule) DecodeConfig(out any) error {
	if len(r.ConfigJSON) == 0 {
		return nil
	}
	return json.Unmarshal(r.ConfigJSON, out)
}

// Evaluator implements one kind of supply-chain control.
type Evaluator interface {
	// Kind matches Rule.Kind for rules that should be passed to this
	// evaluator.
	Kind() string
	// Evaluate folds the matching rules into a Result. Rules are
	// pre-filtered to ones that Match the subject and sorted
	// specificity DESC, priority ASC.
	Evaluate(ctx context.Context, s Subject, a Action, rules []Rule) Result
}
