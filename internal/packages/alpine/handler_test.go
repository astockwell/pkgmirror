// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// SPDX-License-Identifier: MIT

package alpine_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	"github.com/astockwell/pkgmirror/internal/users"

	"github.com/gin-gonic/gin"
)

// fixture wires the full HTTP stack the same way the other format
// grey-box suites do. We don't try to mock at the handler level —
// the routes, auth, and DB are all part of the contract we want to
// guard.
type fixture struct {
	ts         *httptest.Server
	adminToken string
	tenant     string
}

func newFixture(t *testing.T, vis tenants.Visibility) *fixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	db, err := pkgdb.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		// macOS APFS sometimes races t.TempDir's RemoveAll against
		// the SQLite WAL teardown — explicit cleanup keeps the test
		// log clean. Mirrors the other grey-box suites.
		_ = os.RemoveAll(dir)
	})

	pkgModels := models.New(db)
	ts := tenants.New(db)
	us := users.New(db)
	tk := tokens.New(db)
	res, err := bootstrap.Ensure(context.Background(), ts, us, tk, bootstrap.Options{
		DefaultTenantName:       "default",
		DefaultTenantVisibility: vis,
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	blobs, _ := storage.NewLocalStorage(context.Background(), filepath.Join(dir, "blobs"))
	svc := pkgsvc.NewService(pkgModels, blobs)
	router, err := server.New(server.Deps{
		Service:   svc,
		Models:    pkgModels,
		Tenants:   ts,
		Templates: assets.Templates(),
		Authenticator: &auth.TokenAuthenticator{
			Tokens: tk, Users: us, Tenants: ts,
		},
	})
	if err != nil {
		t.Fatalf("build server: %v", err)
	}
	httpts := httptest.NewServer(router)
	t.Cleanup(httpts.Close)
	return &fixture{ts: httpts, adminToken: res.GeneratedAdminToken, tenant: res.DefaultTenant.Name}
}

func (f *fixture) do(t *testing.T, method, path string, body []byte, withAuth bool) (*http.Response, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, _ := http.NewRequest(method, f.ts.URL+path, rdr)
	if withAuth {
		req.Header.Set("Authorization", "Bearer "+f.adminToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	out, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, out
}

// buildAPK constructs a synthetic .apk: two gzip streams, the second
// containing a tar with a .PKGINFO entry. Real apks have many more
// streams (data, scripts, etc.) but our parser only cares about the
// PKGINFO and the checksum of its stream.
func buildAPK(t *testing.T, pkginfo string) []byte {
	t.Helper()
	var buf bytes.Buffer
	// Stream 1: dummy header tar.
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Name: ".SIGN.RSA.dummy", Mode: 0o600, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte{0}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	// Stream 2: PKGINFO tar.
	zw = gzip.NewWriter(&buf)
	tw = tar.NewWriter(zw)
	body := []byte(pkginfo)
	if err := tw.WriteHeader(&tar.Header{Name: ".PKGINFO", Mode: 0o600, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func samplePKGINFO(name, version, arch string) string {
	return "pkgname = " + name + "\n" +
		"pkgver = " + version + "\n" +
		"pkgdesc = test package\n" +
		"url = https://example.test/" + name + "\n" +
		"size = 1024\n" +
		"arch = " + arch + "\n" +
		"origin = " + name + "\n" +
		"maintainer = Tester <t@example.test>\n" +
		"license = MIT\n" +
		"builddate = 1700000000\n"
}

// --- Tests -----------------------------------------------------------------

func TestAlpine_Upload_Anon401(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	resp, _ := f.do(t, http.MethodPut,
		"/api/packages/"+f.tenant+"/alpine/v3.20/main",
		buildAPK(t, samplePKGINFO("foo", "1.0.0", "x86_64")), false)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous upload should be 401, got %d", resp.StatusCode)
	}
}

func TestAlpine_UploadAndDownloadRoundTrip(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	apk := buildAPK(t, samplePKGINFO("foo", "1.0.0", "x86_64"))
	resp, body := f.do(t, http.MethodPut,
		"/api/packages/"+f.tenant+"/alpine/v3.20/main", apk, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d %s", resp.StatusCode, body)
	}

	resp, dl := f.do(t, http.MethodGet,
		"/api/packages/"+f.tenant+"/alpine/v3.20/main/x86_64/foo-1.0.0.apk", nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download: %d %s", resp.StatusCode, dl)
	}
	if !bytes.Equal(dl, apk) {
		t.Fatalf("download bytes don't match upload (got %d bytes, want %d)", len(dl), len(apk))
	}
}

func TestAlpine_DuplicateFile_409(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	apk := buildAPK(t, samplePKGINFO("foo", "1.0.0", "x86_64"))
	if r, _ := f.do(t, http.MethodPut,
		"/api/packages/"+f.tenant+"/alpine/v3.20/main", apk, true); r.StatusCode != http.StatusCreated {
		t.Fatalf("first upload: %d", r.StatusCode)
	}
	if r, _ := f.do(t, http.MethodPut,
		"/api/packages/"+f.tenant+"/alpine/v3.20/main", apk, true); r.StatusCode != http.StatusConflict {
		t.Fatalf("second upload should 409, got %d", r.StatusCode)
	}
}

func TestAlpine_NoarchFanout(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)

	// Seed two arches so noarch fan-out has multiple targets.
	for _, a := range []string{"x86_64", "aarch64"} {
		if r, b := f.do(t, http.MethodPut,
			"/api/packages/"+f.tenant+"/alpine/v3.20/main",
			buildAPK(t, samplePKGINFO("seed", "1.0.0", a)), true); r.StatusCode != http.StatusCreated {
			t.Fatalf("seed %s: %d %s", a, r.StatusCode, b)
		}
	}

	// Upload a noarch package.
	if r, b := f.do(t, http.MethodPut,
		"/api/packages/"+f.tenant+"/alpine/v3.20/main",
		buildAPK(t, samplePKGINFO("docs", "2.0.0", "noarch")), true); r.StatusCode != http.StatusCreated {
		t.Fatalf("noarch upload: %d %s", r.StatusCode, b)
	}

	for _, a := range []string{"x86_64", "aarch64"} {
		r, body := f.do(t, http.MethodGet,
			"/api/packages/"+f.tenant+"/alpine/v3.20/main/"+a+"/docs-2.0.0.apk", nil, true)
		if r.StatusCode != http.StatusOK {
			t.Fatalf("download docs-%s: %d %s", a, r.StatusCode, body)
		}
	}
}

func TestAlpine_NoarchFallbackArchitecture(t *testing.T) {
	// Empty repo + noarch should fan out to a single x86_64 file.
	f := newFixture(t, tenants.VisibilityPrivate)
	if r, b := f.do(t, http.MethodPut,
		"/api/packages/"+f.tenant+"/alpine/v3.20/main",
		buildAPK(t, samplePKGINFO("docs", "1.0.0", "noarch")), true); r.StatusCode != http.StatusCreated {
		t.Fatalf("noarch upload: %d %s", r.StatusCode, b)
	}
	if r, _ := f.do(t, http.MethodGet,
		"/api/packages/"+f.tenant+"/alpine/v3.20/main/x86_64/docs-1.0.0.apk", nil, true); r.StatusCode != http.StatusOK {
		t.Fatalf("x86_64 fallback: %d", r.StatusCode)
	}
}

func TestAlpine_DeleteFile(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	if r, _ := f.do(t, http.MethodPut,
		"/api/packages/"+f.tenant+"/alpine/v3.20/main",
		buildAPK(t, samplePKGINFO("foo", "1.0.0", "x86_64")), true); r.StatusCode != http.StatusCreated {
		t.Fatalf("upload")
	}
	if r, _ := f.do(t, http.MethodDelete,
		"/api/packages/"+f.tenant+"/alpine/v3.20/main/x86_64/foo-1.0.0.apk", nil, true); r.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", r.StatusCode)
	}
	if r, _ := f.do(t, http.MethodGet,
		"/api/packages/"+f.tenant+"/alpine/v3.20/main/x86_64/foo-1.0.0.apk", nil, true); r.StatusCode != http.StatusNotFound {
		t.Fatalf("after delete: %d", r.StatusCode)
	}
}

func TestAlpine_IndexContainsUploadedPackage(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	if r, _ := f.do(t, http.MethodPut,
		"/api/packages/"+f.tenant+"/alpine/v3.20/main",
		buildAPK(t, samplePKGINFO("foo", "1.0.0", "x86_64")), true); r.StatusCode != http.StatusCreated {
		t.Fatalf("upload")
	}

	resp, body := f.do(t, http.MethodGet,
		"/api/packages/"+f.tenant+"/alpine/v3.20/main/x86_64/APKINDEX.tar.gz", nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("index: %d %s", resp.StatusCode, body)
	}

	// The first gzip stream is the detached signature; the second
	// contains the real APKINDEX. Read both and verify we find a
	// P:foo line in the index body.
	indexBody := extractAPKINDEX(t, body)
	if !strings.Contains(indexBody, "P:foo\n") {
		t.Fatalf("expected P:foo in index, got:\n%s", indexBody)
	}
	if !strings.Contains(indexBody, "V:1.0.0\n") {
		t.Fatalf("expected V:1.0.0 in index, got:\n%s", indexBody)
	}
	if !strings.Contains(indexBody, "A:x86_64\n") {
		t.Fatalf("expected A:x86_64 in index, got:\n%s", indexBody)
	}
}

// extractAPKINDEX skips the detached signature stream and returns the
// text content of the APKINDEX entry inside the second gzip stream.
func extractAPKINDEX(t *testing.T, archive []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("gzip 1: %v", err)
	}
	// Skip the signature stream — read until EOF to advance the
	// underlying reader to the next stream boundary.
	zr.Multistream(false)
	_, _ = io.Copy(io.Discard, zr)
	if err := zr.Reset(bytes.NewReader(archive)); err != nil {
		t.Fatalf("reset: %v", err)
	}
	// We can't easily seek through Multistream on a re-Reader; the
	// easier path is to read every gzip stream and concatenate. apk
	// treats them all as one logical archive.
	zr.Multistream(true)
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		if hdr.Name == "APKINDEX" {
			b, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("read APKINDEX: %v", err)
			}
			return string(b)
		}
	}
	t.Fatal("APKINDEX entry not found in archive")
	return ""
}

