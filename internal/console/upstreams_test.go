package console_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/astockwell/pkgmirror/internal/tenants"
	"github.com/astockwell/pkgmirror/internal/users"
)

// ============================================================
// /console/tenants/:name/upstreams
// ============================================================
//
// Coverage targets:
//   - GET: anonymous redirects to login (RequireAuth)
//   - GET: non-admin gets 403 (RequireSystemAdmin)
//   - GET: admin sees row for every compiled-in format
//   - GET: persisted row appears as "custom" with the saved URL
//   - POST upsert: invalid mode rejected
//   - POST upsert: invalid URL rejected
//   - POST upsert: huge TTL rejected
//   - POST upsert: valid update persists, redirects, audit row written
//   - POST delete: removes the override (resolver falls back to default)
//   - POST upsert: no CSRF token -> 403

func TestUpstreams_AnonRedirectsToLogin(t *testing.T) {
	f := newAuthFixture(t)
	if _, err := f.Tenants.Create(context.Background(), "demo", tenants.VisibilityPrivate); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	resp, _ := f.Get(t, "/console/tenants/demo/upstreams")
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("expected 303, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/console/login") {
		t.Errorf("expected redirect to login, got %q", loc)
	}
}

func TestUpstreams_NonAdminGets403(t *testing.T) {
	f := newAuthFixture(t)
	if _, err := f.Tenants.Create(context.Background(), "demo", tenants.VisibilityPrivate); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	// Create a regular user with a password + log in.
	u, err := f.Users.Create(context.Background(), users.CreateOptions{
		Name: "bob", Kind: users.KindHuman,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	hash, err := users.HashPassword("plain-password-12char")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := f.Users.SetPasswordHash(context.Background(), u.ID, hash); err != nil {
		t.Fatalf("set password: %v", err)
	}
	loginAs(t, f, "bob", "plain-password-12char")

	resp, _ := f.Get(t, "/console/tenants/demo/upstreams")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403, got %d", resp.StatusCode)
	}
}

func TestUpstreams_AdminSeesRowPerFormat(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	loginAs(t, f, "alice", "correct-password-12chars")
	if _, err := f.Tenants.Create(context.Background(), "demo", tenants.VisibilityPrivate); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	resp, body := f.Get(t, "/console/tenants/demo/upstreams")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	for _, want := range []string{"pypi", "npm", "rubygems", "maven", "go", "cran", "alpine", "debian", "rpm", "nuget"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected page to mention format %q", want)
		}
	}
	// All formats start as "default" (no override). Test the badge.
	if !strings.Contains(body, "default") {
		t.Error("expected 'default' badge for at least one row")
	}
	// PyPI's allowlisted host should appear in the help text.
	if !strings.Contains(body, "files.pythonhosted.org") {
		t.Error("expected pypi allowlisted hosts to be rendered")
	}
}

