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
	pkg, err := f.Models.GetOrCreatePackage(context.Background(), tenant.ID, models.Type("npm"), "left-pad")
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

func TestQuarantine_PromoteFlow(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")
	tenant, _ := f.Tenants.Create(context.Background(), "team", tenants.VisibilityPrivate)
	pkg, _ := f.Models.GetOrCreatePackage(context.Background(), tenant.ID, models.Type("npm"), "evilpkg")
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
