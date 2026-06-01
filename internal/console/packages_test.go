package console_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/astockwell/pkgmirror/internal/models"
	"github.com/astockwell/pkgmirror/internal/tenants"
	"github.com/astockwell/pkgmirror/internal/users"
)

func TestPackagesByTenant_EmptyState(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	_, _ = f.Tenants.Create(context.Background(), "empty", tenants.VisibilityPrivate)
	f.loginAs(t, "alice", "correct-password-12chars")

	resp, body := f.Get(t, "/console/tenants/empty/packages")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if !strings.Contains(body, "No packages") {
		t.Errorf("expected empty-state alert; got %s", body)
	}
}

func TestPackagesByTenant_403ForNonMember(t *testing.T) {
	f := newAuthFixture(t)
	u, _ := f.Users.Create(context.Background(), users.CreateOptions{Name: "bob"})
	hash, _ := users.HashPassword("correct-password-12chars")
	_ = f.Users.SetPasswordHash(context.Background(), u.ID, hash)
	_, _ = f.Tenants.Create(context.Background(), "secret", tenants.VisibilityPrivate)
	f.loginAs(t, "bob", "correct-password-12chars")

	resp, _ := f.Get(t, "/console/tenants/secret/packages")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403, got %d", resp.StatusCode)
	}
}

func TestPackageDetail_ShowsVersions(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	tenant, _ := f.Tenants.Create(context.Background(), "team", tenants.VisibilityPrivate)
	pkg, err := f.Models.GetOrCreatePackage(context.Background(), tenant.ID, models.Type("npm"), "left-pad", models.CreatedViaUploaded)
	if err != nil {
		t.Fatalf("create package: %v", err)
	}
	_, _ = f.Models.CreateVersion(context.Background(), pkg.ID, "1.0.0", "{}")
	_, _ = f.Models.CreateVersion(context.Background(), pkg.ID, "1.1.0", "{}")
	f.loginAs(t, "alice", "correct-password-12chars")

	_, body := f.Get(t, "/console/tenants/team/packages/npm/left-pad")
	for _, want := range []string{"left-pad", "1.0.0", "1.1.0", "live"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in body", want)
		}
	}
}

// TestPackageDetail_GoModuleWithSlashInName covers the bug where the
// package detail show page returned 404 for Go module paths like
// "rsc.io/quote" because Gin's `:pkgname` only matches a single path
// segment. The fix uses a *pkgname catch-all on the GET route so
// arbitrary slash-containing module paths survive routing. Reading
// gc.Param("pkgname") returns "/rsc.io/quote" (with leading slash);
// the handler strips it before the lookup.
func TestPackageDetail_GoModuleWithSlashInName(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	// The schema migration auto-seeds a "default" tenant, so fetch
	// it rather than trying (and failing) to create another.
	tenant, err := f.Tenants.GetByName(context.Background(), "default")
	if err != nil {
		t.Fatalf("get default tenant: %v", err)
	}
	pkg, err := f.Models.GetOrCreatePackage(context.Background(),
		tenant.ID, models.Type("go"), "rsc.io/quote", models.CreatedViaPullThrough)
	if err != nil {
		t.Fatalf("create package: %v", err)
	}
	if _, err := f.Models.CreateVersion(context.Background(), pkg.ID, "v1.5.2", "{}"); err != nil {
		t.Fatalf("create version: %v", err)
	}
	f.loginAs(t, "alice", "correct-password-12chars")

	// Plain URL with literal slashes in the module path - this is what
	// browsers will resolve when a user clicks a link generated from
	// the packages list. Pre-fix this returned 404.
	resp, body := f.Get(t, "/console/tenants/default/packages/go/rsc.io/quote")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for go/rsc.io/quote, got %d body=%s", resp.StatusCode, body)
	}
	for _, want := range []string{"rsc.io/quote", "v1.5.2"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in body", want)
		}
	}

	// Defense-in-depth: percent-encoded form (%2F) should also resolve
	// - some clients (including Forgejo-style links) encode the slash.
	// Gin normalizes the path before matching, so this exercises the
	// same catch-all branch via a different input.
	resp2, body2 := f.Get(t, "/console/tenants/default/packages/go/rsc.io%2Fquote")
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for go/rsc.io%%2Fquote, got %d body=%s", resp2.StatusCode, body2)
	}
}

func TestQuarantine_PromoteFlow(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	tenant, _ := f.Tenants.Create(context.Background(), "team", tenants.VisibilityPrivate)
	pkg, _ := f.Models.GetOrCreatePackage(context.Background(), tenant.ID, models.Type("npm"), "evilpkg", models.CreatedViaUploaded)
	ver, _ := f.Models.CreateVersion(context.Background(), pkg.ID, "0.1.0", "{}")
	// Quarantine it.
	if err := f.Models.QuarantineVersion(context.Background(), ver.ID, 0, "test-policy"); err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	f.loginAs(t, "alice", "correct-password-12chars")

	// Verify it shows up.
	_, body := f.Get(t, "/console/quarantine")
	if !strings.Contains(body, "evilpkg") {
		t.Errorf("quarantine list missing evilpkg; body=%s", body)
	}

	// Promote it.
	promotePath := "/console/quarantine/" + itoa(ver.ID) + "/promote"
	resp := f.PostFormFrom(t, "/console/quarantine", promotePath, url.Values{})
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("promote: expected 303, got %d", resp.StatusCode)
	}

	// Verify it's no longer quarantined.
	got, _ := f.Models.GetVersion(context.Background(), pkg.ID, "0.1.0")
	if got.IsQuarantined() {
		t.Error("expected version to be promoted out of quarantine")
	}
}

func TestQuarantine_NonAdminGet403(t *testing.T) {
	f := newAuthFixture(t)
	u, _ := f.Users.Create(context.Background(), users.CreateOptions{Name: "bob"})
	hash, _ := users.HashPassword("correct-password-12chars")
	_ = f.Users.SetPasswordHash(context.Background(), u.ID, hash)
	f.loginAs(t, "bob", "correct-password-12chars")

	resp, _ := f.Get(t, "/console/quarantine")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403, got %d", resp.StatusCode)
	}
}
