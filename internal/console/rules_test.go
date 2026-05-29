package console_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/astockwell/pkgmirror/internal/policy"
)

func TestRulesList_EmptyState(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	f.loginAs(t, "alice", "correct-password-12chars")

	_, body := f.Get(t, "/console/rules")
	if !strings.Contains(body, "No rules configured") {
		t.Errorf("expected empty-state alert; got %s", body)
	}
}

func TestRuleCreate_HappyPath(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	f.loginAs(t, "alice", "correct-password-12chars")

	resp := f.PostFormFrom(t, "/console/rules/new", "/console/rules", url.Values{
		"name":        {"test-cooldown"},
		"kind":        {"cooldown"},
		"action":      {"warn"},
		"priority":    {"50"},
		"format":      {"npm"},
		"config_json": {`{"days":7}`},
		"enabled":     {"on"},
	})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d; body=%s", resp.StatusCode, body)
	}
	r, err := f.Rules.GetByName(context.Background(), "test-cooldown")
	if err != nil {
		t.Fatalf("GetByName after create: %v", err)
	}
	if r.Action != "warn" || r.Format != "npm" || r.Priority != 50 {
		t.Errorf("rule not stored as posted: %+v", r)
	}
}

func TestRuleCreate_RejectsBadJSON(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	f.loginAs(t, "alice", "correct-password-12chars")

	resp := f.PostFormFrom(t, "/console/rules/new", "/console/rules", url.Values{
		"name":        {"bad-json"},
		"kind":        {"cooldown"},
		"action":      {"warn"},
		"priority":    {"50"},
		"config_json": {"{not-json"},
		"enabled":     {"on"},
	})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "valid JSON") {
		t.Errorf("expected JSON validation error; got %s", body)
	}
}

func TestRuleToggleEnabled(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	id, err := f.Rules.Upsert(context.Background(), policy.Rule{
		Name: "toggle-me", Kind: "cooldown", Action: "warn",
		Priority: 100, Enabled: true,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	f.loginAs(t, "alice", "correct-password-12chars")

	resp := f.PostFormFrom(t, "/console/rules", "/console/rules/"+itoa(id)+"/enabled", url.Values{
		"enabled": {"false"},
	})
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("toggle expected 303, got %d", resp.StatusCode)
	}
	r, _ := f.Rules.GetByID(context.Background(), id)
	if r.Enabled {
		t.Error("expected rule to be disabled")
	}
}

func TestRuleDelete(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	id, _ := f.Rules.Upsert(context.Background(), policy.Rule{
		Name: "rip", Kind: "cooldown", Action: "warn",
		Priority: 100, Enabled: true,
	})
	f.loginAs(t, "alice", "correct-password-12chars")

	resp := f.PostFormFrom(t, "/console/rules", "/console/rules/"+itoa(id)+"/delete", url.Values{})
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete expected 303, got %d", resp.StatusCode)
	}
	if _, err := f.Rules.GetByID(context.Background(), id); err == nil {
		t.Error("expected rule to be gone")
	}
}
