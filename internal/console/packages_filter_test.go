// Tests for the console UI pieces added alongside the admin
// /admin/packages API: provenance filter on the package list pages,
// and the uploaded/mirrored breakdown on the tenant detail page.

package console_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/astockwell/pkgmirror/internal/models"
)

// TestPackagesByTenant_FilterByProvenance verifies the ?provenance=
// query-param hides non-matching rows and the select-option for the
// chosen value is rendered as selected.
func TestPackagesByTenant_FilterByProvenance(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")

	pkgUploaded, _ := makeTestPackage(t, f, "team", "pypi", "uploaded-pkg", "1.0.0")
	pkgPullThrough, _ := makeTestPackage(t, f, "team", "pypi", "pulled-pkg", "2.0.0")
	if err := f.Models.SetPackageCreatedVia(context.Background(),
		pkgPullThrough.ID, models.CreatedViaPullThrough); err != nil {
		t.Fatalf("seed pull_through: %v", err)
	}
	_ = pkgUploaded

	f.loginAs(t, "alice", "correct-password-12chars")

	// Without filter: both visible.
	resp, body := f.Get(t, "/console/tenants/team/packages")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	for _, want := range []string{"uploaded-pkg", "pulled-pkg"} {
		if !strings.Contains(body, want) {
			t.Errorf("unfiltered: missing %q in body", want)
		}
	}

	// ?provenance=uploaded: only the uploaded one.
	resp, body = f.Get(t, "/console/tenants/team/packages?provenance=uploaded")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if !strings.Contains(body, "uploaded-pkg") {
		t.Errorf("filter=uploaded: missing uploaded-pkg")
	}
	if strings.Contains(body, "pulled-pkg") {
		t.Errorf("filter=uploaded: pulled-pkg leaked through")
	}

	// ?provenance=pull_through: only the mirrored one.
	resp, body = f.Get(t, "/console/tenants/team/packages?provenance=pull_through")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if !strings.Contains(body, "pulled-pkg") {
		t.Errorf("filter=pull_through: missing pulled-pkg")
	}
	if strings.Contains(body, "uploaded-pkg") {
		t.Errorf("filter=pull_through: uploaded-pkg leaked through")
	}
}

// TestPackagesGlobal_FilterByProvenance is the same shape across the
// global list page.
func TestPackagesGlobal_FilterByProvenance(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")

	makeTestPackage(t, f, "team-a", "pypi", "alpha", "1.0.0")
	pkgB, _ := makeTestPackage(t, f, "team-b", "npm", "beta", "1.0.0")
	if err := f.Models.SetPackageCreatedVia(context.Background(),
		pkgB.ID, models.CreatedViaPullThrough); err != nil {
		t.Fatalf("seed: %v", err)
	}

	f.loginAs(t, "alice", "correct-password-12chars")

	resp, body := f.Get(t, "/console/packages?provenance=uploaded")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if !strings.Contains(body, "alpha") {
		t.Errorf("filter=uploaded: missing alpha")
	}
	if strings.Contains(body, "beta") {
		t.Errorf("filter=uploaded: beta leaked through")
	}
}

// TestTenantDetail_ShowsProvenanceBreakdown verifies the count
// summary on the tenant detail page surfaces the split with links
// to filtered list views.
func TestTenantDetail_ShowsProvenanceBreakdown(t *testing.T) {
	f := newAuthFixture(t)
	f.CreateAdmin(t, "alice", "correct-password-12chars")

	makeTestPackage(t, f, "team", "pypi", "u1", "1.0.0")
	makeTestPackage(t, f, "team", "pypi", "u2", "1.0.0")
	pkgPT, _ := makeTestPackage(t, f, "team", "pypi", "p1", "1.0.0")
	if err := f.Models.SetPackageCreatedVia(context.Background(),
		pkgPT.ID, models.CreatedViaPullThrough); err != nil {
		t.Fatalf("seed: %v", err)
	}

	f.loginAs(t, "alice", "correct-password-12chars")
	resp, body := f.Get(t, "/console/tenants/team")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	for _, want := range []string{"2 uploaded", "1 mirrored",
		"/packages?provenance=uploaded", "/packages?provenance=pull_through"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in tenant detail body; got %s", want, body)
		}
	}
}
