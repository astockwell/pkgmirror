package pypi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
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
	"github.com/astockwell/pkgmirror/internal/packages/pypi"
	"github.com/astockwell/pkgmirror/internal/server"
	"github.com/astockwell/pkgmirror/internal/storage"
	"github.com/astockwell/pkgmirror/internal/tenants"
	"github.com/astockwell/pkgmirror/internal/tokens"
	"github.com/astockwell/pkgmirror/internal/users"

	"github.com/gin-gonic/gin"
)

type fixture struct {
	ts         *httptest.Server
	adminToken string
	tenantName string
}

func newFixture(t *testing.T, vis tenants.Visibility) *fixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	db, err := pkgdb.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

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
	engine, err := server.New(server.Deps{
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
	httpts := httptest.NewServer(engine)
	t.Cleanup(httpts.Close)
	return &fixture{
		ts:         httpts,
		adminToken: res.GeneratedAdminToken,
		tenantName: res.DefaultTenant.Name,
	}
}

// buildUploadBody constructs a multipart form body identical to what
// twine sends.
func buildUploadBody(t *testing.T, name, version, filename string, content []byte, extra map[string]string) (string, *bytes.Buffer) {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	for k, v := range map[string]string{
		":action":          "file_upload",
		"protocol_version": "1",
		"name":             name,
		"version":          version,
		"filetype":         "sdist",
		"pyversion":        "source",
		"metadata_version": "2.1",
	} {
		_ = w.WriteField(k, v)
	}
	for k, v := range extra {
		_ = w.WriteField(k, v)
	}
	fw, err := w.CreateFormFile("content", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return w.FormDataContentType(), &b
}

func (f *fixture) do(t *testing.T, method, path string, body io.Reader, hdrs map[string]string, withAuth bool) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, f.ts.URL+path, body)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	if withAuth {
		req.Header.Set("Authorization", "Bearer "+f.adminToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

func TestPyPIUploadAndFetchHTML(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	base := "/api/packages/" + f.tenantName + "/pypi"
	content := []byte("fake sdist tarball bytes")

	ct, body := buildUploadBody(t, "Foo_Bar", "1.2.3", "Foo_Bar-1.2.3.tar.gz", content, map[string]string{
		"summary":         "a fake package",
		"requires_python": ">=3.9",
		"home_page":       "https://example.com/foo-bar",
	})

	// Anonymous upload -> 401
	resp, msg := f.do(t, http.MethodPost, base+"/", body,
		map[string]string{"Content-Type": ct}, false)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anon upload: status=%d body=%s", resp.StatusCode, msg)
	}

	// Authed upload -> 201
	ct, body = buildUploadBody(t, "Foo_Bar", "1.2.3", "Foo_Bar-1.2.3.tar.gz", content, map[string]string{
		"summary":         "a fake package",
		"requires_python": ">=3.9",
	})
	resp, msg = f.do(t, http.MethodPost, base+"/", body,
		map[string]string{"Content-Type": ct}, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("authed upload: status=%d body=%s", resp.StatusCode, msg)
	}

	// Simple HTML index (private tenant: needs auth, gets 401 anon)
	resp, _ = f.do(t, http.MethodGet, base+"/simple/foo-bar/", nil, nil, false)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anon simple: status=%d", resp.StatusCode)
	}
	resp, msg = f.do(t, http.MethodGet, base+"/simple/foo-bar/", nil, nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("simple html: status=%d body=%s", resp.StatusCode, msg)
	}
	if !strings.Contains(string(msg), "Foo_Bar-1.2.3.tar.gz") {
		t.Fatalf("simple html missing filename:\n%s", msg)
	}
	if !strings.Contains(string(msg), "#sha256=") {
		t.Fatalf("simple html missing sha256 fragment")
	}
	if !strings.Contains(string(msg), "requires-python") {
		t.Fatalf("simple html missing requires-python attribute")
	}

	// Simple JSON via Accept header
	resp, msg = f.do(t, http.MethodGet, base+"/simple/foo-bar/", nil,
		map[string]string{"Accept": "application/vnd.pypi.simple.v1+json"}, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("simple json: status=%d", resp.StatusCode)
	}
	var pkg struct {
		Name     string
		Versions []string
		Files    []struct {
			Filename string
			URL      string
			Hashes   struct {
				SHA256 string `json:"sha256"`
			}
			Size           int64
			RequiresPython string `json:"requires-python"`
		}
	}
	if err := json.Unmarshal(msg, &pkg); err != nil {
		t.Fatalf("decode json: %v\n%s", err, msg)
	}
	if pkg.Name != "Foo_Bar" || len(pkg.Versions) != 1 || pkg.Versions[0] != "1.2.3" {
		t.Fatalf("json metadata: %+v", pkg)
	}
	if len(pkg.Files) != 1 || pkg.Files[0].Filename != "Foo_Bar-1.2.3.tar.gz" {
		t.Fatalf("json files: %+v", pkg.Files)
	}
	if pkg.Files[0].Size != int64(len(content)) {
		t.Fatalf("size mismatch: got %d want %d", pkg.Files[0].Size, len(content))
	}
	if pkg.Files[0].RequiresPython != ">=3.9" {
		t.Fatalf("requires-python: %q", pkg.Files[0].RequiresPython)
	}

	// File download roundtrips bytes
	resp, msg = f.do(t, http.MethodGet,
		base+"/files/foo-bar/1.2.3/Foo_Bar-1.2.3.tar.gz", nil, nil, true)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(msg, content) {
		t.Fatalf("download: status=%d len=%d want=%d", resp.StatusCode, len(msg), len(content))
	}

	// Root index lists the package
	resp, msg = f.do(t, http.MethodGet, base+"/simple/", nil, nil, true)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(msg), "Foo_Bar") {
		t.Fatalf("root index: status=%d body=%s", resp.StatusCode, msg)
	}
}