func TestAlpine_KeyEndpointServesPEM(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	resp, body := f.do(t, http.MethodGet, "/api/packages/"+f.tenant+"/alpine/key", nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("key: %d %s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("-----BEGIN PUBLIC KEY-----")) {
		t.Fatalf("expected PEM public key, got:\n%s", body)
	}
	cd := resp.Header.Get("Content-Disposition")
	if !strings.Contains(cd, ".rsa.pub") {
		t.Fatalf("Content-Disposition should end in .rsa.pub, got %q", cd)
	}
}

func TestAlpine_IndexEmptyRepo404(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	r, _ := f.do(t, http.MethodGet,
		"/api/packages/"+f.tenant+"/alpine/v3.20/main/x86_64/APKINDEX.tar.gz", nil, true)
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("empty repo index should 404, got %d", r.StatusCode)
	}
}

// TestAlpine_VersionMetadataPersisted is the regression guard for the
// case where the per-version metadata JSON gets dropped on upload (we
// rely on it for the APKINDEX builder).
func TestAlpine_VersionMetadataPersisted(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	if r, _ := f.do(t, http.MethodPut,
		"/api/packages/"+f.tenant+"/alpine/v3.20/main",
		buildAPK(t, samplePKGINFO("foo", "1.0.0", "x86_64")), true); r.StatusCode != http.StatusCreated {
		t.Fatalf("upload")
	}
	resp, body := f.do(t, http.MethodGet,
		"/api/packages/"+f.tenant+"/alpine/v3.20/main/x86_64/APKINDEX.tar.gz", nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("index: %d", resp.StatusCode)
	}
	idx := extractAPKINDEX(t, body)
	for _, want := range []string{"L:MIT", "m:Tester <t@example.test>", "U:https://example.test/foo"} {
		if !strings.Contains(idx, want) {
			t.Fatalf("missing %q in index:\n%s", want, idx)
		}
	}
}
