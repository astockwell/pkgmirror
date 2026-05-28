// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// SPDX-License-Identifier: MIT

package nuget_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
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

// --- fixture helpers -------------------------------------------------------

// nuspecTemplate is a minimal valid nuspec used by the test packages.
// One target framework, one dependency, one author. The grey-box
// flow only cares about ID/Version round-tripping; the rest is for
// shape-sanity assertions on the registration index.
const nuspecTemplate = `<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://schemas.microsoft.com/packaging/2013/05/nuspec.xsd">
  <metadata>
    <id>%s</id>
    <version>%s</version>
    <authors>pkgmirror test</authors>
    <description>fixture package for pkgmirror nuget tests</description>
    <projectUrl>https://example.com/pkg</projectUrl>
    <dependencies>
      <group targetFramework=".NETStandard2.1">
        <dependency id="System.Text.Json" version="5.0.0" />
      </group>
    </dependencies>
  </metadata>
</package>`

// makeNupkg builds a minimal .nupkg in memory: a zip archive
// containing one .nuspec at the root plus a single content file.
// The shape is small (~600 bytes) and self-contained — exactly what
// `dotnet pack` would produce for a single-file payload, modulo the
// _rels/ and [Content_Types].xml entries that NuGet clients don't
// actually require for ingestion.
func makeNupkg(t *testing.T, id, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	nuspecW, err := zw.Create(strings.ToLower(id) + ".nuspec")
	if err != nil {
		t.Fatalf("zip create nuspec: %v", err)
	}
	if _, err := io.WriteString(nuspecW, fmt.Sprintf(nuspecTemplate, id, version)); err != nil {
		t.Fatalf("write nuspec: %v", err)
	}
	libW, err := zw.Create("lib/netstandard2.1/" + strings.ToLower(id) + ".dll")
	if err != nil {
		t.Fatalf("zip create lib: %v", err)
	}
	_, _ = libW.Write([]byte("fake-dll-bytes"))
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// multipartNupkg wraps body in a multipart/form-data envelope shaped
// like `dotnet nuget push`'s upload. Returns (envelope, contentType).
func multipartNupkg(t *testing.T, body []byte) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="package"; filename="package.nupkg"`)
	h.Set("Content-Type", "application/octet-stream")
	part, err := mw.CreatePart(h)
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close mw: %v", err)
	}
	return buf.Bytes(), mw.FormDataContentType()
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
		_ = os.RemoveAll(dir) // load-bearing on macOS APFS
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

// do executes an HTTP request and returns response + body bytes.
// auth modes: "" (anon), "bearer", "basic", "apikey".
func (f *fixture) do(t *testing.T, method, path string, body []byte, contentType, authMode string) (*http.Response, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, _ := http.NewRequest(method, f.ts.URL+path, rdr)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	switch authMode {
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+f.adminToken)
	case "basic":
		req.SetBasicAuth("x", f.adminToken)
	case "apikey":
		req.Header.Set("X-NuGet-ApiKey", f.adminToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	out, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, out
}

func (f *fixture) base() string { return "/api/packages/" + f.tenant + "/nuget" }

// --- tests -----------------------------------------------------------------

func TestNuGet_ServiceIndex(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	r, body := f.do(t, http.MethodGet, f.base()+"/index.json", nil, "", "bearer")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status: %d %s", r.StatusCode, body)
	}
	var doc struct {
		Version   string `json:"version"`
		Resources []struct {
			ID   string `json:"@id"`
			Type string `json:"@type"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	if doc.Version != "3.0.0" {
		t.Fatalf("version: %q", doc.Version)
	}
	// Spot-check that the four resource types dotnet looks for are
	// present and rooted at our test server URL.
	want := []string{"SearchQueryService", "RegistrationsBaseUrl", "PackageBaseAddress/3.0.0", "PackagePublish/2.0.0"}
	for _, w := range want {
		found := false
		for _, r := range doc.Resources {
			if r.Type == w {
				if !strings.HasPrefix(r.ID, f.ts.URL) {
					t.Fatalf("resource %q @id %q not absolute under %q", w, r.ID, f.ts.URL)
				}
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("service index missing resource type %q", w)
		}
	}
}

func TestNuGet_Upload_Anon401(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	pkg := makeNupkg(t, "Foo", "1.0.0")
	envelope, ct := multipartNupkg(t, pkg)
	r, _ := f.do(t, http.MethodPut, f.base(), envelope, ct, "")
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", r.StatusCode)
	}
}

