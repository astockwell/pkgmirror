package rubygems_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func (f *fixture) do(t *testing.T, method, path, contentType string, body []byte, withAuth bool) (*http.Response, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, _ := http.NewRequest(method, f.ts.URL+path, rdr)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
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

// buildGem produces a minimal valid .gem tarball given a gemspec YAML.
// .gem layout: a tar containing metadata.gz (gzipped gemspec YAML) and
// data.tar.gz (the source — we put a tiny file in to keep clients happy).
func buildGem(t *testing.T, gemspecYAML string) []byte {
	t.Helper()
	// inner data.tar.gz
	var dataGz bytes.Buffer
	dGz := gzip.NewWriter(&dataGz)
	dTw := tar.NewWriter(dGz)
	_ = dTw.WriteHeader(&tar.Header{Name: "lib/dummy.rb", Mode: 0o644, Size: 1})
	_, _ = dTw.Write([]byte{'\n'})
	_ = dTw.Close()
	_ = dGz.Close()

	// metadata.gz
	var metaGz bytes.Buffer
	mGz := gzip.NewWriter(&metaGz)
	_, _ = mGz.Write([]byte(gemspecYAML))
	_ = mGz.Close()

	// Outer tar containing both.
	var outer bytes.Buffer
	tw := tar.NewWriter(&outer)
	if err := tw.WriteHeader(&tar.Header{Name: "metadata.gz", Mode: 0o644, Size: int64(metaGz.Len())}); err != nil {
		t.Fatalf("metadata hdr: %v", err)
	}
	_, _ = tw.Write(metaGz.Bytes())
	if err := tw.WriteHeader(&tar.Header{Name: "data.tar.gz", Mode: 0o644, Size: int64(dataGz.Len())}); err != nil {
		t.Fatalf("data hdr: %v", err)
	}
	_, _ = tw.Write(dataGz.Bytes())
	_ = tw.Close()
	return outer.Bytes()
}

// minimalGemspec is the YAML shape produced by `Gem::Specification#to_yaml`,
// trimmed to the fields our parser cares about. The "object" wrapper is
// preserved to match what real gems emit.
func minimalGemspec(name, version, platform, license string) string {
	if platform == "" {
		platform = "ruby"
	}
	return "" +
		"--- !ruby/object:Gem::Specification\n" +
		"name: " + name + "\n" +
		"version: !ruby/object:Gem::Version\n" +
		"  version: " + version + "\n" +
		"platform: " + platform + "\n" +
		"authors:\n" +
		"- Test\n" +
		"licenses:\n" +
		"- " + license + "\n" +
		"summary: test gem\n" +
		"description: test gem\n" +
		"homepage: https://example.com/\n" +
		"required_ruby_version: !ruby/object:Gem::Requirement\n" +
		"  requirements: []\n" +
		"required_rubygems_version: !ruby/object:Gem::Requirement\n" +
		"  requirements: []\n" +
		"dependencies: []\n"
}

func TestRubyGems_UploadAndDownload(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	base := "/api/packages/" + f.tenant + "/rubygems"
	gem := buildGem(t, minimalGemspec("foo", "1.0.0", "ruby", "MIT"))

	// Anonymous upload -> 401.
	resp, _ := f.do(t, http.MethodPost, base+"/api/v1/gems", "application/octet-stream", gem, false)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anon upload: %d", resp.StatusCode)
	}

	// Authed upload -> 201.
	resp, body := f.do(t, http.MethodPost, base+"/api/v1/gems", "application/octet-stream", gem, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d %s", resp.StatusCode, body)
	}

	// Download the .gem we just uploaded.
	resp, body = f.do(t, http.MethodGet, base+"/gems/foo-1.0.0.gem", "", nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download: %d %s", resp.StatusCode, body)
	}
	if !bytes.Equal(body, gem) {
		t.Fatalf("download: bytes differ (got %d, want %d)", len(body), len(gem))
	}

	// Duplicate upload -> 409.
	resp, _ = f.do(t, http.MethodPost, base+"/api/v1/gems", "application/octet-stream", gem, true)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("dup: %d", resp.StatusCode)
	}
}

func TestRubyGems_CompactIndex_Info(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	base := "/api/packages/" + f.tenant + "/rubygems"
	for _, v := range []string{"1.0.0", "1.1.0"} {
		gem := buildGem(t, minimalGemspec("bar", v, "ruby", "MIT"))
		resp, body := f.do(t, http.MethodPost, base+"/api/v1/gems", "application/octet-stream", gem, true)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("upload %s: %d %s", v, resp.StatusCode, body)
		}
	}
	resp, body := f.do(t, http.MethodGet, base+"/info/bar", "", nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("info: %d %s", resp.StatusCode, body)
	}
	got := string(body)
	if !strings.HasPrefix(got, "---\n") {
		t.Errorf("info missing header: %q", got)
	}
	for _, v := range []string{"1.0.0", "1.1.0"} {
		if !strings.Contains(got, v+" |checksum:") {
			t.Errorf("info missing version %q line in:\n%s", v, got)
		}
	}
}

