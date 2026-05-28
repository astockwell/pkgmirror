// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// SPDX-License-Identifier: MIT

package debian_test

import (
	"archive/tar"
	"bytes"
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

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/blakesmith/ar"
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

func (f *fixture) repoBase() string {
	return "/api/packages/" + f.tenant + "/debian"
}

// buildDeb constructs a minimal but parser-valid .deb: an `ar`
// archive carrying a single `control.tar` member whose tar contains
// a `control` text file. We don't need data.tar — our parser never
// reads it.
func buildDeb(t *testing.T, name, version, arch string) []byte {
	t.Helper()
	control := "Package: " + name +
		"\nVersion: " + version +
		"\nArchitecture: " + arch +
		"\nMaintainer: test <t@example.test>" +
		"\nDescription: pkgmirror fixture\n"

	var ctar bytes.Buffer
	tw := tar.NewWriter(&ctar)
	body := []byte(control)
	if err := tw.WriteHeader(&tar.Header{
		Name: "control", Mode: 0o600, Size: int64(len(body)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	var deb bytes.Buffer
	aw := ar.NewWriter(&deb)
	if err := aw.WriteGlobalHeader(); err != nil {
		t.Fatal(err)
	}
	// debian-binary is technically required by the spec; the parser
	// doesn't enforce it, but real-world tooling does.
	binVer := []byte("2.0\n")
	if err := aw.WriteHeader(&ar.Header{Name: "debian-binary", Mode: 0o600, Size: int64(len(binVer))}); err != nil {
		t.Fatal(err)
	}
	if _, err := aw.Write(binVer); err != nil {
		t.Fatal(err)
	}
	if err := aw.WriteHeader(&ar.Header{Name: "control.tar", Mode: 0o600, Size: int64(ctar.Len())}); err != nil {
		t.Fatal(err)
	}
	if _, err := aw.Write(ctar.Bytes()); err != nil {
		t.Fatal(err)
	}
	return deb.Bytes()
}

// --- Tests -----------------------------------------------------------------

func TestDebian_Upload_Anon401(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	deb := buildDeb(t, "foo", "1.0.0", "amd64")
	resp, _ := f.do(t, http.MethodPut,
		f.repoBase()+"/pool/bookworm/main/upload", deb, false)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anon upload should 401, got %d", resp.StatusCode)
	}
}

func TestDebian_UploadAndDownloadRoundTrip(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	deb := buildDeb(t, "foo", "1.0.0", "amd64")
	if r, b := f.do(t, http.MethodPut,
		f.repoBase()+"/pool/bookworm/main/upload", deb, true); r.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d %s", r.StatusCode, b)
	}

	r, got := f.do(t, http.MethodGet,
		f.repoBase()+"/pool/bookworm/main/foo_1.0.0_amd64.deb", nil, true)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("download: %d", r.StatusCode)
	}
	if !bytes.Equal(got, deb) {
		t.Fatalf("download mismatch")
	}
}

func TestDebian_HEADDeb(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	f.do(t, http.MethodPut, f.repoBase()+"/pool/bookworm/main/upload",
		buildDeb(t, "foo", "1.0.0", "amd64"), true)
	r, body := f.do(t, http.MethodHead,
		f.repoBase()+"/pool/bookworm/main/foo_1.0.0_amd64.deb", nil, true)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("HEAD: %d", r.StatusCode)
	}
	if len(body) != 0 {
		t.Fatalf("HEAD body should be empty, got %d bytes", len(body))
	}
	if r.Header.Get("Content-Length") == "" {
		t.Fatalf("HEAD should set Content-Length")
	}
}

func TestDebian_DuplicateUpload409(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	deb := buildDeb(t, "foo", "1.0.0", "amd64")
	if r, _ := f.do(t, http.MethodPut,
		f.repoBase()+"/pool/bookworm/main/upload", deb, true); r.StatusCode != http.StatusCreated {
		t.Fatalf("first upload: %d", r.StatusCode)
	}
	if r, _ := f.do(t, http.MethodPut,
		f.repoBase()+"/pool/bookworm/main/upload", deb, true); r.StatusCode != http.StatusConflict {
		t.Fatalf("dup upload should 409, got %d", r.StatusCode)
	}
}

func TestDebian_PackagesIndexContainsUpload(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	if r, _ := f.do(t, http.MethodPut, f.repoBase()+"/pool/bookworm/main/upload",
		buildDeb(t, "foo", "1.0.0", "amd64"), true); r.StatusCode != http.StatusCreated {
		t.Fatalf("upload")
	}
	r, body := f.do(t, http.MethodGet,
		f.repoBase()+"/dists/bookworm/main/binary-amd64/Packages", nil, true)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("Packages: %d", r.StatusCode)
	}
	idx := string(body)
	for _, want := range []string{
		"Package: foo\n",
		"Version: 1.0.0\n",
		"Architecture: amd64\n",
		"Filename: pool/bookworm/main/foo_1.0.0_amd64.deb\n",
		"SHA256: ",
	} {
		if !strings.Contains(idx, want) {
			t.Fatalf("Packages missing %q:\n%s", want, idx)
		}
	}
}

func TestDebian_PackagesIndex_EmptyArch404(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	r, _ := f.do(t, http.MethodGet,
		f.repoBase()+"/dists/bookworm/main/binary-amd64/Packages", nil, true)
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("empty: want 404 got %d", r.StatusCode)
	}
}

