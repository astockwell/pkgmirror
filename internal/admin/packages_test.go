// Tests for the admin packages endpoints (PR follow-up to the
// packages.created_via plan): GET /admin/packages and
// POST /admin/packages/:id/provenance.

package admin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/astockwell/pkgmirror/internal/models"
)

// seedPackage creates a package row with the requested provenance.
// Returns the package ID for follow-up assertions.
func seedPackage(t *testing.T, f *fixture, typ models.Type, name string, via models.CreatedVia, versions ...string) int64 {
	t.Helper()
	pkg, err := f.models.GetOrCreatePackage(context.Background(),
		f.tenant.ID, typ, name, via)
	if err != nil {
		t.Fatalf("seed package %s/%s: %v", typ, name, err)
	}
	for _, v := range versions {
		if _, err := f.models.CreateVersion(context.Background(), pkg.ID, v, "{}"); err != nil {
			t.Fatalf("seed version %s: %v", v, err)
		}
	}
	return pkg.ID
}

// TestAdminPackages_ListEverything proves the happy path: an admin
// can GET /admin/packages and see every package across every tenant.
func TestAdminPackages_ListEverything(t *testing.T) {
	f := newFixture(t)
	seedPackage(t, f, models.TypePyPI, "alpha", models.CreatedViaUploaded, "1.0.0")
	seedPackage(t, f, models.TypePyPI, "beta", models.CreatedViaPullThrough, "2.0.0", "2.1.0")
	seedPackage(t, f, models.TypeNpm, "gamma", models.CreatedViaUploaded)

	resp, body := f.do(t, http.MethodGet, "/admin/packages", nil, f.adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var payload struct {
		Packages []struct {
			Name       string `json:"name"`
			Type       string `json:"type"`
			CreatedVia string `json:"created_via"`
			Versions   int    `json:"versions"`
		} `json:"packages"`
		Total      int  `json:"total"`
		NextOffset *int `json:"next_offset"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	if payload.Total != 3 {
		t.Errorf("total = %d, want 3", payload.Total)
	}
	if payload.NextOffset != nil {
		t.Errorf("next_offset = %v, want null (single page)", *payload.NextOffset)
	}
	if len(payload.Packages) != 3 {
		t.Fatalf("len(packages) = %d, want 3", len(payload.Packages))
	}

	// Sanity-check the rows are all present + provenance is round-tripped.
	have := map[string]string{}
	for _, p := range payload.Packages {
		have[p.Name] = p.CreatedVia
	}
	for name, wantVia := range map[string]string{
		"alpha": "uploaded",
		"beta":  "pull_through",
		"gamma": "uploaded",
	} {
		if have[name] != wantVia {
			t.Errorf("package %q created_via = %q, want %q", name, have[name], wantVia)
		}
	}
}

// TestAdminPackages_FiltersByProvenance proves the ?provenance=
// query-param filter works against either value.
func TestAdminPackages_FiltersByProvenance(t *testing.T) {
	f := newFixture(t)
	seedPackage(t, f, models.TypePyPI, "alpha", models.CreatedViaUploaded)
	seedPackage(t, f, models.TypePyPI, "beta", models.CreatedViaPullThrough)
	seedPackage(t, f, models.TypePyPI, "gamma", models.CreatedViaPullThrough)

	for via, wantCount := range map[string]int{
		"uploaded":     1,
		"pull_through": 2,
	} {
		t.Run(via, func(t *testing.T) {
			resp, body := f.do(t, http.MethodGet,
				"/admin/packages?provenance="+via, nil, f.adminToken)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			var payload struct {
				Total    int `json:"total"`
				Packages []struct {
					CreatedVia string `json:"created_via"`
				} `json:"packages"`
			}
			_ = json.Unmarshal(body, &payload)
			if payload.Total != wantCount {
				t.Errorf("filter=%s total = %d, want %d", via, payload.Total, wantCount)
			}
			for _, p := range payload.Packages {
				if p.CreatedVia != via {
					t.Errorf("filter=%s leaked a row with created_via=%s", via, p.CreatedVia)
				}
			}
		})
	}
}

// TestAdminPackages_FiltersByFormat proves the ?format= filter works
// and composes with provenance.
func TestAdminPackages_FiltersByFormat(t *testing.T) {
	f := newFixture(t)
	seedPackage(t, f, models.TypePyPI, "py-a", models.CreatedViaUploaded)
	seedPackage(t, f, models.TypePyPI, "py-b", models.CreatedViaPullThrough)
	seedPackage(t, f, models.TypeNpm, "npm-a", models.CreatedViaUploaded)

	resp, body := f.do(t, http.MethodGet,
		"/admin/packages?format=pypi", nil, f.adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("format=pypi: status=%d body=%s", resp.StatusCode, body)
	}
	var payload struct {
		Total    int `json:"total"`
		Packages []struct {
			Type string `json:"type"`
		} `json:"packages"`
	}
	_ = json.Unmarshal(body, &payload)
	if payload.Total != 2 {
		t.Errorf("format=pypi total = %d, want 2", payload.Total)
	}
	for _, p := range payload.Packages {
		if p.Type != "pypi" {
			t.Errorf("format filter leaked %q", p.Type)
		}
	}
}

// TestAdminPackages_Pagination proves limit + offset advance through
// pages and next_offset surfaces correctly.
func TestAdminPackages_Pagination(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 5; i++ {
		seedPackage(t, f, models.TypePyPI,
			fmt.Sprintf("pkg-%d", i), models.CreatedViaUploaded)
	}

	// First page of 2.
	resp, body := f.do(t, http.MethodGet,
		"/admin/packages?limit=2&offset=0", nil, f.adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("page 1: status=%d body=%s", resp.StatusCode, body)
	}
	var page1 struct {
		Total      int  `json:"total"`
		NextOffset *int `json:"next_offset"`
		Packages   []struct {
			ID int64 `json:"id"`
		} `json:"packages"`
	}
	_ = json.Unmarshal(body, &page1)
	if page1.Total != 5 {
		t.Errorf("total = %d, want 5", page1.Total)
	}
	if len(page1.Packages) != 2 {
		t.Errorf("page 1 returned %d packages, want 2", len(page1.Packages))
	}
	if page1.NextOffset == nil || *page1.NextOffset != 2 {
		t.Errorf("page 1 next_offset = %v, want 2", page1.NextOffset)
	}

	// Last page of 1 (5 total - offset 4 = 1 remaining).
	resp, body = f.do(t, http.MethodGet,
		"/admin/packages?limit=2&offset=4", nil, f.adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("page last: status=%d body=%s", resp.StatusCode, body)
	}
	var page3 struct {
		NextOffset *int `json:"next_offset"`
		Packages   []struct {
			ID int64 `json:"id"`
		} `json:"packages"`
	}
	_ = json.Unmarshal(body, &page3)
	if len(page3.Packages) != 1 {
		t.Errorf("last page returned %d, want 1", len(page3.Packages))
	}
	if page3.NextOffset != nil {
		t.Errorf("last page next_offset = %v, want null", *page3.NextOffset)
	}
}

// TestAdminPackages_RequiresAdmin gates the route by system-admin.
func TestAdminPackages_RequiresAdmin(t *testing.T) {
	f := newFixture(t)
	seedPackage(t, f, models.TypePyPI, "alpha", models.CreatedViaUploaded)

	resp, _ := f.do(t, http.MethodGet, "/admin/packages", nil, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("unauthed status = %d, want 403", resp.StatusCode)
	}
}

// TestAdminPackages_SetProvenance happy-path-flips a package's
// provenance via the admin API, verifies the new value sticks, and
// confirms the audit row carries source=admin_api.
func TestAdminPackages_SetProvenance(t *testing.T) {
	f := newFixture(t)
	id := seedPackage(t, f, models.TypePyPI, "alpha", models.CreatedViaUploaded)

	body := []byte(`{"created_via":"pull_through"}`)
	resp, out := f.do(t, http.MethodPost,
		fmt.Sprintf("/admin/packages/%d/provenance", id), body, f.adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	var payload struct {
		ID         int64  `json:"id"`
		CreatedVia string `json:"created_via"`
	}
	_ = json.Unmarshal(out, &payload)
	if payload.CreatedVia != "pull_through" {
		t.Errorf("response created_via = %q, want pull_through", payload.CreatedVia)
	}

	// DB reflects it.
	got, err := f.models.GetPackageByID(context.Background(), id)
	if err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
	if got.CreatedVia != models.CreatedViaPullThrough {
		t.Errorf("DB created_via = %q, want pull_through", got.CreatedVia)
	}

	// Audit row carries source=admin_api - distinguishes from
	// console-driven flips even though the action name is shared.
	// Poll briefly because audit writes are async.
	var found bool
	for i := 0; i < 20; i++ {
		resp, body := f.do(t, http.MethodGet,
			"/admin/audit?action=tenants.package.set_provenance",
			nil, f.adminToken)
		if resp.StatusCode == http.StatusOK {
			var evs struct {
				Events []struct {
					Extra map[string]any `json:"Extra"`
				} `json:"events"`
			}
			if err := json.Unmarshal(body, &evs); err == nil {
				for _, e := range evs.Events {
					if src, _ := e.Extra["source"].(string); src == "admin_api" {
						found = true
						break
					}
				}
			}
		}
		if found {
			break
		}
	}
	if !found {
		t.Errorf("expected audit row with source=admin_api after flip")
	}
}

// TestAdminPackages_SetProvenance_RejectsInvalidValue proves the
// value validation - garbage like "garbage" is rejected with 400
// and the DB is unchanged.
func TestAdminPackages_SetProvenance_RejectsInvalidValue(t *testing.T) {
	f := newFixture(t)
	id := seedPackage(t, f, models.TypePyPI, "alpha", models.CreatedViaUploaded)

	body := []byte(`{"created_via":"bogus"}`)
	resp, out := f.do(t, http.MethodPost,
		fmt.Sprintf("/admin/packages/%d/provenance", id), body, f.adminToken)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d, want 400, body=%s", resp.StatusCode, out)
	}

	got, err := f.models.GetPackageByID(context.Background(), id)
	if err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
	if got.CreatedVia != models.CreatedViaUploaded {
		t.Errorf("DB created_via mutated to %q despite invalid input", got.CreatedVia)
	}
}

// TestAdminPackages_SetProvenance_NotFound returns 404 for a
// nonexistent package id (without surfacing an internal error page).
func TestAdminPackages_SetProvenance_NotFound(t *testing.T) {
	f := newFixture(t)

	body := []byte(`{"created_via":"uploaded"}`)
	resp, _ := f.do(t, http.MethodPost,
		"/admin/packages/9999999/provenance", body, f.adminToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status=%d, want 404", resp.StatusCode)
	}
}