func TestNuGet_UploadMultipartAndDownload(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	pkg := makeNupkg(t, "Foo.Bar", "1.0.0")
	envelope, ct := multipartNupkg(t, pkg)
	r, body := f.do(t, http.MethodPut, f.base(), envelope, ct, "apikey")
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d %s", r.StatusCode, body)
	}

	// download .nupkg
	r, got := f.do(t, http.MethodGet, f.base()+"/package/foo.bar/1.0.0/foo.bar.1.0.0.nupkg", nil, "", "bearer")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("download: %d %s", r.StatusCode, got)
	}
	if !bytes.Equal(got, pkg) {
		t.Fatalf("nupkg round-trip mismatch: %d vs %d bytes", len(got), len(pkg))
	}

	// HEAD on the same URL returns 200 with Content-Length but no body.
	r, _ = f.do(t, http.MethodHead, f.base()+"/package/foo.bar/1.0.0/foo.bar.1.0.0.nupkg", nil, "", "bearer")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("HEAD: %d", r.StatusCode)
	}
	if r.Header.Get("Content-Length") == "" {
		t.Fatal("HEAD missing Content-Length")
	}
}

func TestNuGet_UploadRawBody(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	pkg := makeNupkg(t, "RawPush", "2.0.0")
	r, body := f.do(t, http.MethodPut, f.base(), pkg, "application/octet-stream", "basic")
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("raw upload: %d %s", r.StatusCode, body)
	}
}

func TestNuGet_DuplicateUpload409(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	pkg := makeNupkg(t, "Dup", "1.0.0")
	envelope, ct := multipartNupkg(t, pkg)
	if r, _ := f.do(t, http.MethodPut, f.base(), envelope, ct, "apikey"); r.StatusCode != http.StatusCreated {
		t.Fatalf("first: %d", r.StatusCode)
	}
	r, body := f.do(t, http.MethodPut, f.base(), envelope, ct, "apikey")
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("second should 409, got %d %s", r.StatusCode, body)
	}
}

func TestNuGet_BadNupkgRejected(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	envelope, ct := multipartNupkg(t, []byte("not a zip file"))
	r, _ := f.do(t, http.MethodPut, f.base(), envelope, ct, "apikey")
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", r.StatusCode)
	}
}

