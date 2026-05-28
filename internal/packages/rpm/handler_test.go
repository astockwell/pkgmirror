// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// SPDX-License-Identifier: MIT

package rpm_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/xml"
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

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/gin-gonic/gin"
)

// fixtureRPM is the upstream Forgejo test RPM (gitea-test
// 1.0.2-1.x86_64.rpm), gzipped + base64'd for embedding. Decoded
// at first use and cached.
//
// Same blob as parser_test.go's base64RpmPackage — duplicated here
// because Go test packages can't import each other's unexported
// constants. Don't change either copy without changing both.
const fixtureRPMBase64Gz = `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`

// decodeFixtureRPM unpacks the embedded test rpm. The .rpm binary
// is ~3.5 KB; decoding once per test is cheap enough that we don't
// bother caching across tests.
func decodeFixtureRPM(t *testing.T) []byte {
	t.Helper()
	gz, err := base64.StdEncoding.DecodeString(fixtureRPMBase64Gz)
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	gr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	body, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return body
}

// --- fixture ---------------------------------------------------------------

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

func (f *fixture) repoBase() string { return "/api/packages/" + f.tenant + "/rpm" }

// --- tests -----------------------------------------------------------------

func TestRPM_Upload_Anon401(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	resp, _ := f.do(t, http.MethodPut,
		f.repoBase()+"/el9/upload", decodeFixtureRPM(t), false)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anon upload should 401, got %d", resp.StatusCode)
	}
}

func TestRPM_UploadAndDownloadRoundTrip(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	rpmBytes := decodeFixtureRPM(t)

	if r, b := f.do(t, http.MethodPut,
		f.repoBase()+"/el9/upload", rpmBytes, true); r.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d %s", r.StatusCode, b)
	}
	// fixture is gitea-test 1.0.2-1, x86_64 → name shape
	// `gitea-test-1.0.2-1.x86_64.rpm`.
	r, got := f.do(t, http.MethodGet,
		f.repoBase()+"/el9/package/gitea-test/1.0.2-1/x86_64/gitea-test-1.0.2-1.x86_64.rpm", nil, true)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("download: %d", r.StatusCode)
	}
	if !bytes.Equal(got, rpmBytes) {
		t.Fatalf("download mismatch")
	}
}

func TestRPM_DuplicateUpload409(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	rpmBytes := decodeFixtureRPM(t)
	if r, _ := f.do(t, http.MethodPut,
		f.repoBase()+"/el9/upload", rpmBytes, true); r.StatusCode != http.StatusCreated {
		t.Fatalf("first upload")
	}
	if r, _ := f.do(t, http.MethodPut,
		f.repoBase()+"/el9/upload", rpmBytes, true); r.StatusCode != http.StatusConflict {
		t.Fatalf("dup should 409 got %d", r.StatusCode)
	}
}

func TestRPM_HEADPackage(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	f.do(t, http.MethodPut, f.repoBase()+"/el9/upload", decodeFixtureRPM(t), true)
	r, body := f.do(t, http.MethodHead,
		f.repoBase()+"/el9/package/gitea-test/1.0.2-1/x86_64/gitea-test-1.0.2-1.x86_64.rpm", nil, true)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("HEAD: %d", r.StatusCode)
	}
	if len(body) != 0 {
		t.Fatalf("HEAD should have empty body")
	}
	if r.Header.Get("Content-Length") == "" {
		t.Fatalf("HEAD should set Content-Length")
	}
}

func TestRPM_DeletePackage(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	f.do(t, http.MethodPut, f.repoBase()+"/el9/upload", decodeFixtureRPM(t), true)
	if r, _ := f.do(t, http.MethodDelete,
		f.repoBase()+"/el9/package/gitea-test/1.0.2-1/x86_64", nil, true); r.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", r.StatusCode)
	}
	if r, _ := f.do(t, http.MethodGet,
		f.repoBase()+"/el9/package/gitea-test/1.0.2-1/x86_64/gitea-test-1.0.2-1.x86_64.rpm", nil, true); r.StatusCode != http.StatusNotFound {
		t.Fatalf("after delete: want 404 got %d", r.StatusCode)
	}
}

func TestRPM_RepoFileServesValidConfig(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	r, body := f.do(t, http.MethodGet, f.repoBase()+"/el9/repository.repo", nil, true)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("repo file: %d", r.StatusCode)
	}
	s := string(body)
	for _, want := range []string{
		"[pkgmirror-default-el9]",
		"enabled=1",
		"gpgcheck=1",
		"baseurl=",
		"gpgkey=",
		"/repository.key",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("repo file missing %q:\n%s", want, s)
		}
	}
}

