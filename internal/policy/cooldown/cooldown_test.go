package cooldown

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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

// ============================================================
// PR G: TimeSource: upstream_publish
// ============================================================

// subjWithPublish builds a subject with BOTH ingest_age_seconds (1 hour)
// and upstream_published_unix (`pubAgeDays` days ago). The two times
// differ so tests can verify which one the evaluator actually used.
func subjWithPublish(t *testing.T, ingestAgeSeconds int64, pubAgeDays int64) policy.Subject {
	t.Helper()
	return policy.Subject{
		TenantID: 1, Format: "pypi", Package: "foo", Version: "1.0.0",
		Attrs: map[string]any{
			"ingest_age_seconds":      ingestAgeSeconds,
			"upstream_published_unix": time.Now().Unix() - pubAgeDays*86400,
		},
	}
}

func TestCooldown_TimeSourceUpstreamPublish_BlocksFreshUpstream(t *testing.T) {
	// Ingest age = 1 day (would pass a 7-day cooldown by ingest); but
	// upstream published only 2 days ago -> when source=upstream_publish,
	// the rule should still block.
	rule := policy.Rule{
		ID: 1, Kind: Kind, Action: "deny", Enabled: true,
		ConfigJSON: mustConfig(t, Config{
			MinAgeDays: 7,
			TimeSource: TimeSourceUpstreamPublish,
		}),
	}
	s := subjWithPublish(t, 1*86400, 2)
	r := New().Evaluate(context.Background(), s, policy.ActionRead, []policy.Rule{rule})
	if r.Decision != policy.Deny {
		t.Fatalf("expected Deny on fresh upstream pub (2 days < 7); got %s reason=%q", r.Decision, r.Reason)
	}
	if !strings.Contains(r.Reason, "upstream_publish") {
		t.Errorf("expected Reason to mention upstream_publish; got %q", r.Reason)
	}
}

func TestCooldown_TimeSourceUpstreamPublish_AllowsOldUpstream(t *testing.T) {
	// Ingest age = 1 hour (would block by ingest); upstream published
	// 30 days ago -> when source=upstream_publish, should pass.
	rule := policy.Rule{
		ID: 1, Kind: Kind, Action: "deny", Enabled: true,
		ConfigJSON: mustConfig(t, Config{
			MinAgeDays: 7,
			TimeSource: TimeSourceUpstreamPublish,
		}),
	}
	s := subjWithPublish(t, 3600, 30)
	r := New().Evaluate(context.Background(), s, policy.ActionRead, []policy.Rule{rule})
	if r.Decision != policy.Allow {
		t.Fatalf("expected Allow when upstream age clears threshold; got %s reason=%q", r.Decision, r.Reason)
	}
}

func TestCooldown_TimeSourceUpstreamPublish_FallsBackToIngestWhenAbsent(t *testing.T) {
	// upstream_published_unix missing entirely (locally-uploaded version);
	// rule asks for upstream_publish. Evaluator should fall back to ingest
	// age and SURFACE the fallback in Reason.
	rule := policy.Rule{
		ID: 1, Kind: Kind, Action: "deny", Enabled: true,
		ConfigJSON: mustConfig(t, Config{
			MinAgeDays: 7,
			TimeSource: TimeSourceUpstreamPublish,
		}),
	}
	// Ingest age = 1 day (below 7-day threshold).
	s := policy.Subject{
		TenantID: 1,
		Attrs:    map[string]any{"ingest_age_seconds": int64(86400)},
	}
	r := New().Evaluate(context.Background(), s, policy.ActionRead, []policy.Rule{rule})
	if r.Decision != policy.Deny {
		t.Fatalf("expected Deny via fallback to ingest age (1 < 7); got %s", r.Decision)
	}
	if !strings.Contains(r.Reason, "fell back to ingest") {
		t.Errorf("expected fallback annotation in Reason; got %q", r.Reason)
	}
}

func TestCooldown_TimeSourceUpstreamPublish_ZeroValueFallsBack(t *testing.T) {
	// upstream_published_unix present but 0 (some adapter wrote a
	// sentinel) - treat as absent and fall back.
	rule := policy.Rule{
		ID: 1, Kind: Kind, Action: "deny", Enabled: true,
		ConfigJSON: mustConfig(t, Config{
			MinAgeDays: 7,
			TimeSource: TimeSourceUpstreamPublish,
		}),
	}
	s := policy.Subject{
		TenantID: 1,
		Attrs: map[string]any{
			"ingest_age_seconds":      int64(30 * 86400), // 30 days local
			"upstream_published_unix": int64(0),          // adapter wrote 0
		},
	}
	r := New().Evaluate(context.Background(), s, policy.ActionRead, []policy.Rule{rule})
	if r.Decision != policy.Allow {
		t.Fatalf("expected Allow via fallback to ingest age (30 > 7); got %s", r.Decision)
	}
	if !strings.Contains(r.Reason, "fell back to ingest") {
		t.Errorf("expected fallback annotation in Reason; got %q", r.Reason)
	}
}

func TestCooldown_TimeSourceDefaultIsIngest(t *testing.T) {
	// Empty TimeSource string MUST behave exactly like the
	// pre-PR-G code: use ingest_age_seconds.
	rule := policy.Rule{
		ID: 1, Kind: Kind, Action: "deny", Enabled: true,
		// No time_source field in config.
		ConfigJSON: mustConfig(t, Config{MinAgeDays: 7}),
	}
	// Subject has both attrs; verify the evaluator ignores upstream and
	// reads ingest.
	s := policy.Subject{
		TenantID: 1,
		Attrs: map[string]any{
			"ingest_age_seconds":      int64(30 * 86400), // 30 days local
			"upstream_published_unix": time.Now().Unix() - 86400, // 1 day upstream
		},
	}
	r := New().Evaluate(context.Background(), s, policy.ActionRead, []policy.Rule{rule})
	if r.Decision != policy.Allow {
		t.Fatalf("default source=ingest with 30d ingest age should Allow; got %s reason=%q", r.Decision, r.Reason)
	}
}

func TestCooldown_TimeSourceExplicitIngest(t *testing.T) {
	// Explicit time_source: ingest is equivalent to default.
	rule := policy.Rule{
		ID: 1, Kind: Kind, Action: "deny", Enabled: true,
		ConfigJSON: mustConfig(t, Config{
			MinAgeDays: 7,
			TimeSource: TimeSourceIngest,
		}),
	}
	s := policy.Subject{
		TenantID: 1,
		Attrs:    map[string]any{"ingest_age_seconds": int64(86400)},
	}
	r := New().Evaluate(context.Background(), s, policy.ActionRead, []policy.Rule{rule})
	if r.Decision != policy.Deny {
		t.Fatalf("expected Deny (1 < 7) under explicit ingest source; got %s", r.Decision)
	}
}
