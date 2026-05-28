// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// SPDX-License-Identifier: MIT

package maven_test

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
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

// pomFor builds a minimal but well-formed pom.xml for the given GAV.
func pomFor(groupID, artifactID, version string) string {
	return `<?xml version="1.0"?>
<project>
  <modelVersion>4.0.0</modelVersion>
  <groupId>` + groupID + `</groupId>
  <artifactId>` + artifactID + `</artifactId>
  <version>` + version + `</version>
  <name>` + artifactID + `</name>
  <description>fixture</description>
  <url>https://example.test/` + artifactID + `</url>
  <licenses><license><name>MIT</name></license></licenses>
</project>`
}

// repoBase is the URL prefix for the default tenant's maven repo.
func (f *fixture) repoBase() string {
	return "/api/packages/" + f.tenant + "/maven"
}

// path returns "groupId-as-path/artifactId/version/filename".
func gavPath(groupID, artifactID, version, filename string) string {
	return strings.ReplaceAll(groupID, ".", "/") +
		"/" + artifactID +
		"/" + version +
		"/" + filename
}

// --- Tests -----------------------------------------------------------------

func TestMaven_Upload_Anon401(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	resp, _ := f.do(t, http.MethodPut,
		f.repoBase()+"/"+gavPath("com.example", "foo", "1.0.0", "foo-1.0.0.pom"),
		[]byte(pomFor("com.example", "foo", "1.0.0")), false)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous PUT should 401, got %d", resp.StatusCode)
	}
}

func TestMaven_UploadPOMAndJARRoundTrip(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	pom := []byte(pomFor("com.example", "foo", "1.0.0"))
	jar := []byte("fake jar bytes\x00\x01\x02")

	// PUT pom
	r, body := f.do(t, http.MethodPut,
		f.repoBase()+"/"+gavPath("com.example", "foo", "1.0.0", "foo-1.0.0.pom"),
		pom, true)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("pom upload: %d %s", r.StatusCode, body)
	}

	// PUT jar
	r, body = f.do(t, http.MethodPut,
		f.repoBase()+"/"+gavPath("com.example", "foo", "1.0.0", "foo-1.0.0.jar"),
		jar, true)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("jar upload: %d %s", r.StatusCode, body)
	}

	// GET jar
	r, got := f.do(t, http.MethodGet,
		f.repoBase()+"/"+gavPath("com.example", "foo", "1.0.0", "foo-1.0.0.jar"), nil, true)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("jar get: %d", r.StatusCode)
	}
	if !bytes.Equal(got, jar) {
		t.Fatalf("jar bytes mismatch")
	}

	// GET pom
	r, got = f.do(t, http.MethodGet,
		f.repoBase()+"/"+gavPath("com.example", "foo", "1.0.0", "foo-1.0.0.pom"), nil, true)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("pom get: %d", r.StatusCode)
	}
	if !bytes.Equal(got, pom) {
		t.Fatalf("pom bytes mismatch")
	}
}

func TestMaven_HEADReturnsHeadersOnly(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	jar := []byte("xxxxx")
	f.do(t, http.MethodPut,
		f.repoBase()+"/"+gavPath("com.example", "foo", "1.0.0", "foo-1.0.0.pom"),
		[]byte(pomFor("com.example", "foo", "1.0.0")), true)
	f.do(t, http.MethodPut,
		f.repoBase()+"/"+gavPath("com.example", "foo", "1.0.0", "foo-1.0.0.jar"),
		jar, true)

	r, body := f.do(t, http.MethodHead,
		f.repoBase()+"/"+gavPath("com.example", "foo", "1.0.0", "foo-1.0.0.jar"), nil, true)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("HEAD: %d", r.StatusCode)
	}
	if len(body) != 0 {
		t.Fatalf("HEAD should have empty body, got %d bytes", len(body))
	}
	if r.Header.Get("Content-Length") == "" {
		t.Fatalf("HEAD should set Content-Length")
	}
}

func TestMaven_ChecksumDownload(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	jar := []byte("jar body")
	wantSHA1 := sha1.Sum(jar)
	wantSHA1Hex := hex.EncodeToString(wantSHA1[:])

	f.do(t, http.MethodPut,
		f.repoBase()+"/"+gavPath("com.example", "foo", "1.0.0", "foo-1.0.0.pom"),
		[]byte(pomFor("com.example", "foo", "1.0.0")), true)
	f.do(t, http.MethodPut,
		f.repoBase()+"/"+gavPath("com.example", "foo", "1.0.0", "foo-1.0.0.jar"),
		jar, true)

	r, got := f.do(t, http.MethodGet,
		f.repoBase()+"/"+gavPath("com.example", "foo", "1.0.0", "foo-1.0.0.jar.sha1"), nil, true)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("sha1 GET: %d %s", r.StatusCode, got)
	}
	if string(got) != wantSHA1Hex {
		t.Fatalf("sha1: want %q got %q", wantSHA1Hex, got)
	}
}

