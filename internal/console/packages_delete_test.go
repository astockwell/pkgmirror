package console_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/astockwell/pkgmirror/internal/models"
	"github.com/astockwell/pkgmirror/internal/tenants"
	"github.com/astockwell/pkgmirror/internal/users"
)

// ============================================================
// Delete package + delete version (system admin only)
// ============================================================

// makeTestPackage seeds a package with N versions in `tenantName`,
// returning the package + version IDs in insertion order.
func makeTestPackage(t *testing.T, f *authFixture, tenantName, pkgType, pkgName string, versionStrs ...string) (*models.Package, []*models.Version) {
	t.Helper()
	tenant, err := f.Tenants.GetByName(context.Background(), tenantName)
	if err != nil {
		tenant, err = f.Tenants.Create(context.Background(), tenantName, tenants.VisibilityPrivate)
		if err != nil {
			t.Fatalf("create tenant: %v", err)
		}
	}
	pkg, err := f.Models.GetOrCreatePackage(context.Background(), tenant.ID, models.Type(pkgType), pkgName, models.CreatedViaUploaded)
	if err != nil {
		t.Fatalf("create package: %v", err)
	}
	vers := make([]*models.Version, 0, len(versionStrs))
	for _, vs := range versionStrs {
		v, err := f.Models.CreateVersion(context.Background(), pkg.ID, vs, "{}")
		if err != nil {
			t.Fatalf("create version %s: %v", vs, err)
		}
		vers = append(vers, v)
	}
	return pkg, vers
}

