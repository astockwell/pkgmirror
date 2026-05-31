// Provenance tests for the Go module proxy handler. Two behaviors:
//
//   - Pull-through ingest sets packages.created_via='pull_through'.
//     Without this, the package would default to 'uploaded', which
//     seals it from future upstream merges - the demo flow "disable
//     cooldown, resync, get fresh version" would silently fail
//     because the next /simple/-equivalent request would skip
//     upstream.
//
//   - The non-standard PUT /upload endpoint refuses uploads against
//     pull_through-owned packages with 409. This protects against
//     scripted shadow attacks (the go toolchain itself has no
//     publish verb, so this is a defense against
//     not-the-real-client attacks against pkgmirror's upload API).

package goproxy_test

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/astockwell/pkgmirror/internal/models"
)

// TestPullThroughGo_IngestSetsPullThroughProvenance proves that a
// Go module ingested via pull-through lands with
// CreatedVia=CreatedViaPullThrough (NOT the default 'uploaded' from
// CreationInfo's zero value). Without this fix, every pulled-through
// module would be treated as tenant-owned by the merge logic, and a
// disable-the-cooldown-and-resync flow would never surface fresh
// upstream versions.
func TestPullThroughGo_IngestSetsPullThroughProvenance(t *testing.T) {
	f := newUpstreamFixture(t, 0)
	base := "/api/packages/" + f.tenantName + "/go"

	// Cold pull-through.
	resp, body := f.get(t, base+"/"+f.module+"/@v/"+f.freshVersion+".zip")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("zip: status=%d body=%s", resp.StatusCode, body)
	}

	pkg, err := f.models.GetPackage(context.Background(),
		f.tenantID, models.TypeGo, f.module)
	if err != nil {
		t.Fatalf("get pkg: %v", err)
	}
	if pkg.CreatedVia != models.CreatedViaPullThrough {
		t.Errorf("pull-through ingest produced CreatedVia=%q, want %q",
			pkg.CreatedVia, models.CreatedViaPullThrough)
	}
}

// TestUploadGo_AgainstPullThroughReturns409 proves the upload-side
// gate: once a module is owned by pull-through, uploads to it must
// be refused with a clear 409. The /upload endpoint here is the
// non-standard mirror-population API (see goproxy/handler.go's
// upload handler); the production attack vector is a script with an
// admin token, not `go publish` (which doesn't exist).
func TestUploadGo_AgainstPullThroughReturns409(t *testing.T) {
	f := newUpstreamFixture(t, 0)
	base := "/api/packages/" + f.tenantName + "/go"

	// Seed pull-through ownership by fetching one zip.
	resp, _ := f.get(t, base+"/"+f.module+"/@v/"+f.freshVersion+".zip")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seed-download: status=%d", resp.StatusCode)
	}
	// Sanity: provenance landed correctly (regression guard for the
	// previous test).
	pkg, _ := f.models.GetPackage(context.Background(),
		f.tenantID, models.TypeGo, f.module)
	if pkg.CreatedVia != models.CreatedViaPullThrough {
		t.Fatalf("seed: pkg.CreatedVia=%q, want pull_through", pkg.CreatedVia)
	}

	// Now try to upload a DIFFERENT version of the same module.
	// Build a minimal valid go module zip in-process.
	differentVersion := "v9.9.9"
	zipBody := buildSyntheticModuleZip(t, f.module, differentVersion)

	req, _ := http.NewRequest(http.MethodPut,
		f.pkgmirror.URL+base+"/upload", bytes.NewReader(zipBody))
	req.Header.Set("Authorization", "Bearer "+f.adminToken)
	uploadResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer uploadResp.Body.Close()
	uploadBody, _ := io.ReadAll(uploadResp.Body)

	if uploadResp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 on upload-against-pull_through; got %d body=%s",
			uploadResp.StatusCode, uploadBody)
	}
	// Body should mention the module name + the console URL to flip
	// provenance / delete + the words 'mirrored from upstream'.
	for _, want := range []string{f.module, "mirrored from upstream", "/console/tenants/"} {
		if !strings.Contains(string(uploadBody), want) {
			t.Errorf("expected 409 body to contain %q; got %s", want, uploadBody)
		}
	}

	// Provenance unchanged after the failed upload.
	pkg2, _ := f.models.GetPackage(context.Background(),
		f.tenantID, models.TypeGo, f.module)
	if pkg2.CreatedVia != models.CreatedViaPullThrough {
		t.Errorf("failed upload mutated CreatedVia to %q", pkg2.CreatedVia)
	}
}

// buildSyntheticModuleZip returns a minimal valid Go module zip per
// the layout the goproxy.Parse function expects (every file path
// prefixed with `<module>@<version>/`).
func buildSyntheticModuleZip(t *testing.T, module, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	prefix := fmt.Sprintf("%s@%s/", module, version)
	add := func(name, contents string) {
		f, err := w.Create(prefix + name)
		if err != nil {
			t.Fatalf("zip create %q: %v", name, err)
		}
		if _, err := io.WriteString(f, contents); err != nil {
			t.Fatalf("zip write %q: %v", name, err)
		}
	}
	add("go.mod", "module "+module+"\n\ngo 1.22\n")
	add("README.md", "synthetic upload-test fixture for "+version)
	if err := w.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}
