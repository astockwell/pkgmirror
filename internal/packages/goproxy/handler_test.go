// Tests in this file drive the goproxy HTTP handlers through a real Gin
// engine and a real SQLite DB (in a temp dir), but the system under test
// is in-process — these are grey-box tests focused on protocol behavior
// and error mapping. End-to-end compatibility with the actual `go`
// toolchain is covered by tests/blackbox/goproxy/.

package goproxy_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

type testFixture struct {
	ts          *httptest.Server
	adminToken  string
	tenantName  string
}

// buildModuleZip produces a minimal Go module zip per the Go module zip
// layout: every file path is prefixed with "<module>@<version>/".
func buildModuleZip(t *testing.T, module, version, goMod string, extra map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	prefix := fmt.Sprintf("%s@%s/", module, version)
	write := func(name, contents string) {
		f, err := w.Create(prefix + name)
		if err != nil {
			t.Fatalf("zip create %q: %v", name, err)
		}
		if _, err := io.WriteString(f, contents); err != nil {
			t.Fatalf("zip write %q: %v", name, err)
		}
	}
	write("go.mod", goMod)
	for name, body := range extra {
		write(name, body)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func newTestFixture(t *testing.T) *testFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)

	dir := t.TempDir()
	db, err := pkgdb.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	pkgModels := models.New(db)
	tenantStore := tenants.New(db)
	userStore := users.New(db)
	tokenStore := tokens.New(db)

	ctx := context.Background()
	res, err := bootstrap.Ensure(ctx, tenantStore, userStore, tokenStore, bootstrap.Options{
		DefaultTenantName:       "default",
		DefaultTenantVisibility: tenants.VisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if res.GeneratedAdminToken == "" {
		t.Fatalf("bootstrap did not generate an admin token")
	}

	blobs, err := storage.NewFS(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("open blob storage: %v", err)
	}
	svc := pkgsvc.NewService(pkgModels, blobs)

	authn := &auth.TokenAuthenticator{Tokens: tokenStore, Users: userStore, Tenants: tenantStore}
	engine, err := server.New(server.Deps{
		Service:       svc,
		Models:        pkgModels,
		Tenants:       tenantStore,
		Authenticator: authn,
		Templates:     assets.Templates(),
	})
	if err != nil {
		t.Fatalf("build server: %v", err)
	}
	ts := httptest.NewServer(engine)
	t.Cleanup(ts.Close)

	return &testFixture{
		ts:         ts,
		adminToken: res.GeneratedAdminToken,
		tenantName: res.DefaultTenant.Name,
	}
}

func (f *testFixture) do(t *testing.T, method, path string, body []byte, withAuth bool) (*http.Response, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, f.ts.URL+path, rdr)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if withAuth {
		req.Header.Set("Authorization", "Bearer "+f.adminToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request %s %s: %v", method, path, err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, respBody
}

func TestGoProxyEndToEnd(t *testing.T) {
	f := newTestFixture(t)

	module := "example.com/foo"
	version := "v1.2.3"
	goMod := "module example.com/foo\n\ngo 1.22\n"
	zipBytes := buildModuleZip(t, module, version, goMod, map[string]string{
		"README.md": "hello",
	})

	base := "/api/packages/" + f.tenantName + "/go"

	// --- Upload without auth -> 401 ---
	resp, body := f.do(t, http.MethodPut, base+"/upload", zipBytes, false)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("upload without auth: status=%d body=%s", resp.StatusCode, body)
	}

	// --- Upload with admin token -> 201 ---
	resp, body = f.do(t, http.MethodPut, base+"/upload", zipBytes, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", resp.StatusCode, body)
	}

	// --- Read without auth on a PRIVATE tenant -> 401 ---
	resp, body = f.do(t, http.MethodGet, base+"/example.com/foo/@v/list", nil, false)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("private read without auth: status=%d body=%s", resp.StatusCode, body)
	}

	// --- list ---
	resp, body = f.do(t, http.MethodGet, base+"/example.com/foo/@v/list", nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: status=%d body=%s", resp.StatusCode, body)
	}
	versions := strings.Fields(string(body))
	if len(versions) != 1 || versions[0] != version {
		t.Fatalf("list: expected [%s], got %v", version, versions)
	}

	// --- info ---
	resp, body = f.do(t, http.MethodGet, base+"/example.com/foo/@v/"+version+".info", nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("info: status=%d body=%s", resp.StatusCode, body)
	}
	var info struct {
		Version string
		Time    string
	}
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatalf("info: decode: %v body=%s", err, body)
	}
	if info.Version != version || info.Time == "" {
		t.Fatalf("info: %+v", info)
	}

	// --- mod ---
	resp, body = f.do(t, http.MethodGet, base+"/example.com/foo/@v/"+version+".mod", nil, true)
	if resp.StatusCode != http.StatusOK || string(body) != goMod {
		t.Fatalf("mod: status=%d body=%q want %q", resp.StatusCode, body, goMod)
	}

	// --- zip ---
	resp, body = f.do(t, http.MethodGet, base+"/example.com/foo/@v/"+version+".zip", nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("zip: status=%d body=%s", resp.StatusCode, body)
	}
	if !bytes.Equal(body, zipBytes) {
		t.Fatalf("zip: roundtrip mismatch (got %d bytes, want %d)", len(body), len(zipBytes))
	}

	// --- @latest ---
	resp, body = f.do(t, http.MethodGet, base+"/example.com/foo/@latest", nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("@latest: status=%d body=%s", resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatalf("@latest decode: %v", err)
	}
	if info.Version != version {
		t.Fatalf("@latest: %+v want %s", info, version)
	}

	// --- Duplicate upload -> 409 ---
	resp, body = f.do(t, http.MethodPut, base+"/upload", zipBytes, true)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate upload: status=%d body=%s", resp.StatusCode, body)
	}

	// --- Missing version -> 404 ---
	resp, body = f.do(t, http.MethodGet, base+"/example.com/foo/@v/v9.9.9.info", nil, true)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing version: status=%d body=%s", resp.StatusCode, body)
	}

	// --- Nonexistent tenant -> 404 ---
	resp, body = f.do(t, http.MethodGet, "/api/packages/no-such-tenant/go/foo/@v/list", nil, true)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing tenant: status=%d body=%s", resp.StatusCode, body)
	}
}

