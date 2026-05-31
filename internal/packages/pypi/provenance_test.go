// Tests for the provenance gate added in PR B of
// plans/created-via-package-ownership.md. Two behaviors are covered:
//
//   - The /simple/ merge is GATED on packages.created_via. Tenant-
//     uploaded packages don't merge upstream (typosquat defense).
//   - An upload to a package that was first ingested via pull-through
//     returns 409 with an actionable error body.

package pypi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/astockwell/pkgmirror/assets"
	"github.com/astockwell/pkgmirror/internal/auth"
	"github.com/astockwell/pkgmirror/internal/bootstrap"
	pkgdb "github.com/astockwell/pkgmirror/internal/db"
	"github.com/astockwell/pkgmirror/internal/models"
	pkgsvc "github.com/astockwell/pkgmirror/internal/packages"
	"github.com/astockwell/pkgmirror/internal/server"
	"github.com/astockwell/pkgmirror/internal/storage"
	"github.com/astockwell/pkgmirror/internal/tenants"
	"github.com/astockwell/pkgmirror/internal/tokens"
	"github.com/astockwell/pkgmirror/internal/upstream"
	"github.com/astockwell/pkgmirror/internal/users"

	"github.com/gin-gonic/gin"
)

// provenanceFixture spins up a minimal pkgmirror + httptest upstream
// serving one well-known `widgets` package, like the upstream fixture
// in upstream_test.go but pared down to what the provenance tests
// need (no policy engine wired - we want to exercise the merge
// gate directly).
type provenanceFixture struct {
	pkgmirror   *httptest.Server
	fakePypi    *httptest.Server
	adminToken  string
	tenantName  string
	tenantID    int64
	models      *models.Store
	upstreamHit *int32
}

