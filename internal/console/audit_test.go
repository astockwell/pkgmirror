package console_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/astockwell/pkgmirror/internal/audit"
	"github.com/astockwell/pkgmirror/internal/users"
)

func TestAudit_RendersForAdmin(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	// Drop a couple of audit rows directly.
	f.Audit.Log(audit.Event{Action: "ingest", Format: "npm", Package: "p1", Version: "1.0.0", Decision: "allow"})
	f.Audit.Log(audit.Event{Action: "console.login", ActorKind: "session"})
	f.loginAs(t, "alice", "correct-password-12chars")

	resp, body := f.Get(t, "/console/audit")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	for _, want := range []string{"Audit log", "Action", "Decision"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in body", want)
		}
	}
}

func TestAudit_FilterByAction(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	f.Audit.Log(audit.Event{Action: "ingest", Format: "npm", Package: "keep-me", Decision: "allow"})
	f.Audit.Log(audit.Event{Action: "console.logout", ActorKind: "session"})
	f.loginAs(t, "alice", "correct-password-12chars")
	// Wait briefly for the audit drainer; the buffered logger writes
	// asynchronously.
	awaitAudit(t, f, "console.login")

	_, body := f.Get(t, "/console/audit?action=console.login")
	if !strings.Contains(body, "console.login") {
		t.Errorf("expected console.login row in filtered body; got %s", body)
	}
	if strings.Contains(body, "keep-me") {
		t.Error("filter should have excluded ingest rows; saw keep-me")
	}
}

func TestAudit_NonAdminGet403(t *testing.T) {
	f := newAuthFixture(t)
	u, _ := f.Users.Create(context.Background(), users.CreateOptions{Name: "bob"})
	hash, _ := users.HashPassword("correct-password-12chars")
	_ = f.Users.SetPasswordHash(context.Background(), u.ID, hash)
	f.loginAs(t, "bob", "correct-password-12chars")

	resp, _ := f.Get(t, "/console/audit")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403, got %d", resp.StatusCode)
	}
}

func TestAuditCSV_StreamsRows(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	f.Audit.Log(audit.Event{Action: "ingest", Format: "npm", Package: "csv-test", Version: "1.0.0", Decision: "allow"})
	f.loginAs(t, "alice", "correct-password-12chars")

	resp, err := f.Client.Get(f.Server.URL + "/console/audit.csv")
	if err != nil {
		t.Fatalf("GET csv: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("Content-Type=%q want text/csv prefix", ct)
	}
	if !strings.Contains(string(body), "created_unix,action") {
		t.Errorf("expected CSV header row; got %s", body)
	}
}

// awaitAudit polls the audit log via the page until rowsForAction
// returns nonzero or timeout. The audit Logger is buffered so writes
// are not visible until the drainer goroutine has flushed.
func awaitAudit(t *testing.T, f *authFixture, action string) {
	t.Helper()
	// quickest path: just refresh /console/audit?action=... a couple
	// times; the drainer batches sub-millisecond.
	for i := 0; i < 5; i++ {
		_, body := f.Get(t, "/console/audit?action="+url.QueryEscape(action))
		if strings.Contains(body, action) {
			return
		}
	}
}
