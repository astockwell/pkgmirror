package console_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/astockwell/pkgmirror/internal/users"
)

// TestPackagesGlobal_AdminSeesAllTenants exercises the sidebar
// /console/packages page. Two tenants, each with a couple of packages;
// an admin should see all four rows plus the tenant column. This
// proves the global handler exists (previously the sidebar link 404'd
// because no route was registered).
func TestPackagesGlobal_AdminSeesAllTenants(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")

	makeTestPackage(t, f, "team-a", "pypi", "requests", "2.32.0")
	makeTestPackage(t, f, "team-a", "pypi", "urllib3", "2.0.0")
	makeTestPackage(t, f, "team-b", "npm", "left-pad", "1.0.0")

	f.loginAs(t, "alice", "correct-password-12chars")

	resp, body := f.Get(t, "/console/packages")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	for _, want := range []string{"team-a", "team-b", "requests", "urllib3", "left-pad", "pypi", "npm"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in global packages body; got %s", want, body)
		}
	}
}

// TestPackagesGlobal_NonMemberSeesEmptyState confirms the cross-
// tenant view honors per-tenant read permissions. A user with no
// memberships sees the empty-state alert, not a list of every
// tenant's packages.
func TestPackagesGlobal_NonMemberSeesEmptyState(t *testing.T) {
	f := newAuthFixture(t)
	// Seed packages in a tenant the user is NOT a member of.
	makeTestPackage(t, f, "private-team", "pypi", "requests", "2.32.0")

	// Create a non-admin, no-membership user (same recipe as
	// TestTenantsList_NonMemberSeesEmptyState).
	ctx := context.Background()
	u, err := f.Users.Create(ctx, users.CreateOptions{Name: "bob"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	hash, _ := users.HashPassword("correct-password-12chars")
	if err := f.Users.SetPasswordHash(ctx, u.ID, hash); err != nil {
		t.Fatalf("set password: %v", err)
	}

	f.loginAs(t, "bob", "correct-password-12chars")

	resp, body := f.Get(t, "/console/packages")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	// Empty-state alert is rendered when no readable tenants have
	// packages. The alert template uses "No packages" as its title.
	if !strings.Contains(body, "No packages") {
		t.Errorf("expected empty-state alert; got %s", body)
	}
	// And the private tenant's contents must not leak.
	if strings.Contains(body, "private-team") || strings.Contains(body, "requests") {
		t.Errorf("non-member should not see private-team's packages; got %s", body)
	}
}