func newProvenanceFixture(t *testing.T) *provenanceFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	db, err := pkgdb.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	pkgModels := models.New(db)
	tenStore := tenants.New(db)
	userStore := users.New(db)
	tokenStore := tokens.New(db)
	res, err := bootstrap.Ensure(context.Background(), tenStore, userStore, tokenStore, bootstrap.Options{
		DefaultTenantName:       "default",
		DefaultTenantVisibility: tenants.VisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// One stable upstream "widgets" wheel.
	wheelBody := []byte("PK\x03\x04 fake widgets wheel - the bytes don't matter")
	sum := sha256.Sum256(wheelBody)
	wheelSha := hex.EncodeToString(sum[:])
	var upstreamHits int32

	realMux := http.NewServeMux()
	realMux.HandleFunc("/simple/widgets/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// HTML form so we don't need content-negotiation.
		// nb: the URL host will be substituted before the handler is
		// installed (see fakePypi.Config.Handler swap below).
		_, _ = io.WriteString(w, `<!DOCTYPE html>
<html><body>
<a href="BASEURL/packages/widgets-9.9.9-py3-none-any.whl#sha256=`+wheelSha+`">widgets-9.9.9-py3-none-any.whl</a>
</body></html>`)
	})
	realMux.HandleFunc("/packages/widgets-9.9.9-py3-none-any.whl", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(wheelBody)
	})
	fakePypi := httptest.NewServer(realMux)
	t.Cleanup(fakePypi.Close)
	// Reinstall the mux with BASEURL substituted now that we know it.
	finalMux := http.NewServeMux()
	finalMux.HandleFunc("/simple/widgets/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!DOCTYPE html>
<html><body>
<a href="`+fakePypi.URL+`/packages/widgets-9.9.9-py3-none-any.whl#sha256=`+wheelSha+`">widgets-9.9.9-py3-none-any.whl</a>
</body></html>`)
	})
	finalMux.HandleFunc("/packages/widgets-9.9.9-py3-none-any.whl", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(wheelBody)
	})
	fakePypi.Config.Handler = finalMux

	fakeURL, _ := url.Parse(fakePypi.URL)
	cfg := upstream.DefaultConfig()
	cfg.AllowedHostsExtra = []string{fakeURL.Hostname()}
	cfg.AllowPrivateIPs = true
	cfg.AllowPlaintext = true
	store := stubUpstreamStore{
		rows: map[string]*upstream.PersistedConfig{
			fmt.Sprintf("%d|pypi", res.DefaultTenant.ID): {
				Mode:        string(upstream.ModeCacheAndServe),
				UpstreamURL: sql.NullString{Valid: true, String: fakePypi.URL},
			},
		},
	}
	fetcher := upstream.New(cfg, store)

	blobs, _ := storage.NewLocalStorage(context.Background(), filepath.Join(dir, "blobs"))
	svc := pkgsvc.NewService(pkgModels, blobs)
	authn := &auth.TokenAuthenticator{Tokens: tokenStore, Users: userStore, Tenants: tenStore}
	router, err := server.New(server.Deps{
		Service:       svc,
		Models:        pkgModels,
		Tenants:       tenStore,
		Authenticator: authn,
		Templates:     assets.Templates(),
		Upstream:      fetcher,
	})
	if err != nil {
		t.Fatalf("build server: %v", err)
	}
	pkgmirror := httptest.NewServer(router)
	t.Cleanup(pkgmirror.Close)

	return &provenanceFixture{
		pkgmirror:   pkgmirror,
		fakePypi:    fakePypi,
		adminToken:  res.GeneratedAdminToken,
		tenantName:  res.DefaultTenant.Name,
		tenantID:    res.DefaultTenant.ID,
		models:      pkgModels,
		upstreamHit: &upstreamHits,
	}
}

func (f *provenanceFixture) get(t *testing.T, path string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, f.pkgmirror.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+f.adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}

// uploadWidget performs a twine-style POST to the PyPI upload
// endpoint. Returns the HTTP status + response body so tests can
// assert on both.
func (f *provenanceFixture) uploadWidget(t *testing.T, version string) (int, string) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range map[string]string{
		"name":             "widgets",
		"version":          version,
		"filetype":         "bdist_wheel",
		"pyversion":        "py3",
		"metadata_version": "2.1",
	} {
		_ = w.WriteField(k, v)
	}
	fw, _ := w.CreateFormFile("content", "widgets-"+version+"-py3-none-any.whl")
	_, _ = fw.Write([]byte("fake-local-wheel-bytes for " + version))
	_ = w.Close()

	req, _ := http.NewRequest(http.MethodPost,
		f.pkgmirror.URL+"/api/packages/"+f.tenantName+"/pypi/", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+f.adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST upload: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// --- Tests ---

// TestProvenance_UploadedPackageDoesNotMerge proves the typosquat
// defense: a package that the tenant uploaded is sealed from
// upstream merge, even when the same name exists upstream.
func TestProvenance_UploadedPackageDoesNotMerge(t *testing.T) {
	f := newProvenanceFixture(t)
	base := "/api/packages/" + f.tenantName + "/pypi"

	// Tenant uploads widgets v1.0.0 first.
	code, body := f.uploadWidget(t, "1.0.0")
	if code != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", code, body)
	}

	// Sanity: provenance is 'uploaded'.
	pkg, err := f.models.GetPackageByLookup(context.Background(),
		f.tenantID, models.TypePyPI, "widgets")
	if err != nil {
		t.Fatalf("get pkg: %v", err)
	}
	if pkg.CreatedVia != models.CreatedViaUploaded {
		t.Fatalf("upload produced CreatedVia=%q, want %q",
			pkg.CreatedVia, models.CreatedViaUploaded)
	}

	// Now request /simple/widgets/. Even though upstream advertises
	// widgets v9.9.9, the response MUST NOT include it - this package
	// is tenant-owned.
	resp, indexBody := f.get(t, base+"/simple/widgets/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/simple/: status=%d body=%s", resp.StatusCode, indexBody)
	}
	if !strings.Contains(string(indexBody), "widgets-1.0.0") {
		t.Errorf("expected local 1.0.0 in /simple/; got %s", indexBody)
	}
	if strings.Contains(string(indexBody), "9.9.9") {
		t.Errorf("upstream 9.9.9 leaked into /simple/ for uploaded package; got %s",
			indexBody)
	}

	// AND upstream was never contacted - that's the round-trip-cost win.
	if got := atomic.LoadInt32(f.upstreamHit); got != 0 {
		t.Errorf("expected 0 upstream hits for uploaded package; got %d", got)
	}
}

// TestProvenance_PullThroughPackageStillMerges is the regression test
// for the existing merge behavior - it must still work for packages
// pkgmirror originally ingested via pull-through.
func TestProvenance_PullThroughPackageStillMerges(t *testing.T) {
	f := newProvenanceFixture(t)
	base := "/api/packages/" + f.tenantName + "/pypi"

	// Cold pull-through: first /simple/ request triggers the upstream
	// fetch, which records the package row with CreatedViaPullThrough.
	// We follow that by a download to actually persist a local file
	// (so the second /simple/ takes the merge path, not the cold path).
	if _, _ = f.get(t, base+"/simple/widgets/"); true {
		// ignore body
	}
	resp, _ := f.get(t, base+"/files/widgets/9.9.9/widgets-9.9.9-py3-none-any.whl")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seed-download: status=%d", resp.StatusCode)
	}

	// Sanity: provenance is 'pull_through'.
	pkg, err := f.models.GetPackageByLookup(context.Background(),
		f.tenantID, models.TypePyPI, "widgets")
	if err != nil {
		t.Fatalf("get pkg: %v", err)
	}
	if pkg.CreatedVia != models.CreatedViaPullThrough {
		t.Fatalf("pull-through ingest produced CreatedVia=%q, want %q",
			pkg.CreatedVia, models.CreatedViaPullThrough)
	}

	// And /simple/ shows the local-persisted version (merge gate
	// allows the upstream contact too, but there's nothing new
	// upstream since we already pulled the only advertised version).
	resp, indexBody := f.get(t, base+"/simple/widgets/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/simple/: status=%d body=%s", resp.StatusCode, indexBody)
	}
	if !strings.Contains(string(indexBody), "9.9.9") {
		t.Errorf("expected widgets 9.9.9 in /simple/; got %s", indexBody)
	}
}

// TestProvenance_UploadAgainstPullThroughReturns409 proves the
// insider-shadow defense: once a package is owned by pull-through,
// uploads to it must be refused with a clear 409 + actionable body.
func TestProvenance_UploadAgainstPullThroughReturns409(t *testing.T) {
	f := newProvenanceFixture(t)
	base := "/api/packages/" + f.tenantName + "/pypi"

	// Seed pull-through ownership.
	resp, _ := f.get(t, base+"/files/widgets/9.9.9/widgets-9.9.9-py3-none-any.whl")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seed-download: status=%d", resp.StatusCode)
	}

	// Try to upload a different version of the same package.
	code, body := f.uploadWidget(t, "10.0.0")
	if code != http.StatusConflict {
		t.Fatalf("expected 409 on upload-against-pull_through; got %d body=%s",
			code, body)
	}
	// Body should mention the package name and the console URL the
	// admin needs to visit.
	for _, want := range []string{"widgets", "mirrored from upstream", "/console/tenants/"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected 409 body to contain %q; got %s", want, body)
		}
	}

	// Provenance is unchanged - the row wasn't mutated by the failed upload.
	pkg, err := f.models.GetPackageByLookup(context.Background(),
		f.tenantID, models.TypePyPI, "widgets")
	if err != nil {
		t.Fatalf("get pkg: %v", err)
	}
	if pkg.CreatedVia != models.CreatedViaPullThrough {
		t.Errorf("failed upload mutated CreatedVia to %q", pkg.CreatedVia)
	}
}

// TestProvenance_UploadAgainstUploadedSucceeds is the regression test:
// when the existing row is 'uploaded', another upload (different
// version) of the same name must succeed - the provenance check only
// fires against pull_through rows.
func TestProvenance_UploadAgainstUploadedSucceeds(t *testing.T) {
	f := newProvenanceFixture(t)

	code, body := f.uploadWidget(t, "1.0.0")
	if code != http.StatusCreated {
		t.Fatalf("first upload: status=%d body=%s", code, body)
	}
	code, body = f.uploadWidget(t, "1.1.0")
	if code != http.StatusCreated {
		t.Fatalf("second upload to same name: status=%d body=%s", code, body)
	}
}

// TestProvenance_AdminFlipChangesBehavior exercises the admin
// SetPackageCreatedVia store method directly (PR C wires it to the
// console UI). Confirms that flipping a 'pull_through' row to
// 'uploaded' immediately makes the merge stop contacting upstream.
func TestProvenance_AdminFlipChangesBehavior(t *testing.T) {
	f := newProvenanceFixture(t)
	base := "/api/packages/" + f.tenantName + "/pypi"

	// Seed pull-through ownership.
	resp, _ := f.get(t, base+"/files/widgets/9.9.9/widgets-9.9.9-py3-none-any.whl")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seed-download: status=%d", resp.StatusCode)
	}

	// Baseline hit count after the seed.
	beforeFlip := atomic.LoadInt32(f.upstreamHit)

	pkg, err := f.models.GetPackageByLookup(context.Background(),
		f.tenantID, models.TypePyPI, "widgets")
	if err != nil {
		t.Fatalf("get pkg: %v", err)
	}
	if err := f.models.SetPackageCreatedVia(context.Background(),
		pkg.ID, models.CreatedViaUploaded); err != nil {
		t.Fatalf("flip provenance: %v", err)
	}

	// After the flip, /simple/widgets/ must NOT contact upstream.
	resp, _ = f.get(t, base+"/simple/widgets/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/simple/ after flip: status=%d", resp.StatusCode)
	}
	if got := atomic.LoadInt32(f.upstreamHit); got != beforeFlip {
		t.Errorf("flip to uploaded should stop upstream contact; before=%d after=%d",
			beforeFlip, got)
	}
}
