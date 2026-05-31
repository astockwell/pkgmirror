// Tests for the admin "change provenance" form added in PR C of
// plans/created-via-package-ownership.md.

package console_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/astockwell/pkgmirror/internal/models"
	"github.com/astockwell/pkgmirror/internal/users"
)

// TestPackageSetProvenance_AdminFlipsPullThroughToUploaded covers
// the happy path: admin flips a pull_through package to uploaded,
// the DB reflects the new value, the audit log captures the change,
// and the user is redirected back to the package detail page with
// a success flash.
func TestPackageSetProvenance_AdminFlipsPullThroughToUploaded(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")

	// Seed a package + mutate its provenance to pull_through (the
	// makeTestPackage helper defaults to uploaded; we want to flip
	// it away from the default to prove the form actually moves it).
	pkg, _ := makeTestPackage(t, f, "team", "npm", "left-pad", "1.0.0")
	if err := f.Models.SetPackageCreatedVia(context.Background(),
		pkg.ID, models.CreatedViaPullThrough); err != nil {
		t.Fatalf("seed pull_through: %v", err)
	}

	f.loginAs(t, "alice", "correct-password-12chars")

	resp := f.PostFormFrom(t,
		"/console/tenants/team/packages/npm/left-pad",
		"/console/tenants/team/packages/npm/left-pad/provenance",
		url.Values{"created_via": {"uploaded"}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 303, got %d body=%s", resp.StatusCode, body)
	}
	if loc := resp.Header.Get("Location"); loc != "/console/tenants/team/packages/npm/left-pad" {
		t.Errorf("redirect = %q, want detail page", loc)
	}

	// DB reflects the new value.
	got, err := f.Models.GetPackage(context.Background(),
		tenantID(t, f, "team"), models.Type("npm"), "left-pad")
	if err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
	if got.CreatedVia != models.CreatedViaUploaded {
		t.Errorf("CreatedVia = %q, want uploaded", got.CreatedVia)
	}

	// Audit row written with the old + new values.
	rows := auditActions(t, f, "tenants.package.set_provenance")
	if len(rows) != 1 {
		t.Errorf("expected 1 set_provenance audit row, got %d", len(rows))
	}
}

// TestPackageSetProvenance_RejectsInvalidValue protects against an
// admin (or a malicious POST) setting created_via to something
// unrecognized. The handler validates against models.CreatedVia.Valid().
func TestPackageSetProvenance_RejectsInvalidValue(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	pkg, _ := makeTestPackage(t, f, "team", "npm", "left-pad", "1.0.0")
	f.loginAs(t, "alice", "correct-password-12chars")

	resp := f.PostFormFrom(t,
		"/console/tenants/team/packages/npm/left-pad",
		"/console/tenants/team/packages/npm/left-pad/provenance",
		url.Values{"created_via": {"bogus_value"}})
	defer resp.Body.Close()
	// RenderError returns a 5xx error page; the important assertion
	// is that the DB did NOT change.
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatalf("expected non-redirect on invalid value; got 303")
	}

	got, err := f.Models.GetPackage(context.Background(),
		tenantID(t, f, "team"), models.Type("npm"), "left-pad")
	if err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
	if got.CreatedVia != pkg.CreatedVia {
		t.Errorf("CreatedVia mutated to %q despite invalid input", got.CreatedVia)
	}

	// And no audit row was written.
	if rows := auditActions(t, f, "tenants.package.set_provenance"); len(rows) != 0 {
		t.Errorf("expected 0 audit rows on invalid value, got %d", len(rows))
	}
}

// TestPackageSetProvenance_NonAdminGets403 confirms the route is
// gated by the system-admin middleware.
func TestPackageSetProvenance_NonAdminGets403(t *testing.T) {
	f := newAuthFixture(t)
	// bob is a regular user, not a system admin.
	ctx := context.Background()
	u, err := f.Users.Create(ctx, users.CreateOptions{Name: "bob"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	hash, _ := users.HashPassword("correct-password-12chars")
	if err := f.Users.SetPasswordHash(ctx, u.ID, hash); err != nil {
		t.Fatalf("set password: %v", err)
	}
	makeTestPackage(t, f, "team", "npm", "left-pad", "1.0.0")
	f.loginAs(t, "bob", "correct-password-12chars")

	resp := f.PostFormFrom(t,
		"/console/profile",
		"/console/tenants/team/packages/npm/left-pad/provenance",
		url.Values{"created_via": {"pull_through"}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403, got %d", resp.StatusCode)
	}
}

// TestPackageDetail_ShowsProvenanceBadge is a render-side check:
// the package detail page surfaces the provenance label so admins
// know which mode the package is in without having to inspect SQL.
func TestPackageDetail_ShowsProvenanceBadge(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")

	pkg, _ := makeTestPackage(t, f, "team", "pypi", "requests", "2.32.0")
	if err := f.Models.SetPackageCreatedVia(context.Background(),
		pkg.ID, models.CreatedViaPullThrough); err != nil {
		t.Fatalf("seed pull_through: %v", err)
	}

	f.loginAs(t, "alice", "correct-password-12chars")

	resp, body := f.Get(t, "/console/tenants/team/packages/pypi/requests")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("detail: status=%d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "mirrored from upstream") {
		t.Errorf("expected provenance badge text in detail body; got %s", body)
	}
	// Admin should see a flip form pointing at the provenance endpoint.
	if !strings.Contains(body, "/packages/pypi/requests/provenance") {
		t.Errorf("expected provenance flip form in admin view; got %s", body)
	}
}
