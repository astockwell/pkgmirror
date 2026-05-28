package license

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/astockwell/pkgmirror/internal/policy"
)

func subjLicense(lic string) policy.Subject {
	s := policy.Subject{TenantID: 1, Format: "pypi", Package: "foo", Version: "1.0.0"}
	if lic != "" {
		s.Attrs = map[string]any{"license": lic}
	} else {
		s.Attrs = map[string]any{}
	}
	return s
}

func ruleConfig(t *testing.T, c Config) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestLicense_NoRulesIsAllow(t *testing.T) {
	r := New().Evaluate(context.Background(), subjLicense("GPL-3.0"), policy.ActionRead, nil)
	if r.Decision != policy.Allow {
		t.Fatalf("got %s, want allow", r.Decision)
	}
}

func TestLicense_OnAllowlistIsAllow(t *testing.T) {
	rule := policy.Rule{
		ID:         1, Kind: Kind, Action: "deny", Enabled: true,
		ConfigJSON: ruleConfig(t, Config{Allow: []string{"MIT", "Apache-2.0"}}),
	}
	r := New().Evaluate(context.Background(), subjLicense("MIT"), policy.ActionIngest, []policy.Rule{rule})
	if r.Decision != policy.Allow {
		t.Fatalf("MIT should pass MIT/Apache; got %s", r.Decision)
	}
}

func TestLicense_OffAllowlistMapsToAction(t *testing.T) {
	cases := []struct {
		action string
		want   policy.Decision
	}{
		{"deny", policy.Deny},
		{"quarantine", policy.Quarantine},
		{"warn", policy.Warn},
		{"weird", policy.Warn}, // fallback
	}
	for _, c := range cases {
		t.Run(c.action, func(t *testing.T) {
			rule := policy.Rule{
				ID: 7, Kind: Kind, Action: c.action, Enabled: true,
				ConfigJSON: ruleConfig(t, Config{Allow: []string{"MIT"}}),
			}
			r := New().Evaluate(context.Background(), subjLicense("GPL-3.0"), policy.ActionIngest, []policy.Rule{rule})
			if r.Decision != c.want {
				t.Errorf("decision: got %s want %s", r.Decision, c.want)
			}
			if r.RuleID != 7 {
				t.Errorf("rule id: got %d want 7", r.RuleID)
			}
		})
	}
}

func TestLicense_CaseInsensitive(t *testing.T) {
	rule := policy.Rule{
		Kind: Kind, Action: "deny", Enabled: true,
		ConfigJSON: ruleConfig(t, Config{Allow: []string{"MIT"}}),
	}
	r := New().Evaluate(context.Background(), subjLicense("mit"), policy.ActionRead, []policy.Rule{rule})
	if r.Decision != policy.Allow {
		t.Errorf("case mismatch should not block: got %s", r.Decision)
	}
}

func TestLicense_OnUnknown(t *testing.T) {
	cases := []struct {
		mode string
		want policy.Decision
	}{
		{"", policy.Warn},      // default
		{"warn", policy.Warn},
		{"allow", policy.Allow},
		{"deny", policy.Deny},  // rule.Action="deny" → Deny
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			rule := policy.Rule{
				Kind: Kind, Action: "deny", Enabled: true,
				ConfigJSON: ruleConfig(t, Config{Allow: []string{"MIT"}, OnUnknown: c.mode}),
			}
			r := New().Evaluate(context.Background(), subjLicense(""), policy.ActionRead, []policy.Rule{rule})
			if r.Decision != c.want {
				t.Errorf("on_unknown=%q: got %s want %s", c.mode, r.Decision, c.want)
			}
		})
	}
}

func TestLicense_MostSpecificReplaces(t *testing.T) {
	// Per plan §6.2 the more-specific rule REPLACES the less-specific one
	// (no merging of allowlists). Engine pre-sorts; evaluator uses rules[0].
	rules := []policy.Rule{
		{ID: 1, Kind: Kind, Action: "deny", Enabled: true,
			ConfigJSON: ruleConfig(t, Config{Allow: []string{"GPL-3.0"}})}, // narrower, but only GPL allowed
		{ID: 2, Kind: Kind, Action: "deny", Enabled: true,
			ConfigJSON: ruleConfig(t, Config{Allow: []string{"MIT", "Apache-2.0"}})}, // generic
	}
	// With rule 1 winning, "MIT" is NOT in its allowlist → Deny.
	r := New().Evaluate(context.Background(), subjLicense("MIT"), policy.ActionIngest, rules)
	if r.Decision != policy.Deny {
		t.Fatalf("more-specific rule should replace generic allowlist; got %s", r.Decision)
	}
	if r.RuleID != 1 {
		t.Fatalf("expected rule 1 to be reported; got %d", r.RuleID)
	}
}

func TestLicense_BadConfigIsAllow(t *testing.T) {
	rule := policy.Rule{
		ID: 5, Kind: Kind, Action: "deny", Enabled: true,
		ConfigJSON: []byte(`not json`),
	}
	r := New().Evaluate(context.Background(), subjLicense("MIT"), policy.ActionIngest, []policy.Rule{rule})
	if r.Decision != policy.Allow {
		t.Fatalf("bad config should not Deny; got %s", r.Decision)
	}
	if r.Reason == "" {
		t.Fatalf("expected a Reason explaining the bad config")
	}
}