func TestDebian_ReleaseAndInReleaseAndGpgRoundTrip(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	if r, _ := f.do(t, http.MethodPut, f.repoBase()+"/pool/bookworm/main/upload",
		buildDeb(t, "foo", "1.0.0", "amd64"), true); r.StatusCode != http.StatusCreated {
		t.Fatalf("upload")
	}

	// Fetch the public key first; the signature checks below need it.
	r, pubKeyBody := f.do(t, http.MethodGet, f.repoBase()+"/key.gpg", nil, true)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("key.gpg: %d", r.StatusCode)
	}
	keyring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(pubKeyBody))
	if err != nil {
		t.Fatalf("parse pubkey: %v", err)
	}

	// Plain Release
	r, releaseBody := f.do(t, http.MethodGet,
		f.repoBase()+"/dists/bookworm/Release", nil, true)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("Release: %d", r.StatusCode)
	}
	for _, want := range []string{
		"Origin: pkgmirror\n",
		"Suite: bookworm\n",
		"Components: main\n",
		"Architectures: amd64\n",
		"SHA256:\n",
		"main/binary-amd64/Packages",
	} {
		if !strings.Contains(string(releaseBody), want) {
			t.Fatalf("Release missing %q:\n%s", want, releaseBody)
		}
	}

	// Detached signature: Release.gpg validates against our public key
	r, gpgBody := f.do(t, http.MethodGet,
		f.repoBase()+"/dists/bookworm/Release.gpg", nil, true)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("Release.gpg: %d", r.StatusCode)
	}
	if _, err := openpgp.CheckArmoredDetachedSignature(keyring, bytes.NewReader(releaseBody), bytes.NewReader(gpgBody), nil); err != nil {
		t.Fatalf("Release.gpg does not validate against published public key: %v", err)
	}

	// InRelease: clearsigned, parses + validates
	r, inrelBody := f.do(t, http.MethodGet,
		f.repoBase()+"/dists/bookworm/InRelease", nil, true)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("InRelease: %d", r.StatusCode)
	}
	block, _ := clearsign.Decode(inrelBody)
	if block == nil {
		t.Fatalf("InRelease is not clearsigned PGP:\n%s", inrelBody)
	}
	if _, err := openpgp.CheckDetachedSignature(keyring, bytes.NewReader(block.Bytes), block.ArmoredSignature.Body, nil); err != nil {
		t.Fatalf("InRelease signature does not validate: %v", err)
	}
	// The cleartext inside InRelease should match (modulo
	// trailing-newline normalization) what /Release served. We
	// don't byte-compare because clearsign re-emits with CRLF
	// normalization; checking key fields is sufficient.
	cleartext := string(block.Bytes)
	if !strings.Contains(cleartext, "Suite: bookworm") {
		t.Fatalf("InRelease cleartext missing Suite line:\n%s", cleartext)
	}
}

func TestDebian_DeletePackageFile(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	if r, _ := f.do(t, http.MethodPut, f.repoBase()+"/pool/bookworm/main/upload",
		buildDeb(t, "foo", "1.0.0", "amd64"), true); r.StatusCode != http.StatusCreated {
		t.Fatalf("upload")
	}
	if r, _ := f.do(t, http.MethodDelete,
		f.repoBase()+"/pool/bookworm/main/foo/1.0.0/amd64", nil, true); r.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", r.StatusCode)
	}
	if r, _ := f.do(t, http.MethodGet,
		f.repoBase()+"/pool/bookworm/main/foo_1.0.0_amd64.deb", nil, true); r.StatusCode != http.StatusNotFound {
		t.Fatalf("after delete: want 404 got %d", r.StatusCode)
	}
}

func TestDebian_MultiArchAndComponentInRelease(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	uploads := []struct {
		dist, comp, arch, name, ver string
	}{
		{"bookworm", "main", "amd64", "foo", "1.0.0"},
		{"bookworm", "main", "arm64", "foo", "1.0.0"},
		{"bookworm", "contrib", "amd64", "bar", "2.0.0"},
	}
	for _, u := range uploads {
		if r, b := f.do(t, http.MethodPut,
			f.repoBase()+"/pool/"+u.dist+"/"+u.comp+"/upload",
			buildDeb(t, u.name, u.ver, u.arch), true); r.StatusCode != http.StatusCreated {
			t.Fatalf("upload %+v: %d %s", u, r.StatusCode, b)
		}
	}

	r, body := f.do(t, http.MethodGet, f.repoBase()+"/dists/bookworm/Release", nil, true)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("Release: %d", r.StatusCode)
	}
	s := string(body)
	// Components sorted alphabetically.
	if !strings.Contains(s, "Components: contrib main\n") {
		t.Fatalf("Release components not sorted:\n%s", s)
	}
	if !strings.Contains(s, "Architectures: amd64 arm64\n") {
		t.Fatalf("Release arches not sorted:\n%s", s)
	}
	// Every (comp, arch) combination should appear in the hash
	// section. Only check SHA256 to avoid being noisy.
	for _, path := range []string{
		"main/binary-amd64/Packages",
		"main/binary-arm64/Packages",
		"contrib/binary-amd64/Packages",
	} {
		if !strings.Contains(s, path) {
			t.Fatalf("Release missing path %q:\n%s", path, s)
		}
	}
}

func TestDebian_InvalidDebRejected(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	r, _ := f.do(t, http.MethodPut, f.repoBase()+"/pool/bookworm/main/upload",
		[]byte("not an ar archive at all"), true)
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad deb: want 400 got %d", r.StatusCode)
	}
}

func TestDebian_KeyEndpointServesArmoredPGP(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	r, body := f.do(t, http.MethodGet, f.repoBase()+"/key.gpg", nil, true)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("key.gpg: %d", r.StatusCode)
	}
	if !bytes.HasPrefix(body, []byte("-----BEGIN PGP PUBLIC KEY BLOCK-----")) {
		t.Fatalf("key not armored PGP public key block:\n%s", body)
	}
}