func TestNuGet_RegistrationIndex(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	for _, v := range []string{"1.0.0", "1.1.0", "2.0.0"} {
		envelope, ct := multipartNupkg(t, makeNupkg(t, "Reg.Pkg", v))
		if r, _ := f.do(t, http.MethodPut, f.base(), envelope, ct, "apikey"); r.StatusCode != http.StatusCreated {
			t.Fatalf("upload %s: %d", v, r.StatusCode)
		}
	}
	r, body := f.do(t, http.MethodGet, f.base()+"/registration/reg.pkg/index.json", nil, "", "bearer")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("regindex: %d %s", r.StatusCode, body)
	}
	var doc struct {
		Count int      `json:"count"`
		Type  []string `json:"@type"`
		Pages []struct {
			Count int    `json:"count"`
			Lower string `json:"lower"`
			Upper string `json:"upper"`
			Items []struct {
				PackageContent string `json:"packageContent"`
				CatalogEntry   struct {
					ID      string `json:"id"`
					Version string `json:"version"`
				} `json:"catalogEntry"`
			} `json:"items"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	if len(doc.Pages) != 1 || doc.Pages[0].Count != 3 {
		t.Fatalf("expected 1 page with 3 items, got %+v", doc.Pages)
	}
	// Sanity check the @id rewriting — every packageContent URL
	// should be absolute and rooted under our test server URL.
	for _, it := range doc.Pages[0].Items {
		if !strings.HasPrefix(it.PackageContent, f.ts.URL) {
			t.Fatalf("packageContent not absolute: %q", it.PackageContent)
		}
		if it.CatalogEntry.ID != "Reg.Pkg" {
			t.Fatalf("CatalogEntry.ID should preserve original case, got %q", it.CatalogEntry.ID)
		}
	}
}

func TestNuGet_RegistrationLeafAndPackageVersions(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	for _, v := range []string{"1.0.0", "1.1.0"} {
		envelope, ct := multipartNupkg(t, makeNupkg(t, "LeafPkg", v))
		if r, _ := f.do(t, http.MethodPut, f.base(), envelope, ct, "apikey"); r.StatusCode != http.StatusCreated {
			t.Fatalf("upload: %d", r.StatusCode)
		}
	}
	// leaf at /registration/<id>/<version>.json
	r, body := f.do(t, http.MethodGet, f.base()+"/registration/leafpkg/1.1.0.json", nil, "", "bearer")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("leaf: %d %s", r.StatusCode, body)
	}
	var leaf struct {
		PackageContent string `json:"packageContent"`
	}
	_ = json.Unmarshal(body, &leaf)
	if !strings.HasSuffix(leaf.PackageContent, "leafpkg.1.1.0.nupkg") {
		t.Fatalf("leaf packageContent: %q", leaf.PackageContent)
	}

	// versions at /package/<id>/index.json
	r, body = f.do(t, http.MethodGet, f.base()+"/package/leafpkg/index.json", nil, "", "bearer")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("versions: %d %s", r.StatusCode, body)
	}
	var pv struct {
		Versions []string `json:"versions"`
	}
	_ = json.Unmarshal(body, &pv)
	if len(pv.Versions) != 2 || pv.Versions[0] != "1.0.0" || pv.Versions[1] != "1.1.0" {
		t.Fatalf("versions: %v", pv.Versions)
	}
}

func TestNuGet_RegistrationIndex_NotFound(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	r, _ := f.do(t, http.MethodGet, f.base()+"/registration/nope/index.json", nil, "", "bearer")
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", r.StatusCode)
	}
}

func TestNuGet_Search(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	for _, id := range []string{"Alpha", "AlphaBeta", "Gamma"} {
		envelope, ct := multipartNupkg(t, makeNupkg(t, id, "1.0.0"))
		if r, _ := f.do(t, http.MethodPut, f.base(), envelope, ct, "apikey"); r.StatusCode != http.StatusCreated {
			t.Fatalf("upload %s: %d", id, r.StatusCode)
		}
	}
	// substring search
	r, body := f.do(t, http.MethodGet, f.base()+"/query?q=alpha", nil, "", "bearer")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("search: %d %s", r.StatusCode, body)
	}
	var sr struct {
		TotalHits int64 `json:"totalHits"`
		Data      []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &sr)
	if sr.TotalHits != 2 || len(sr.Data) != 2 {
		t.Fatalf("expected 2 hits, got totalHits=%d data=%+v", sr.TotalHits, sr.Data)
	}
	// empty q returns everything
	r, body = f.do(t, http.MethodGet, f.base()+"/query?q=", nil, "", "bearer")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("search all: %d", r.StatusCode)
	}
	_ = json.Unmarshal(body, &sr)
	if sr.TotalHits != 3 {
		t.Fatalf("search all: %d", sr.TotalHits)
	}
}

func TestNuGet_DeletePackage(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	pkg := makeNupkg(t, "DelPkg", "1.0.0")
	envelope, ct := multipartNupkg(t, pkg)
	if r, _ := f.do(t, http.MethodPut, f.base(), envelope, ct, "apikey"); r.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d", r.StatusCode)
	}
	r, _ := f.do(t, http.MethodDelete, f.base()+"/delpkg/1.0.0", nil, "", "bearer")
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", r.StatusCode)
	}
	r, _ = f.do(t, http.MethodGet, f.base()+"/package/delpkg/1.0.0/delpkg.1.0.0.nupkg", nil, "", "bearer")
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", r.StatusCode)
	}
}

func TestNuGet_NuspecDownloadable(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	pkg := makeNupkg(t, "NuspecPkg", "3.2.1")
	envelope, ct := multipartNupkg(t, pkg)
	if r, _ := f.do(t, http.MethodPut, f.base(), envelope, ct, "apikey"); r.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d", r.StatusCode)
	}
	// dotnet fetches /package/<id>/<version>/<id>.nuspec for symbol
	// resolution and the IDE Object Browser. The handler serves it
	// from the file row stored during upload.
	r, body := f.do(t, http.MethodGet, f.base()+"/package/nuspecpkg/3.2.1/nuspecpkg.nuspec", nil, "", "bearer")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("nuspec: %d %s", r.StatusCode, body)
	}
	if !strings.Contains(string(body), "<id>NuspecPkg</id>") {
		t.Fatalf("nuspec body missing id: %q", body)
	}
}

func TestNuGet_AnonReadOnPublicTenant(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPublic)
	// Anon can read the service index without a token.
	r, _ := f.do(t, http.MethodGet, f.base()+"/index.json", nil, "", "")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("public anon index: %d", r.StatusCode)
	}
	// Anon still can't write.
	pkg := makeNupkg(t, "WriteCheck", "1.0.0")
	envelope, ct := multipartNupkg(t, pkg)
	r, _ = f.do(t, http.MethodPut, f.base(), envelope, ct, "")
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anon write should 401 even on public, got %d", r.StatusCode)
	}
}