func TestRubyGems_CompactIndex_Versions(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	base := "/api/packages/" + f.tenant + "/rubygems"
	for _, n := range []string{"alpha", "beta"} {
		gem := buildGem(t, minimalGemspec(n, "1.0.0", "ruby", "MIT"))
		resp, body := f.do(t, http.MethodPost, base+"/api/v1/gems", "application/octet-stream", gem, true)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("upload %s: %d %s", n, resp.StatusCode, body)
		}
	}
	resp, body := f.do(t, http.MethodGet, base+"/versions", "", nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("versions: %d %s", resp.StatusCode, body)
	}
	got := string(body)
	if !strings.HasPrefix(got, "---\n") {
		t.Errorf("versions missing header: %q", got)
	}
	for _, n := range []string{"alpha 1.0.0 ", "beta 1.0.0 "} {
		if !strings.Contains(got, n) {
			t.Errorf("versions missing prefix %q in:\n%s", n, got)
		}
	}
}

func TestRubyGems_LegacySpecsGz(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	base := "/api/packages/" + f.tenant + "/rubygems"
	gem := buildGem(t, minimalGemspec("legacy", "2.0.0", "ruby", "MIT"))
	resp, _ := f.do(t, http.MethodPost, base+"/api/v1/gems", "application/octet-stream", gem, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d", resp.StatusCode)
	}

	resp, body := f.do(t, http.MethodGet, base+"/specs.4.8.gz", "", nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("specs.gz: %d", resp.StatusCode)
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	raw, _ := io.ReadAll(zr)
	_ = zr.Close()
	// Marshal preamble is 0x04 0x08. The package name "legacy" should
	// appear literally in the Marshal byte stream.
	if len(raw) < 2 || raw[0] != 4 || raw[1] != 8 {
		t.Fatalf("expected Marshal v4.8 preamble, got %v", raw[:2])
	}
	if !bytes.Contains(raw, []byte("legacy")) {
		t.Fatalf("expected package name in specs payload")
	}
	if !bytes.Contains(raw, []byte("2.0.0")) {
		t.Fatalf("expected version in specs payload")
	}
}

func TestRubyGems_LatestSpecsGz_Only_Latest(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	base := "/api/packages/" + f.tenant + "/rubygems"
	for _, v := range []string{"1.0.0", "2.0.0"} {
		gem := buildGem(t, minimalGemspec("multi", v, "ruby", "MIT"))
		resp, _ := f.do(t, http.MethodPost, base+"/api/v1/gems", "application/octet-stream", gem, true)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("upload %s: %d", v, resp.StatusCode)
		}
	}
	resp, body := f.do(t, http.MethodGet, base+"/latest_specs.4.8.gz", "", nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("latest_specs.gz: %d", resp.StatusCode)
	}
	zr, _ := gzip.NewReader(bytes.NewReader(body))
	raw, _ := io.ReadAll(zr)
	_ = zr.Close()
	if !bytes.Contains(raw, []byte("multi")) {
		t.Fatalf("expected multi in latest_specs")
	}
	// Only 2.0.0 (the newest by created_unix) should be present.
	if !bytes.Contains(raw, []byte("2.0.0")) {
		t.Fatalf("expected 2.0.0 in latest_specs")
	}
	if bytes.Contains(raw, []byte("1.0.0")) {
		t.Fatalf("did not expect 1.0.0 in latest_specs")
	}
}

func TestRubyGems_QuickGemspec(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	base := "/api/packages/" + f.tenant + "/rubygems"
	gem := buildGem(t, minimalGemspec("qspec", "3.0.0", "ruby", "MIT"))
	resp, _ := f.do(t, http.MethodPost, base+"/api/v1/gems", "application/octet-stream", gem, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d", resp.StatusCode)
	}
	resp, body := f.do(t, http.MethodGet, base+"/quick/Marshal.4.8/qspec-3.0.0.gemspec.rz", "", nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("quick: %d", resp.StatusCode)
	}
	// zlib-compressed Marshal — sanity-check via decompression.
	zr, err := newZlibReader(body)
	if err != nil {
		t.Fatalf("zlib: %v", err)
	}
	raw, _ := io.ReadAll(zr)
	_ = zr.Close()
	if len(raw) < 2 || raw[0] != 4 || raw[1] != 8 {
		t.Fatalf("not a Marshal stream: %v", raw[:2])
	}
	if !bytes.Contains(raw, []byte("Gem::Specification")) {
		t.Fatalf("expected Gem::Specification symbol")
	}
	if !bytes.Contains(raw, []byte("qspec")) || !bytes.Contains(raw, []byte("3.0.0")) {
		t.Fatalf("expected qspec/3.0.0 in stream")
	}
}

