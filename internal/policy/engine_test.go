package policy_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	pkgdb "github.com/astockwell/pkgmirror/internal/db"
	"github.com/astockwell/pkgmirror/internal/policy"
)

// fakeResolver maps tenant names to integer IDs without touching a DB.
type fakeResolver map[string]int64

func (f fakeResolver) ResolveTenantID(_ context.Context, name string) (int64, error) {
	if id, ok := f[name]; ok {
		return id, nil
	}
	return 0, errNoSuchTenant
}

var errNoSuchTenant = &resolveError{}

type resolveError struct{}

func (e *resolveError) Error() string { return "no such tenant" }

// recordingEvaluator captures every Evaluate call so we can assert
// dispatch + folding behavior of the ChainEngine.
type recordingEvaluator struct {
	kind  string
	calls []recCall
	reply policy.Result
}

type recCall struct {
	Subject policy.Subject
	Action  policy.Action
	Rules   []policy.Rule
}

func (e *recordingEvaluator) Kind() string { return e.kind }
func (e *recordingEvaluator) Evaluate(_ context.Context, s policy.Subject, a policy.Action, rules []policy.Rule) policy.Result {
	e.calls = append(e.calls, recCall{Subject: s, Action: a, Rules: append([]policy.Rule(nil), rules...)})
	return e.reply
}

func TestRule_SpecificityAndMatches(t *testing.T) {
	subj := policy.Subject{TenantID: 7, Format: "pypi", Package: "requests", Version: "2.32.0"}

	cases := []struct {
		name      string
		rule      policy.Rule
		wantSpec  int
		wantMatch bool
	}{
		{
			name:      "any",
			rule:      policy.Rule{Enabled: true},
			wantSpec:  0,
			wantMatch: true,
		},
		{
			name:      "tenant only",
			rule:      policy.Rule{TenantID: 7, Enabled: true},
			wantSpec:  4,
			wantMatch: true,
		},
		{
			name:      "tenant + format + package + version",
			rule:      policy.Rule{TenantID: 7, Format: "pypi", PackageLowerName: "requests", VersionPattern: "2.32.0", Enabled: true},
			wantSpec:  30,
			wantMatch: true,
		},
		{
			name:      "wrong tenant",
			rule:      policy.Rule{TenantID: 8, Enabled: true},
			wantSpec:  4,
			wantMatch: false,
		},
		{
			name:      "version glob",
			rule:      policy.Rule{Format: "pypi", VersionPattern: "2.*", Enabled: true},
			wantSpec:  18,
			wantMatch: true,
		},
		{
			name:      "expired",
			rule:      policy.Rule{Format: "pypi", ExpiresUnix: 1, Enabled: true},
			wantSpec:  2,
			wantMatch: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.rule.Specificity(); got != c.wantSpec {
				t.Errorf("specificity: got %d want %d", got, c.wantSpec)
			}
			if got := c.rule.Matches(subj); got != c.wantMatch {
				t.Errorf("matches: got %v want %v", got, c.wantMatch)
			}
		})
	}
}

func TestChainEngine_DispatchAndFold(t *testing.T) {
	cooldownEv := &recordingEvaluator{kind: "cooldown", reply: policy.Result{Decision: policy.Warn, Reason: "cd"}}
	licenseEv := &recordingEvaluator{kind: "license_allow", reply: policy.Result{Decision: policy.Deny, Reason: "lic"}}
	other := &recordingEvaluator{kind: "block", reply: policy.Result{Decision: policy.Quarantine, Reason: "blk"}}

	e := policy.NewChainEngine(cooldownEv, licenseEv, other)
	e.Refresh([]policy.Rule{
		{ID: 1, Kind: "cooldown", Format: "pypi", Action: "warn", Enabled: true},
		{ID: 2, Kind: "license_allow", Action: "deny", Enabled: true},
		// non-matching:
		{ID: 3, Kind: "cooldown", Format: "go", Action: "warn", Enabled: true},
		// disabled:
		{ID: 4, Kind: "cooldown", Action: "deny", Enabled: false},
	})

	r := e.Evaluate(context.Background(),
		policy.Subject{TenantID: 1, Format: "pypi", Package: "foo", Version: "1.0.0"},
		policy.ActionRead)

	// license_allow returned Deny which is strictest; "block" should
	// have been short-circuited and never called.
	if r.Decision != policy.Deny || r.Reason != "lic" {
		t.Fatalf("decision: %+v want Deny lic", r)
	}
	if len(other.calls) != 0 {
		t.Fatalf("post-Deny evaluator was still called %d times", len(other.calls))
	}

	// cooldown evaluator should have seen only the matching pypi rule.
	if got := len(cooldownEv.calls); got != 1 {
		t.Fatalf("cooldown calls: %d", got)
	}
	if got := len(cooldownEv.calls[0].Rules); got != 1 || cooldownEv.calls[0].Rules[0].ID != 1 {
		t.Fatalf("cooldown rules: %+v", cooldownEv.calls[0].Rules)
	}
}

func TestChainEngine_StrictestWins(t *testing.T) {
	a := &recordingEvaluator{kind: "a", reply: policy.Result{Decision: policy.Warn}}
	b := &recordingEvaluator{kind: "b", reply: policy.Result{Decision: policy.Quarantine}}

	e := policy.NewChainEngine(a, b)
	e.Refresh([]policy.Rule{
		{Kind: "a", Action: "warn", Enabled: true},
		{Kind: "b", Action: "quarantine", Enabled: true},
	})

	r := e.Evaluate(context.Background(), policy.Subject{}, policy.ActionRead)
	if r.Decision != policy.Quarantine {
		t.Fatalf("Quarantine should beat Warn: got %s", r.Decision)
	}
}

