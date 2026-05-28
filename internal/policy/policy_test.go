package policy

import (
	"context"
	"testing"
)

func TestDecisionOrdering(t *testing.T) {
	// The strictest-wins rule depends on this ordering. If anyone adds
	// a new Decision constant they must keep the order from least to
	// most strict — this test pins it.
	if !(Allow < Warn && Warn < Quarantine && Quarantine < Deny) {
		t.Fatalf("Decision constants out of order: %d %d %d %d",
			Allow, Warn, Quarantine, Deny)
	}
}

func TestResultHelpers(t *testing.T) {
	cases := []struct {
		decision        Decision
		wantAllowed     bool
		wantBlocked     bool
	}{
		{Allow, true, false},
		{Warn, false, false},
		{Quarantine, false, true},
		{Deny, false, true},
	}
	for _, c := range cases {
		r := Result{Decision: c.decision}
		if got := r.IsAllowed(); got != c.wantAllowed {
			t.Errorf("%s.IsAllowed = %v, want %v", c.decision, got, c.wantAllowed)
		}
		if got := r.IsBlocked(); got != c.wantBlocked {
			t.Errorf("%s.IsBlocked = %v, want %v", c.decision, got, c.wantBlocked)
		}
	}
}

func TestNoopEngine(t *testing.T) {
	e := NoopEngine{}
	for _, a := range []Action{ActionIngest, ActionRead} {
		r := e.Evaluate(context.Background(), Subject{TenantID: 1, Format: "go"}, a)
		if r.Decision != Allow {
			t.Errorf("NoopEngine %s: got %s want allow", a, r.Decision)
		}
	}
}