func TestGoProxyMultipleVersions(t *testing.T) {
	f := newTestFixture(t)
	module := "example.com/bar"
	base := "/api/packages/" + f.tenantName + "/go"

	for _, v := range []string{"v0.1.0", "v0.2.0", "v1.0.0"} {
		z := buildModuleZip(t, module, v, "module example.com/bar\n", nil)
		resp, body := f.do(t, http.MethodPut, base+"/upload", z, true)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("upload %s: %d %s", v, resp.StatusCode, body)
		}
	}

	resp, body := f.do(t, http.MethodGet, base+"/example.com/bar/@v/list", nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: %d %s", resp.StatusCode, body)
	}
	got := strings.Fields(string(body))
	want := []string{"v0.1.0", "v0.2.0", "v1.0.0"}
	if !equalStrings(got, want) {
		t.Fatalf("list: got %v want %v", got, want)
	}
}

func TestUploadRejectsInvalidZip(t *testing.T) {
	f := newTestFixture(t)
	resp, body := f.do(t, http.MethodPut, "/api/packages/"+f.tenantName+"/go/upload", []byte("not a zip"), true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad zip: status=%d body=%s", resp.StatusCode, body)
	}
}

func TestPublicTenantAllowsAnonRead(t *testing.T) {
	// Create a fixture where the default tenant is PUBLIC so we can verify
	// that read endpoints don't require auth.
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	db, err := pkgdb.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	pkgModels := models.New(db)
	tenantStore := tenants.New(db)
	userStore := users.New(db)
	tokenStore := tokens.New(db)
	ctx := context.Background()
	res, err := bootstrap.Ensure(ctx, tenantStore, userStore, tokenStore, bootstrap.Options{
		DefaultTenantName:       "public-tenant",
		DefaultTenantVisibility: tenants.VisibilityPublic,
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	blobs, _ := storage.NewFS(filepath.Join(dir, "blobs"))
	svc := pkgsvc.NewService(pkgModels, blobs)
	authn := &auth.TokenAuthenticator{Tokens: tokenStore, Users: userStore, Tenants: tenantStore}
	engine, err := server.New(server.Deps{
		Service: svc, Models: pkgModels, Tenants: tenantStore,
		Authenticator: authn, Templates: assets.Templates(),
	})
	if err != nil {
		t.Fatalf("build server: %v", err)
	}
	ts := httptest.NewServer(engine)
	t.Cleanup(ts.Close)

	zipBytes := buildModuleZip(t, "example.com/baz", "v1.0.0", "module example.com/baz\n", nil)
	upReq, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/packages/public-tenant/go/upload", bytes.NewReader(zipBytes))
	upReq.Header.Set("Authorization", "Bearer "+res.GeneratedAdminToken)
	upResp, _ := http.DefaultClient.Do(upReq)
	_, _ = io.Copy(io.Discard, upResp.Body)
	_ = upResp.Body.Close()
	if upResp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d", upResp.StatusCode)
	}

	// Anonymous read of public tenant should succeed.
	listResp, err := http.Get(ts.URL + "/api/packages/public-tenant/go/example.com/baz/@v/list")
	if err != nil {
		t.Fatalf("anon list: %v", err)
	}
	body, _ := io.ReadAll(listResp.Body)
	_ = listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("anon list on public tenant: status=%d body=%s", listResp.StatusCode, body)
	}
	if !strings.Contains(string(body), "v1.0.0") {
		t.Fatalf("anon list missing version: %q", body)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