func TestRPM_KeyEndpointServesArmoredPGP(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	r, body := f.do(t, http.MethodGet, f.repoBase()+"/el9/repository.key", nil, true)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("key: %d", r.StatusCode)
	}
	if !bytes.HasPrefix(body, []byte("-----BEGIN PGP PUBLIC KEY BLOCK-----")) {
		t.Fatalf("not an armored PGP public key:\n%s", body)
	}
}

func TestRPM_RepomdContainsThreeDataEntries(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	f.do(t, http.MethodPut, f.repoBase()+"/el9/upload", decodeFixtureRPM(t), true)

	r, body := f.do(t, http.MethodGet, f.repoBase()+"/el9/repodata/repomd.xml", nil, true)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("repomd: %d", r.StatusCode)
	}
	// Parse with a tolerant struct; we only need to verify the
	// three expected data types are present.
	var rm struct {
		Data []struct {
			Type     string `xml:"type,attr"`
			Location struct {
				Href string `xml:"href,attr"`
			} `xml:"location"`
		} `xml:"data"`
	}
	if err := xml.Unmarshal(body, &rm); err != nil {
		t.Fatalf("decode repomd: %v\n%s", err, body)
	}
	want := map[string]string{
		"primary":   "repodata/primary.xml.gz",
		"filelists": "repodata/filelists.xml.gz",
		"other":     "repodata/other.xml.gz",
	}
	for _, d := range rm.Data {
		if exp, ok := want[d.Type]; ok {
			if d.Location.Href != exp {
				t.Fatalf("data[%s] href: want %q got %q", d.Type, exp, d.Location.Href)
			}
			delete(want, d.Type)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing repomd entries: %v", want)
	}
}

// TestRPM_RepomdAscValidates is the load-bearing wire-compat check
// — the same kind of detached-sig validation we do for Debian.
// /repomd.xml and /repomd.xml.asc are served from independent
// requests, so the only way this passes is if the index bytes are
// byte-stable across requests (which BuildAll's deterministic
// `releaseTimestamp` guarantees).
func TestRPM_RepomdAscValidates(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	f.do(t, http.MethodPut, f.repoBase()+"/el9/upload", decodeFixtureRPM(t), true)

	_, pubKey := f.do(t, http.MethodGet, f.repoBase()+"/el9/repository.key", nil, true)
	keyring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(pubKey))
	if err != nil {
		t.Fatalf("parse pubkey: %v", err)
	}

	_, repomd := f.do(t, http.MethodGet, f.repoBase()+"/el9/repodata/repomd.xml", nil, true)
	_, asc := f.do(t, http.MethodGet, f.repoBase()+"/el9/repodata/repomd.xml.asc", nil, true)

	if _, err := openpgp.CheckArmoredDetachedSignature(keyring, bytes.NewReader(repomd), bytes.NewReader(asc), nil); err != nil {
		t.Fatalf("repomd.xml.asc does not validate against the published public key: %v", err)
	}
}

func TestRPM_RepodataEmptyGroup404(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	r, _ := f.do(t, http.MethodGet, f.repoBase()+"/el9/repodata/repomd.xml", nil, true)
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("empty repodata: want 404 got %d", r.StatusCode)
	}
}

func TestRPM_PrimaryXMLContainsUpload(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	f.do(t, http.MethodPut, f.repoBase()+"/el9/upload", decodeFixtureRPM(t), true)

	r, body := f.do(t, http.MethodGet, f.repoBase()+"/el9/repodata/primary.xml.gz", nil, true)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("primary.xml.gz: %d", r.StatusCode)
	}
	gr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	xmlBody, _ := io.ReadAll(gr)
	for _, want := range []string{
		`<name>gitea-test</name>`,
		`<arch>x86_64</arch>`,
		`href="package/gitea-test/1.0.2-1/x86_64/gitea-test-1.0.2-1.x86_64.rpm"`,
		`type="rpm"`,
	} {
		if !strings.Contains(string(xmlBody), want) {
			t.Fatalf("primary.xml missing %q:\n%s", want, xmlBody)
		}
	}
}

func TestRPM_BadRPMRejected(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	r, _ := f.do(t, http.MethodPut, f.repoBase()+"/el9/upload",
		[]byte("not an rpm"), true)
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad rpm: want 400 got %d", r.StatusCode)
	}
}

func TestRPM_GroupIsolation(t *testing.T) {
	// Packages in group A shouldn't appear in group B's index.
	f := newFixture(t, tenants.VisibilityPrivate)
	f.do(t, http.MethodPut, f.repoBase()+"/el9/upload", decodeFixtureRPM(t), true)

	r, _ := f.do(t, http.MethodGet, f.repoBase()+"/fedora41/repodata/repomd.xml", nil, true)
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("isolated group: want 404 got %d", r.StatusCode)
	}
}
