package console_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestDashboard_RequiresAuth(t *testing.T) {
	f := newAuthFixture(t)
	resp, _ := f.Get(t, "/console/")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303 to login, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/console/login" {
		t.Errorf("expected redirect to /console/login, got %q", loc)
	}
}

func TestDashboard_RendersForSignedInAdmin(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")

	// Sign in.
	resp := f.PostForm(t, "/console/login", url.Values{
		"username": {"alice"}, "password": {"correct-password-12chars"},
	})
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login failed: %d", resp.StatusCode)
	}

	// Hit dashboard.
	page, body := f.Get(t, "/console/")
	if page.StatusCode != http.StatusOK {
		t.Fatalf("dashboard = %d; body=%s", page.StatusCode, body)
	}
	// Spot-check rendered fragments.
	for _, want := range []string{
		"<h1>Dashboard</h1>",
		"alice",  // signed-in name in subtitle
		"Tenants",
		"Packages",
		"Versions",
		"Recent ingests",
		"Policy decisions",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in dashboard body", want)
		}
	}
}

func TestDashboard_RendersEmptyStateGracefully(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")

	resp := f.PostForm(t, "/console/login", url.Values{
		"username": {"alice"}, "password": {"correct-password-12chars"},
	})
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	// On a fresh DB there are no ingest events and no policy
	// decisions. The empty-state alerts should render.
	_, body := f.Get(t, "/console/")
	if !strings.Contains(body, "No ingest events yet") {
		t.Error("expected empty-state alert for ingests")
	}
	if !strings.Contains(body, "Quiet") {
		t.Error("expected empty-state alert for decisions")
	}
}