func TestMaven_ChecksumUploadVerifies(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	jar := []byte("jar body")
	wantSHA1 := sha1.Sum(jar)
	wantSHA1Hex := hex.EncodeToString(wantSHA1[:])

	f.do(t, http.MethodPut,
		f.repoBase()+"/"+gavPath("com.example", "foo", "1.0.0", "foo-1.0.0.pom"),
		[]byte(pomFor("com.example", "foo", "1.0.0")), true)
	f.do(t, http.MethodPut,
		f.repoBase()+"/"+gavPath("com.example", "foo", "1.0.0", "foo-1.0.0.jar"),
		jar, true)

	// correct checksum → 200
	r, _ := f.do(t, http.MethodPut,
		f.repoBase()+"/"+gavPath("com.example", "foo", "1.0.0", "foo-1.0.0.jar.sha1"),
		[]byte(wantSHA1Hex), true)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("checksum PUT (matching): %d", r.StatusCode)
	}

	// wrong checksum → 400
	r, _ = f.do(t, http.MethodPut,
		f.repoBase()+"/"+gavPath("com.example", "foo", "1.0.0", "foo-1.0.0.jar.sha1"),
		[]byte("0000000000000000000000000000000000000000"), true)
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("checksum PUT (mismatch): want 400 got %d", r.StatusCode)
	}
}

func TestMaven_MavenMetadataGenerated(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	for _, v := range []string{"1.0.0", "1.1.0", "2.0.0-SNAPSHOT"} {
		if r, b := f.do(t, http.MethodPut,
			f.repoBase()+"/"+gavPath("com.example", "foo", v, "foo-"+v+".pom"),
			[]byte(pomFor("com.example", "foo", v)), true); r.StatusCode != http.StatusCreated {
			t.Fatalf("seed %s: %d %s", v, r.StatusCode, b)
		}
	}

	r, body := f.do(t, http.MethodGet,
		f.repoBase()+"/com/example/foo/maven-metadata.xml", nil, true)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("maven-metadata GET: %d", r.StatusCode)
	}
	// Decode the XML and verify shape rather than do brittle
	// string matching — element order is checked separately by
	// the conformance test running real `mvn`.
	var meta struct {
		GroupID    string   `xml:"groupId"`
		ArtifactID string   `xml:"artifactId"`
		Release    string   `xml:"versioning>release"`
		Latest     string   `xml:"versioning>latest"`
		Versions   []string `xml:"versioning>versions>version"`
	}
	if err := xml.Unmarshal(body, &meta); err != nil {
		t.Fatalf("decode metadata: %v\n%s", err, body)
	}
	if meta.GroupID != "com.example" || meta.ArtifactID != "foo" {
		t.Fatalf("wrong GAV: %+v", meta)
	}
	// Last upload was SNAPSHOT, so latest == that. release is
	// the last *non-SNAPSHOT*, which is 1.1.0.
	if meta.Latest != "2.0.0-SNAPSHOT" {
		t.Fatalf("latest: want 2.0.0-SNAPSHOT got %q", meta.Latest)
	}
	if meta.Release != "1.1.0" {
		t.Fatalf("release: want 1.1.0 got %q", meta.Release)
	}
	if len(meta.Versions) != 3 {
		t.Fatalf("versions: want 3 got %d: %v", len(meta.Versions), meta.Versions)
	}
}

func TestMaven_MavenMetadataNotFound(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	r, _ := f.do(t, http.MethodGet,
		f.repoBase()+"/com/example/nope/maven-metadata.xml", nil, true)
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown package: want 404, got %d", r.StatusCode)
	}
}

// TestMaven_PutMavenMetadataIgnored: clients PUT a
// freshly-generated maven-metadata.xml after every deploy. We accept
// and ignore — we always regenerate on read. The 200 keeps mvn happy.
func TestMaven_PutMavenMetadataIgnored(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	r, _ := f.do(t, http.MethodPut,
		f.repoBase()+"/com/example/foo/maven-metadata.xml",
		[]byte(`<metadata/>`), true)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("PUT maven-metadata: want 200 got %d", r.StatusCode)
	}
}

func TestMaven_InvalidPathRejected(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	// Missing version segment.
	r, _ := f.do(t, http.MethodPut,
		f.repoBase()+"/foo-1.0.0.pom",
		[]byte(pomFor("com.example", "foo", "1.0.0")), true)
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid path: want 400 got %d", r.StatusCode)
	}
}

func TestMaven_BadPOM400(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	r, _ := f.do(t, http.MethodPut,
		f.repoBase()+"/"+gavPath("com.example", "foo", "1.0.0", "foo-1.0.0.pom"),
		[]byte("not xml"), true)
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad pom: want 400 got %d", r.StatusCode)
	}
}

func TestMaven_DuplicateFile409(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	url := f.repoBase() + "/" + gavPath("com.example", "foo", "1.0.0", "foo-1.0.0.pom")
	body := []byte(pomFor("com.example", "foo", "1.0.0"))
	if r, _ := f.do(t, http.MethodPut, url, body, true); r.StatusCode != http.StatusCreated {
		t.Fatalf("first upload: %d", r.StatusCode)
	}
	if r, _ := f.do(t, http.MethodPut, url, body, true); r.StatusCode != http.StatusConflict {
		t.Fatalf("second upload should 409 got %d", r.StatusCode)
	}
}