func TestRubyGems_Yank_HidesFromIndex(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	base := "/api/packages/" + f.tenant + "/rubygems"
	for _, v := range []string{"1.0.0", "1.1.0"} {
		gem := buildGem(t, minimalGemspec("yankme", v, "ruby", "MIT"))
		resp, _ := f.do(t, http.MethodPost, base+"/api/v1/gems", "application/octet-stream", gem, true)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("upload %s: %d", v, resp.StatusCode)
		}
	}
	form := url.Values{}
	form.Set("gem_name", "yankme")
	form.Set("version", "1.0.0")
	resp, body := f.do(t, http.MethodDelete, base+"/api/v1/gems/yank",
		"application/x-www-form-urlencoded", []byte(form.Encode()), true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("yank: %d %s", resp.StatusCode, body)
	}

	resp, body = f.do(t, http.MethodGet, base+"/info/yankme", "", nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("info: %d %s", resp.StatusCode, body)
	}
	got := string(body)
	if strings.Contains(got, "1.0.0 |") {
		t.Errorf("yanked 1.0.0 still in /info:\n%s", got)
	}
	if !strings.Contains(got, "1.1.0 |") {
		t.Errorf("1.1.0 missing from /info:\n%s", got)
	}
}

func TestRubyGems_InvalidGemRejected(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	base := "/api/packages/" + f.tenant + "/rubygems"
	// Not a tar at all.
	resp, _ := f.do(t, http.MethodPost, base+"/api/v1/gems", "application/octet-stream",
		[]byte("definitely not a gem"), true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestRubyGems_PublicTenantAnonReadAllowed(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPublic)
	base := "/api/packages/" + f.tenant + "/rubygems"
	gem := buildGem(t, minimalGemspec("pub", "1.0.0", "ruby", "MIT"))
	resp, _ := f.do(t, http.MethodPost, base+"/api/v1/gems", "application/octet-stream", gem, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d", resp.StatusCode)
	}
	// Anonymous /info works on a public tenant.
	resp, body := f.do(t, http.MethodGet, base+"/info/pub", "", nil, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anon info: %d %s", resp.StatusCode, body)
	}
}

func TestRubyGems_MetadataPersisted(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	base := "/api/packages/" + f.tenant + "/rubygems"
	gem := buildGem(t, minimalGemspec("metadat", "1.0.0", "ruby", "Apache-2.0"))
	resp, _ := f.do(t, http.MethodPost, base+"/api/v1/gems", "application/octet-stream", gem, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d", resp.StatusCode)
	}
	// The /quick spec embeds licenses verbatim — sanity-check ours appears.
	resp, body := f.do(t, http.MethodGet, base+"/quick/Marshal.4.8/metadat-1.0.0.gemspec.rz", "", nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("quick: %d", resp.StatusCode)
	}
	zr, _ := newZlibReader(body)
	raw, _ := io.ReadAll(zr)
	_ = zr.Close()
	if !bytes.Contains(raw, []byte("Apache-2.0")) {
		t.Fatalf("expected Apache-2.0 license in spec")
	}
}

func TestRubyGems_VersionsFileChecksumStable(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	base := "/api/packages/" + f.tenant + "/rubygems"
	gem := buildGem(t, minimalGemspec("stable", "1.0.0", "ruby", "MIT"))
	resp, _ := f.do(t, http.MethodPost, base+"/api/v1/gems", "application/octet-stream", gem, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d", resp.StatusCode)
	}
	resp1, body1 := f.do(t, http.MethodGet, base+"/versions", "", nil, true)
	resp2, body2 := f.do(t, http.MethodGet, base+"/versions", "", nil, true)
	if resp1.StatusCode != 200 || resp2.StatusCode != 200 {
		t.Fatalf("versions: %d/%d", resp1.StatusCode, resp2.StatusCode)
	}
	if !bytes.Equal(body1, body2) {
		t.Fatalf("versions output should be deterministic")
	}
}

// helper: zlib reader that returns a sensible error on bad input.
func newZlibReader(b []byte) (io.ReadCloser, error) {
	return decompressZlib(b)
}

// keep imports honest if encoding/json + base64 ever go unused while
// iterating tests.
var _ = json.Marshal
var _ = base64.StdEncoding
