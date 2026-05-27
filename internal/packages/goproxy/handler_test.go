package goproxy_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astockwell/pkgmirror/assets"
	pkgdb "github.com/astockwell/pkgmirror/internal/db"
	"github.com/astockwell/pkgmirror/internal/models"
	pkgsvc "github.com/astockwell/pkgmirror/internal/packages"
	"github.com/astockwell/pkgmirror/internal/server"
	"github.com/astockwell/pkgmirror/internal/storage"

	"github.com/gin-gonic/gin"
)

// buildModuleZip produces a minimal Go module zip following
// https://go.dev/ref/mod#zip-files: every file path is prefixed with
// "<module>@<version>/".
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

// newTestServer wires up a complete pkgmirror stack against a temp dir and
// returns an httptest server.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)

	dir := t.TempDir()
	db, err := pkgdb.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store := models.New(db)
	blobs, err := storage.NewFS(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("open blob storage: %v", err)
	}
	svc := pkgsvc.NewService(store, blobs)

	engine, err := server.New(svc, store, assets.Templates())
	if err != nil {
		t.Fatalf("build server: %v", err)
	}
	ts := httptest.NewServer(engine)
	t.Cleanup(ts.Close)
	return ts
}

func mustDo(t *testing.T, req *http.Request) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request %s %s: %v", req.Method, req.URL, err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, body
}

func TestGoProxyEndToEnd(t *testing.T) {
	ts := newTestServer(t)

	module := "example.com/foo"
	version := "v1.2.3"
	goMod := "module example.com/foo\n\ngo 1.22\n"
	zipBytes := buildModuleZip(t, module, version, goMod, map[string]string{
		"README.md": "hello",
	})

	// --- Upload ---
	upReq, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/packages/go/upload", bytes.NewReader(zipBytes))
	resp, body := mustDo(t, upReq)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", resp.StatusCode, body)
	}

	base := ts.URL + "/api/packages/go/" + module

	// --- list ---
	resp, body = mustDo(t, must(http.NewRequest(http.MethodGet, base+"/@v/list", nil)))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: status=%d body=%s", resp.StatusCode, body)
	}
	versions := strings.Fields(string(body))
	if len(versions) != 1 || versions[0] != version {
		t.Fatalf("list: expected [%s], got %v", version, versions)
	}

	// --- info ---
	resp, body = mustDo(t, must(http.NewRequest(http.MethodGet, base+"/@v/"+version+".info", nil)))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("info: status=%d body=%s", resp.StatusCode, body)
	}
	var info struct {
		Version string `json:"Version"`
		Time    string `json:"Time"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatalf("info: decode: %v body=%s", err, body)
	}
	if info.Version != version {
		t.Fatalf("info: expected version=%s got %s", version, info.Version)
	}
	if info.Time == "" {
		t.Fatalf("info: empty Time")
	}

	// --- mod ---
	resp, body = mustDo(t, must(http.NewRequest(http.MethodGet, base+"/@v/"+version+".mod", nil)))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mod: status=%d body=%s", resp.StatusCode, body)
	}
	if string(body) != goMod {
		t.Fatalf("mod: got %q want %q", string(body), goMod)
	}

	// --- zip ---
	resp, body = mustDo(t, must(http.NewRequest(http.MethodGet, base+"/@v/"+version+".zip", nil)))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("zip: status=%d body=%s", resp.StatusCode, body)
	}
	if !bytes.Equal(body, zipBytes) {
		t.Fatalf("zip: roundtrip mismatch (got %d bytes, want %d)", len(body), len(zipBytes))
	}
	// Sanity-check it parses as a real zip.
	if _, err := zip.NewReader(bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("zip: not a valid zip: %v", err)
	}

	// --- @latest ---
	resp, body = mustDo(t, must(http.NewRequest(http.MethodGet, base+"/@latest", nil)))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("@latest: status=%d body=%s", resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatalf("@latest decode: %v body=%s", err, body)
	}
	if info.Version != version {
		t.Fatalf("@latest: version=%s want %s", info.Version, version)
	}

	// --- Duplicate upload returns 409 ---
	dupReq, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/packages/go/upload", bytes.NewReader(zipBytes))
	resp, body = mustDo(t, dupReq)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate upload: status=%d body=%s", resp.StatusCode, body)
	}

	// --- Missing version yields 404 ---
	resp, body = mustDo(t, must(http.NewRequest(http.MethodGet, base+"/@v/v9.9.9.info", nil)))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing version: status=%d body=%s", resp.StatusCode, body)
	}
}

func TestGoProxyMultipleVersions(t *testing.T) {
	ts := newTestServer(t)

	module := "example.com/bar"
	for _, v := range []string{"v0.1.0", "v0.2.0", "v1.0.0"} {
		zipBytes := buildModuleZip(t, module, v, "module example.com/bar\n", nil)
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/packages/go/upload", bytes.NewReader(zipBytes))
		resp, body := mustDo(t, req)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("upload %s: %d %s", v, resp.StatusCode, body)
		}
	}

	resp, body := mustDo(t, must(http.NewRequest(http.MethodGet,
		ts.URL+"/api/packages/go/"+module+"/@v/list", nil)))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: %d %s", resp.StatusCode, body)
	}
	got := strings.Fields(string(body))
	want := []string{"v0.1.0", "v0.2.0", "v1.0.0"}
	if !equalStrings(got, want) {
		t.Fatalf("list: got %v want %v", got, want)
	}

	// @latest should be the newest by created time, which is v1.0.0 (uploaded last).
	resp, body = mustDo(t, must(http.NewRequest(http.MethodGet,
		ts.URL+"/api/packages/go/"+module+"/@latest", nil)))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("@latest: %d %s", resp.StatusCode, body)
	}
	var info struct{ Version string }
	_ = json.Unmarshal(body, &info)
	if info.Version != "v1.0.0" {
		t.Fatalf("@latest: %s want v1.0.0", info.Version)
	}
}

func TestUploadRejectsInvalidZip(t *testing.T) {
	ts := newTestServer(t)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/packages/go/upload",
		strings.NewReader("this is not a zip"))
	resp, body := mustDo(t, req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad zip: status=%d body=%s", resp.StatusCode, body)
	}
}

func must(req *http.Request, err error) *http.Request {
	if err != nil {
		panic(err)
	}
	return req
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
