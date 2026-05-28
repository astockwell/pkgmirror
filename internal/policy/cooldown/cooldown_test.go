package cooldown

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/astockwell/pkgmirror/internal/policy"
)

func subj(ageDays int) policy.Subject {
	return policy.Subject{
		TenantID: 1,
		Format:   "pypi",
		Package:  "foo",
		Version:  "1.0.0",
		Attrs: map[string]any{
			"ingest_age_seconds": int64(ageDays) * 86400,
		},
	}
}

func mustConfig(t *testing.T, c Config) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCooldown_NoRulesIsAllow(t *testing.T) {
	r := New().Evaluate(context.Background(), subj(0), policy.ActionRead, nil)
	if r.Decision != policy.Allow {
		t.Fatalf("got %s, want allow", r.Decision)
	}
}

func TestCooldown_BelowThresholdEnforcesAction(t *testing.T) {
	cases := []struct {
		action string
		want   policy.Decision
	}{
		{"deny", policy.Deny},
		{"quarantine", policy.Quarantine},
		{"warn", policy.Warn},
		{"unknown-action", policy.Warn}, // safety fallback
	}
	for _, c := range cases {
		t.Run(c.action, func(t *testing.T) {
			rule := policy.Rule{
				ID:         42,
				Name:       "test",
				Kind:       Kind,
				Action:     c.action,
				ConfigJSON: mustConfig(t, Config{MinAgeDays: 7}),
				Enabled:    true,
			}
			r := New().Evaluate(context.Background(), subj(3), policy.ActionRead, []policy.Rule{rule})
			if r.Decision != c.want {
				t.Errorf("decision: got %s want %s", r.Decision, c.want)
			}
			if r.RuleID != 42 {
				t.Errorf("rule_id: got %d want 42", r.RuleID)
			}
		})
	}
}

func TestCooldown_AboveThresholdIsAllow(t *testing.T) {
	rule := policy.Rule{
		Kind:       Kind,
		Action:     "deny",
		ConfigJSON: mustConfig(t, Config{MinAgeDays: 7}),
		Enabled:    true,
	}
	r := New().Evaluate(context.Background(), subj(8), policy.ActionRead, []policy.Rule{rule})
	if r.Decision != policy.Allow {
		t.Fatalf("got %s, want allow once threshold cleared", r.Decision)
	}
}

func TestCooldown_ZeroMinAgeIsExemption(t *testing.T) {
	rule := policy.Rule{
		ID:         99,
		Name:       "fastpath",
		Kind:       Kind,
		Action:     "deny",
		ConfigJSON: mustConfig(t, Config{MinAgeDays: 0}),
		Enabled:    true,
	}
	// Even a brand-new version (age 0) is allowed when min_age_days = 0.
	r := New().Evaluate(context.Background(), subj(0), policy.ActionRead, []policy.Rule{rule})
	if r.Decision != policy.Allow {
		t.Fatalf("got %s, want allow (fast-path exemption)", r.Decision)
	}
	if r.RuleID != 99 {
		t.Fatalf("expected the matching exemption rule id to be recorded")
	}
}

func TestCooldown_HighestSpecificityWins(t *testing.T) {
	// Rules are pre-sorted by the engine; this test pins the contract
	// that the cooldown evaluator only consults rules[0].
	rules := []policy.Rule{
		{ID: 1, Kind: Kind, Action: "deny", ConfigJSON: mustConfig(t, Config{MinAgeDays: 0}), Enabled: true},  // fastpath, more specific
		{ID: 2, Kind: Kind, Action: "deny", ConfigJSON: mustConfig(t, Config{MinAgeDays: 30}), Enabled: true}, // generic, less specific
	}
	r := New().Evaluate(context.Background(), subj(1), policy.ActionRead, rules)
	if r.Decision != policy.Allow {
		t.Fatalf("most-specific (min=0) should win and Allow; got %s", r.Decision)
	}
	if r.RuleID != 1 {
		t.Fatalf("expected rule 1 to be reported; got %d", r.RuleID)
	}
}

func TestCooldown_MissingAgeAttrIsAllowWithReason(t *testing.T) {
	rule := policy.Rule{
		ID:         7,
		Kind:       Kind,
		Action:     "deny",
		ConfigJSON: mustConfig(t, Config{MinAgeDays: 7}),
		Enabled:    true,
	}
	noAge := policy.Subject{TenantID: 1, Attrs: map[string]any{}}
	r := New().Evaluate(context.Background(), noAge, policy.ActionRead, []policy.Rule{rule})
	if r.Decision != policy.Allow {
		t.Fatalf("missing age should not Deny; got %s", r.Decision)
	}
	if r.Reason == "" {
		t.Fatalf("expected a Reason explaining why")
	}
}

func TestCooldown_BadConfigIsAllow(t *testing.T) {
	rule := policy.Rule{
		ID:         3,
		Kind:       Kind,
		Action:     "deny",
		ConfigJSON: []byte(`{not valid json`),
		Enabled:    true,
	}
	r := New().Evaluate(context.Background(), subj(0), policy.ActionRead, []policy.Rule{rule})
	if r.Decision != policy.Allow {
		t.Fatalf("bad config should not Deny; got %s", r.Decision)
	}
	if r.Reason == "" {
		t.Fatalf("expected a Reason explaining the error")
	}
}