func TestPyPIAddSecondFileToExistingVersion(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	base := "/api/packages/" + f.tenantName + "/pypi/"

	// sdist
	ct, body := buildUploadBody(t, "Foo", "1.0.0", "foo-1.0.0.tar.gz",
		[]byte("sdist bytes"), nil)
	resp, msg := f.do(t, http.MethodPost, base, body,
		map[string]string{"Content-Type": ct}, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload sdist: %d %s", resp.StatusCode, msg)
	}

	// wheel — same version, different filename — must succeed (not 409)
	ct, body = buildUploadBody(t, "Foo", "1.0.0", "foo-1.0.0-py3-none-any.whl",
		[]byte("wheel bytes"), nil)
	resp, msg = f.do(t, http.MethodPost, base, body,
		map[string]string{"Content-Type": ct}, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload wheel: %d %s", resp.StatusCode, msg)
	}

	// duplicate filename -> 409
	ct, body = buildUploadBody(t, "Foo", "1.0.0", "foo-1.0.0.tar.gz",
		[]byte("sdist bytes"), nil)
	resp, msg = f.do(t, http.MethodPost, base, body,
		map[string]string{"Content-Type": ct}, true)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate filename: %d %s", resp.StatusCode, msg)
	}

	// Simple JSON shows both files under one version
	resp, msg = f.do(t, http.MethodGet,
		"/api/packages/"+f.tenantName+"/pypi/simple/foo/", nil,
		map[string]string{"Accept": "application/vnd.pypi.simple.v1+json"}, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("simple json: %d", resp.StatusCode)
	}
	var pkg struct {
		Versions []string
		Files    []struct{ Filename string }
	}
	if err := json.Unmarshal(msg, &pkg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(pkg.Versions) != 1 || len(pkg.Files) != 2 {
		t.Fatalf("expected 1 version + 2 files, got versions=%v files=%+v", pkg.Versions, pkg.Files)
	}
}

func TestPyPIUploadValidatesNameVersionAndSHA(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	base := "/api/packages/" + f.tenantName + "/pypi/"

	// invalid name
	ct, body := buildUploadBody(t, "bad!name", "1.0.0", "bad-1.0.0.tar.gz",
		[]byte("x"), nil)
	resp, _ := f.do(t, http.MethodPost, base, body,
		map[string]string{"Content-Type": ct}, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad name: status=%d want 400", resp.StatusCode)
	}

	// invalid version
	ct, body = buildUploadBody(t, "Foo", "not-a-version!", "foo-x.tar.gz",
		[]byte("x"), nil)
	resp, _ = f.do(t, http.MethodPost, base, body,
		map[string]string{"Content-Type": ct}, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad version: status=%d want 400", resp.StatusCode)
	}

	// sha mismatch
	ct, body = buildUploadBody(t, "Foo", "1.0.0", "foo-1.0.0.tar.gz",
		[]byte("real bytes"),
		map[string]string{"sha256_digest": "deadbeef"})
	resp, msg := f.do(t, http.MethodPost, base, body,
		map[string]string{"Content-Type": ct}, true)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(msg), "sha256_digest mismatch") {
		t.Fatalf("sha mismatch: status=%d body=%s", resp.StatusCode, msg)
	}
}

func TestPyPINormalizeName(t *testing.T) {
	cases := map[string]string{
		"Foo":           "foo",
		"Foo_Bar":       "foo-bar",
		"Foo.Bar":       "foo-bar",
		"Foo--__..-Bar": "foo-bar",
		"FOO_BAR.baz":   "foo-bar-baz",
		"simple":        "simple",
	}
	for in, want := range cases {
		if got := pypi.NormalizeName(in); got != want {
			t.Errorf("NormalizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPyPIPublicTenantAnonRead(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPublic)
	base := "/api/packages/" + f.tenantName + "/pypi/"

	ct, body := buildUploadBody(t, "Foo", "1.0.0", "foo-1.0.0.tar.gz",
		[]byte("bytes"), nil)
	resp, msg := f.do(t, http.MethodPost, base, body,
		map[string]string{"Content-Type": ct}, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("authed upload on public tenant: %d %s", resp.StatusCode, msg)
	}

	// Anon read succeeds on public tenant.
	resp, msg = f.do(t, http.MethodGet,
		"/api/packages/"+f.tenantName+"/pypi/simple/foo/", nil, nil, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anon simple on public tenant: %d %s", resp.StatusCode, msg)
	}
	if !strings.Contains(string(msg), "foo-1.0.0.tar.gz") {
		t.Fatalf("missing file in anon listing: %s", msg)
	}
}

// keep fmt import in use across all branches
var _ = fmt.Sprintf