func TestChainEngine_PanicRecovery(t *testing.T) {
	panicky := panickingEvaluator{}
	calm := &recordingEvaluator{kind: "calm", reply: policy.Result{Decision: policy.Allow}}
	e := policy.NewChainEngine(panicky, calm)
	e.Refresh([]policy.Rule{
		{Kind: "panic", Action: "deny", Enabled: true},
		{Kind: "calm", Action: "warn", Enabled: true},
	})
	// Should not panic; should still allow the calm evaluator to run.
	r := e.Evaluate(context.Background(), policy.Subject{}, policy.ActionIngest)
	if r.Decision != policy.Allow {
		t.Fatalf("got %s, expected Allow after panic recovery", r.Decision)
	}
	if len(calm.calls) != 1 {
		t.Fatalf("calm evaluator was not called after panic")
	}
}

type panickingEvaluator struct{}

func (panickingEvaluator) Kind() string { return "panic" }
func (panickingEvaluator) Evaluate(context.Context, policy.Subject, policy.Action, []policy.Rule) policy.Result {
	panic("boom")
}

func TestRuleStore_UpsertAndList(t *testing.T) {
	db, err := pkgdb.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store := policy.NewRuleStore(db)
	ctx := context.Background()

	r := policy.Rule{
		Name:             "test-cooldown",
		Kind:             "cooldown",
		Format:           "pypi",
		PackageLowerName: "requests",
		Action:           "quarantine",
		Priority:         50,
		Enabled:          true,
		ConfigJSON:       json.RawMessage(`{"min_age_days":7}`),
	}
	id, err := store.Upsert(ctx, r)
	if err != nil || id == 0 {
		t.Fatalf("upsert: id=%d err=%v", id, err)
	}

	// Re-upsert same name should update, not duplicate.
	r.Priority = 75
	id2, err := store.Upsert(ctx, r)
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if id2 != id {
		t.Fatalf("id changed on update: %d -> %d", id, id2)
	}

	rs, err := store.ListEnabled(ctx)
	if err != nil || len(rs) != 1 {
		t.Fatalf("list: %d %v", len(rs), err)
	}
	if rs[0].Priority != 75 || rs[0].Format != "pypi" || string(rs[0].ConfigJSON) != `{"min_age_days":7}` {
		t.Fatalf("round-trip: %+v", rs[0])
	}

	// Disabling removes the rule from ListEnabled.
	if err := store.SetEnabled(ctx, id, false); err != nil {
		t.Fatalf("setenabled: %v", err)
	}
	rs, _ = store.ListEnabled(ctx)
	if len(rs) != 0 {
		t.Fatalf("disabled rule still listed: %d", len(rs))
	}
}

func TestLoadYAMLFile(t *testing.T) {
	yamlContent := `rules:
  - name: org-default-cooldown
    kind: cooldown
    action: quarantine
    config:
      min_age_days: 7
  - name: prod-strict
    kind: cooldown
    tenant: prod
    format: pypi
    package: requests
    action: warn
    priority: 50
    config:
      min_age_days: 21
`
	path := filepath.Join(t.TempDir(), "rules.yaml")
	if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	resolver := fakeResolver{"prod": 42}
	rules, err := policy.LoadYAMLFile(context.Background(), path, resolver)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("got %d rules", len(rules))
	}
	if rules[0].Name != "org-default-cooldown" || rules[0].Kind != "cooldown" || rules[0].TenantID != 0 {
		t.Errorf("rule 0: %+v", rules[0])
	}
	if rules[1].TenantID != 42 || rules[1].PackageLowerName != "requests" || rules[1].Format != "pypi" {
		t.Errorf("rule 1: %+v", rules[1])
	}

	// Verify config JSON encoding round-trips.
	var cfg struct {
		MinAgeDays int `json:"min_age_days"`
	}
	if err := rules[1].DecodeConfig(&cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if cfg.MinAgeDays != 21 {
		t.Errorf("config: %+v", cfg)
	}
}

func TestRefresh_FiltersDisabledAndSortsBySpecificity(t *testing.T) {
	ev := &recordingEvaluator{kind: "cooldown"}
	e := policy.NewChainEngine(ev)
	e.Refresh([]policy.Rule{
		{ID: 1, Kind: "cooldown", Enabled: true},                                                 // spec 0
		{ID: 2, Kind: "cooldown", TenantID: 1, Format: "pypi", Enabled: true},                    // spec 6
		{ID: 3, Kind: "cooldown", TenantID: 1, Format: "pypi", PackageLowerName: "x", Enabled: true}, // spec 14
		{ID: 4, Kind: "cooldown", Enabled: false},                                                // disabled, dropped
	})

	subj := policy.Subject{TenantID: 1, Format: "pypi", Package: "x", Version: "1.0.0"}
	_ = e.Evaluate(context.Background(), subj, policy.ActionRead)
	if len(ev.calls) != 1 {
		t.Fatalf("eval calls: %d", len(ev.calls))
	}
	got := ev.calls[0].Rules
	if len(got) != 3 {
		t.Fatalf("rules len: got %d want 3 (disabled dropped)", len(got))
	}
	// Sorted by specificity DESC: 3, 2, 1.
	wantOrder := []int64{3, 2, 1}
	for i, w := range wantOrder {
		if got[i].ID != w {
			t.Errorf("position %d: got id=%d want %d", i, got[i].ID, w)
		}
	}
}

// Suppress unused-time import warning if test list shrinks.
var _ = time.Now