func TestUpstreams_UpsertValidPersistsAndAudits(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	loginAs(t, f, "alice", "correct-password-12chars")
	if _, err := f.Tenants.Create(context.Background(), "demo", tenants.VisibilityPrivate); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	resp := f.PostFormFrom(t, "/console/tenants/demo/upstreams", "/console/tenants/demo/upstreams/pypi", url.Values{
		"mode":             {"cache_only"},
		"upstream_url":     {"https://pypi.org"},
		"metadata_ttl_sec": {"120"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	// Re-fetch the page and verify the row now reads "custom".
	_, body := f.Get(t, "/console/tenants/demo/upstreams")
	if !strings.Contains(body, "custom") {
		t.Error("expected page to show the row as 'custom'")
	}
	if !strings.Contains(body, "cache_only") {
		t.Error("expected cache_only mode to render selected")
	}
	if !strings.Contains(body, "https://pypi.org") {
		t.Error("expected saved URL to render in the input")
	}

	// Verify an audit row was written.
	var rows []string
	r, err := f.DB.QueryContext(context.Background(),
		`SELECT action FROM audit_log ORDER BY id DESC LIMIT 5`)
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	defer r.Close()
	for r.Next() {
		var a string
		_ = r.Scan(&a)
		rows = append(rows, a)
	}
	found := false
	for _, a := range rows {
		if a == "tenants.upstream.update" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected tenants.upstream.update in audit log; got %v", rows)
	}
}

func TestUpstreams_RejectsInvalidMode(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	loginAs(t, f, "alice", "correct-password-12chars")
	_, _ = f.Tenants.Create(context.Background(), "demo", tenants.VisibilityPrivate)
	resp := f.PostFormFrom(t, "/console/tenants/demo/upstreams", "/console/tenants/demo/upstreams/pypi", url.Values{
		"mode":             {"nonsense"},
		"upstream_url":     {""},
		"metadata_ttl_sec": {""},
	})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected re-render 200, got %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "Mode must be") {
		t.Errorf("expected validation message in body; got %s", body)
	}
}

func TestUpstreams_RejectsBadURL(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	loginAs(t, f, "alice", "correct-password-12chars")
	_, _ = f.Tenants.Create(context.Background(), "demo", tenants.VisibilityPrivate)
	resp := f.PostFormFrom(t, "/console/tenants/demo/upstreams", "/console/tenants/demo/upstreams/pypi", url.Values{
		"mode":         {"cache_and_serve"},
		"upstream_url": {"not-a-url"},
	})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "fully-qualified") {
		t.Errorf("expected URL validation error; got %s", body)
	}
}

func TestUpstreams_RejectsHugeTTL(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	loginAs(t, f, "alice", "correct-password-12chars")
	_, _ = f.Tenants.Create(context.Background(), "demo", tenants.VisibilityPrivate)
	resp := f.PostFormFrom(t, "/console/tenants/demo/upstreams", "/console/tenants/demo/upstreams/pypi", url.Values{
		"mode":             {"cache_and_serve"},
		"metadata_ttl_sec": {"999999"},
	})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Metadata TTL") {
		t.Errorf("expected TTL validation error; got %s", body)
	}
}

func TestUpstreams_DeleteResetsToDefault(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	loginAs(t, f, "alice", "correct-password-12chars")
	_, _ = f.Tenants.Create(context.Background(), "demo", tenants.VisibilityPrivate)
	// First, upsert a custom row.
	r := f.PostFormFrom(t, "/console/tenants/demo/upstreams", "/console/tenants/demo/upstreams/pypi", url.Values{
		"mode": {"cache_only"},
	})
	_ = r.Body.Close()

	// Now delete it.
	resp := f.PostFormFrom(t, "/console/tenants/demo/upstreams", "/console/tenants/demo/upstreams/pypi/delete", url.Values{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 303, got %d body=%s", resp.StatusCode, body)
	}

	// Row should be back to default.
	_, body := f.Get(t, "/console/tenants/demo/upstreams")
	// Should still show "default" badge for pypi (and no "custom" for it).
	if strings.Count(body, "custom") > 0 {
		// other rows can't have custom because we never set them
		t.Error("expected no 'custom' badge after reset")
	}
}

func TestUpstreams_UpsertWithoutCSRFRejected(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	loginAs(t, f, "alice", "correct-password-12chars")
	_, _ = f.Tenants.Create(context.Background(), "demo", tenants.VisibilityPrivate)
	resp := f.PostFormNoCSRF(t, "/console/tenants/demo/upstreams/pypi", url.Values{
		"mode": {"cache_and_serve"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 without CSRF, got %d", resp.StatusCode)
	}
}

// ---- test helpers local to this file ----

// loginAs is a thin wrapper around the login form POST. Asserts a 303.
func loginAs(t *testing.T, f *authFixture, name, password string) {
	t.Helper()
	r := f.PostForm(t, "/console/login", url.Values{
		"username": {name},
		"password": {password},
	})
	_, _ = io.Copy(io.Discard, r.Body)
	_ = r.Body.Close()
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("login as %q: status=%d (want 303)", name, r.StatusCode)
	}
}