func TestPackageDelete_AdminCanDelete(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	pkg, _ := makeTestPackage(t, f, "team", "npm", "left-pad", "1.0.0", "1.1.0")
	f.loginAs(t, "alice", "correct-password-12chars")

	resp := f.PostFormFrom(t,
		"/console/tenants/team/packages/npm/left-pad",
		"/console/packages/"+itoa(pkg.ID)+"/delete",
		url.Values{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 303, got %d body=%s", resp.StatusCode, body)
	}
	if loc := resp.Header.Get("Location"); loc != "/console/tenants/team/packages" {
		t.Errorf("redirect = %q, want /console/tenants/team/packages", loc)
	}

	// Package is gone.
	if _, err := f.Models.GetPackage(context.Background(), pkg.TenantID, pkg.Type, pkg.Name); err == nil {
		t.Error("expected package to be deleted, but GetPackage succeeded")
	}
	// Versions are gone (cascade).
	vs, _ := f.Models.ListVersions(context.Background(), pkg.ID)
	if len(vs) != 0 {
		t.Errorf("expected 0 versions after delete, got %d", len(vs))
	}

	// Audit row written.
	rows := auditActions(t, f, "tenants.package.delete")
	if len(rows) != 1 {
		t.Errorf("expected 1 tenants.package.delete row, got %d", len(rows))
	}
}

func TestPackageDelete_NonAdminGets403(t *testing.T) {
	f := newAuthFixture(t)
	// bob is a regular user; not a system admin.
	u, _ := f.Users.Create(context.Background(), users.CreateOptions{Name: "bob"})
	hash, _ := users.HashPassword("correct-password-12chars")
	_ = f.Users.SetPasswordHash(context.Background(), u.ID, hash)
	pkg, _ := makeTestPackage(t, f, "team", "npm", "left-pad", "1.0.0")
	f.loginAs(t, "bob", "correct-password-12chars")

	// Prime via /console/profile which any signed-in user can GET.
	// /console/login would 303-redirect for a logged-in user.
	resp := f.PostFormFrom(t,
		"/console/profile",
		"/console/packages/"+itoa(pkg.ID)+"/delete",
		url.Values{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403, got %d", resp.StatusCode)
	}
	// Package must still exist.
	if _, err := f.Models.GetPackage(context.Background(), tenantID(t, f, "team"), models.Type("npm"), "left-pad"); err != nil {
		t.Errorf("expected package to still exist after non-admin attempt; got err: %v", err)
	}
}

func TestPackageDelete_AnonRedirects(t *testing.T) {
	f := newAuthFixture(t)
	pkg, _ := makeTestPackage(t, f, "team", "npm", "left-pad", "1.0.0")
	// No login - POST attempt without CSRF first to confirm it gets
	// gated before any business logic. Without CSRF token gorilla/csrf
	// returns 403; that's the auth-chain order we want to pin.
	resp := f.PostFormNoCSRF(t, "/console/packages/"+itoa(pkg.ID)+"/delete", url.Values{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 (CSRF), got %d", resp.StatusCode)
	}
}

func TestPackageDelete_NoCSRFReturns403(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	pkg, _ := makeTestPackage(t, f, "team", "npm", "left-pad", "1.0.0")
	f.loginAs(t, "alice", "correct-password-12chars")

	resp := f.PostFormNoCSRF(t, "/console/packages/"+itoa(pkg.ID)+"/delete", url.Values{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 without CSRF, got %d", resp.StatusCode)
	}
}

func TestPackageDelete_MissingPackage404(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	_, _ = f.Tenants.Create(context.Background(), "team", tenants.VisibilityPrivate)
	f.loginAs(t, "alice", "correct-password-12chars")
	// 999999 is a package_id that doesn't exist.
	resp := f.PostFormFrom(t,
		"/console/tenants/team/packages",
		"/console/packages/999999/delete",
		url.Values{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestVersionDelete_AdminCanDelete(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	pkg, vers := makeTestPackage(t, f, "team", "npm", "left-pad", "1.0.0", "1.1.0", "1.2.0")
	target := vers[1] // 1.1.0
	f.loginAs(t, "alice", "correct-password-12chars")

	resp := f.PostFormFrom(t,
		"/console/tenants/team/packages/npm/left-pad",
		"/console/packages/"+itoa(pkg.ID)+"/versions/"+itoa(target.ID)+"/delete",
		url.Values{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 303, got %d body=%s", resp.StatusCode, body)
	}
	want := "/console/tenants/team/packages/npm/left-pad"
	if loc := resp.Header.Get("Location"); loc != want {
		t.Errorf("redirect = %q, want %q", loc, want)
	}

	// Target version gone; others remain.
	left, _ := f.Models.ListVersions(context.Background(), pkg.ID)
	if len(left) != 2 {
		t.Fatalf("expected 2 surviving versions, got %d", len(left))
	}
	for _, v := range left {
		if v.ID == target.ID {
			t.Errorf("target version %d still present after delete", target.ID)
		}
	}

	rows := auditActions(t, f, "tenants.package.version.delete")
	if len(rows) != 1 {
		t.Errorf("expected 1 audit row, got %d", len(rows))
	}
}

func TestVersionDelete_CrossPackage404(t *testing.T) {
	// Belt-and-braces: a version from package A must not be deletable
	// via package B's URL even when the admin owns both packages.
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	_, versA := makeTestPackage(t, f, "team", "npm", "pkg-a", "1.0.0")
	pkgB, _ := makeTestPackage(t, f, "team", "npm", "pkg-b", "2.0.0")
	f.loginAs(t, "alice", "correct-password-12chars")

	// Try to delete pkg-a's version via pkg-b's path.
	resp := f.PostFormFrom(t,
		"/console/tenants/team/packages/npm/pkg-b",
		"/console/packages/"+itoa(pkgB.ID)+"/versions/"+itoa(versA[0].ID)+"/delete",
		url.Values{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for cross-package version delete, got %d", resp.StatusCode)
	}
}

func TestVersionDelete_NonAdminGets403(t *testing.T) {
	f := newAuthFixture(t)
	u, _ := f.Users.Create(context.Background(), users.CreateOptions{Name: "bob"})
	hash, _ := users.HashPassword("correct-password-12chars")
	_ = f.Users.SetPasswordHash(context.Background(), u.ID, hash)
	pkg, vers := makeTestPackage(t, f, "team", "npm", "left-pad", "1.0.0")
	f.loginAs(t, "bob", "correct-password-12chars")

	resp := f.PostFormFrom(t,
		"/console/profile",
		"/console/packages/"+itoa(pkg.ID)+"/versions/"+itoa(vers[0].ID)+"/delete",
		url.Values{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403, got %d", resp.StatusCode)
	}
}

func TestPackageDetail_DeleteButtonAdminOnly(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	makeTestPackage(t, f, "team", "npm", "left-pad", "1.0.0")

	// Admin -> sees delete buttons.
	f.loginAs(t, "alice", "correct-password-12chars")
	_, body := f.Get(t, "/console/tenants/team/packages/npm/left-pad")
	if !strings.Contains(body, "Delete entire package") {
		t.Error("admin should see 'Delete entire package' button")
	}
	if !strings.Contains(body, "/versions/") || !strings.Contains(body, "/delete") {
		t.Error("admin should see per-version delete buttons")
	}

	// Non-admin -> NO delete buttons. Create a tenant where bob has
	// read access (public tenant) so bob doesn't 403 the page.
	u, _ := f.Users.Create(context.Background(), users.CreateOptions{Name: "bob"})
	hash, _ := users.HashPassword("correct-password-12chars")
	_ = f.Users.SetPasswordHash(context.Background(), u.ID, hash)
	_, _ = f.Tenants.Create(context.Background(), "public-team", tenants.VisibilityPublic)
	makeTestPackage(t, f, "public-team", "npm", "left-pad", "1.0.0")

	// Switch session by wiping the cookie jar and logging in as bob.
	f.Client.Jar, _ = cookiejar.New(nil)
	f.loginAs(t, "bob", "correct-password-12chars")
	_, body2 := f.Get(t, "/console/tenants/public-team/packages/npm/left-pad")
	if strings.Contains(body2, "Delete entire package") {
		t.Errorf("non-admin should NOT see delete button; body=%s", body2)
	}
}

// ---- helpers local to this file ----

// auditActions returns every audit row with the given action. The audit
// logger is async (channel -> drainer goroutine), so we poll for up to
// 2s rather than racing the drain. A test that legitimately expected
// zero rows should still pass because it queries within the budget
// once and accepts the empty result.
func auditActions(t *testing.T, f *authFixture, action string) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var out []string
	for {
		r, err := f.DB.QueryContext(context.Background(),
			`SELECT action FROM audit_log WHERE action = ?`, action)
		if err != nil {
			t.Fatalf("query audit: %v", err)
		}
		out = out[:0]
		for r.Next() {
			var a string
			_ = r.Scan(&a)
			out = append(out, a)
		}
		_ = r.Close()
		if len(out) > 0 || time.Now().After(deadline) {
			return out
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func tenantID(t *testing.T, f *authFixture, name string) int64 {
	t.Helper()
	tn, err := f.Tenants.GetByName(context.Background(), name)
	if err != nil {
		t.Fatalf("get tenant %q: %v", name, err)
	}
	return tn.ID
}
