package container_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	ocipkg "github.com/astockwell/pkgmirror/internal/packages/container"
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
	blobs, _ := storage.NewFS(filepath.Join(dir, "blobs"))
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

// uploadBlob pushes a blob via the monolithic POST + PUT pattern.
// Returns the digest.
func (f *fixture) uploadBlob(t *testing.T, image string, content []byte) string {
	t.Helper()
	base := "/v2/" + f.tenant + "/" + image
	resp, _ := f.do(t, http.MethodPost, base+"/blobs/uploads/", "", nil, true)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start upload: %d", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if location == "" {
		t.Fatalf("missing Location header")
	}
	sum := sha256.Sum256(content)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	// PUT with body + digest query.
	resp, body := f.do(t, http.MethodPut, location+"?digest="+digest, "application/octet-stream", content, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("finalize: %d %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Docker-Content-Digest"); got != digest {
		t.Fatalf("digest header: got %q want %q", got, digest)
	}
	return digest
}

func TestOCI_V2Probe_PrivateAnon401(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	resp, _ := f.do(t, http.MethodGet, "/v2/", "", nil, false)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Www-Authenticate"); !strings.HasPrefix(got, "Bearer realm=") {
		t.Fatalf("WWW-Authenticate: %q", got)
	}
}

func TestOCI_V2Probe_AuthedOK(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	resp, _ := f.do(t, http.MethodGet, "/v2/", "", nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Docker-Distribution-Api-Version"); got != "registry/2.0" {
		t.Fatalf("version header: %q", got)
	}
}

func TestOCI_Token_EchoesBasicPassword(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	req, _ := http.NewRequest(http.MethodGet, f.ts.URL+"/v2/token", nil)
	req.SetBasicAuth("anyuser", f.adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v2/token: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Token != f.adminToken || body.AccessToken != f.adminToken {
		t.Fatalf("token echo: %+v", body)
	}
}

func TestOCI_BlobUpload_MonolithicPUT(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	digest := f.uploadBlob(t, "img", []byte("hello blob"))

	// HEAD should find it.
	resp, _ := f.do(t, http.MethodHead, "/v2/"+f.tenant+"/img/blobs/"+digest, "", nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD: %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Docker-Content-Digest"); got != digest {
		t.Fatalf("digest header: %q", got)
	}

	// GET should return the bytes.
	resp, body := f.do(t, http.MethodGet, "/v2/"+f.tenant+"/img/blobs/"+digest, "", nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET: %d", resp.StatusCode)
	}
	if !bytes.Equal(body, []byte("hello blob")) {
		t.Fatalf("bytes differ: %q", body)
	}
}

func TestOCI_BlobUpload_DigestMismatch(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	base := "/v2/" + f.tenant + "/img"
	resp, _ := f.do(t, http.MethodPost, base+"/blobs/uploads/", "", nil, true)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start: %d", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	// Send body "real" but claim digest of "fake".
	resp, body := f.do(t, http.MethodPut,
		location+"?digest=sha256:0000000000000000000000000000000000000000000000000000000000000000",
		"application/octet-stream", []byte("real"), true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d %s", resp.StatusCode, body)
	}
}

func TestOCI_BlobUpload_ChunkedPATCH(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	base := "/v2/" + f.tenant + "/img"
	resp, _ := f.do(t, http.MethodPost, base+"/blobs/uploads/", "", nil, true)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start: %d", resp.StatusCode)
	}
	location := resp.Header.Get("Location")

	chunks := [][]byte{[]byte("part1 "), []byte("part2 "), []byte("part3")}
	full := bytes.Join(chunks, nil)
	for _, c := range chunks {
		resp, _ := f.do(t, http.MethodPatch, location, "application/octet-stream", c, true)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("patch: %d", resp.StatusCode)
		}
	}
	sum := sha256.Sum256(full)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	resp, body := f.do(t, http.MethodPut, location+"?digest="+digest, "", nil, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("finalize: %d %s", resp.StatusCode, body)
	}
	resp, body = f.do(t, http.MethodGet, "/v2/"+f.tenant+"/img/blobs/"+digest, "", nil, true)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, full) {
		t.Fatalf("download: %d", resp.StatusCode)
	}
}

func TestOCI_Manifest_PutGetByTagAndDigest(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	configBytes := []byte(`{"architecture":"amd64","os":"linux"}`)
	layerBytes := []byte("layer bytes")
	configDigest := f.uploadBlob(t, "myimg", configBytes)
	layerDigest := f.uploadBlob(t, "myimg", layerBytes)

	manifest := map[string]any{
		"schemaVersion": 2,
		"mediaType":     ocipkg.MediaTypeOCIManifest,
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"size":      len(configBytes),
			"digest":    configDigest,
		},
		"layers": []map[string]any{{
			"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
			"size":      len(layerBytes),
			"digest":    layerDigest,
		}},
	}
	body := mustJSON(t, manifest)
	resp, out := f.do(t, http.MethodPut, "/v2/"+f.tenant+"/myimg/manifests/v1",
		ocipkg.MediaTypeOCIManifest, body, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT manifest: %d %s", resp.StatusCode, out)
	}
	manifestDigest := resp.Header.Get("Docker-Content-Digest")
	if manifestDigest == "" {
		t.Fatalf("no Docker-Content-Digest")
	}

	// Tag GET.
	resp, out = f.do(t, http.MethodGet, "/v2/"+f.tenant+"/myimg/manifests/v1", "", nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET by tag: %d %s", resp.StatusCode, out)
	}
	if !bytes.Equal(out, body) {
		t.Fatalf("body differs")
	}
	if resp.Header.Get("Docker-Content-Digest") != manifestDigest {
		t.Fatalf("tag GET digest header: got %q want %q", resp.Header.Get("Docker-Content-Digest"), manifestDigest)
	}

	// Digest GET.
	resp, out = f.do(t, http.MethodGet, "/v2/"+f.tenant+"/myimg/manifests/"+manifestDigest, "", nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET by digest: %d %s", resp.StatusCode, out)
	}
	if !bytes.Equal(out, body) {
		t.Fatalf("digest GET body differs")
	}

	// HEAD by tag.
	resp, _ = f.do(t, http.MethodHead, "/v2/"+f.tenant+"/myimg/manifests/v1", "", nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD by tag: %d", resp.StatusCode)
	}
}

func TestOCI_Manifest_MissingBlobReferenceRejected(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	manifest := map[string]any{
		"schemaVersion": 2,
		"mediaType":     ocipkg.MediaTypeOCIManifest,
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"size":      0,
			"digest":    "sha256:" + strings.Repeat("0", 64),
		},
		"layers": []map[string]any{},
	}
	resp, body := f.do(t, http.MethodPut, "/v2/"+f.tenant+"/img/manifests/v1",
		ocipkg.MediaTypeOCIManifest, mustJSON(t, manifest), true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d %s", resp.StatusCode, body)
	}
}

func TestOCI_TagsList(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	configBytes := []byte(`{"architecture":"amd64","os":"linux"}`)
	configDigest := f.uploadBlob(t, "tagged", configBytes)
	for _, tag := range []string{"v1", "v2", "v3"} {
		manifest := map[string]any{
			"schemaVersion": 2,
			"mediaType":     ocipkg.MediaTypeOCIManifest,
			"config": map[string]any{
				"mediaType": "application/vnd.oci.image.config.v1+json",
				"size":      len(configBytes),
				"digest":    configDigest,
			},
			"layers": []map[string]any{},
		}
		resp, _ := f.do(t, http.MethodPut, "/v2/"+f.tenant+"/tagged/manifests/"+tag,
			ocipkg.MediaTypeOCIManifest, mustJSON(t, manifest), true)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("PUT %s: %d", tag, resp.StatusCode)
		}
	}
	resp, body := f.do(t, http.MethodGet, "/v2/"+f.tenant+"/tagged/tags/list", "", nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tags/list: %d", resp.StatusCode)
	}
	var got struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	_ = json.Unmarshal(body, &got)
	if got.Name != "tagged" {
		t.Fatalf("name: %q", got.Name)
	}
	if len(got.Tags) != 3 {
		t.Fatalf("tags: %+v", got.Tags)
	}
}

func TestOCI_PublicTenantAnonReadAllowed(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPublic)
	digest := f.uploadBlob(t, "pubimg", []byte("public blob"))
	// Anon GET on a public tenant.
	resp, body := f.do(t, http.MethodGet, "/v2/"+f.tenant+"/pubimg/blobs/"+digest, "", nil, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anon blob GET: %d", resp.StatusCode)
	}
	if !bytes.Equal(body, []byte("public blob")) {
		t.Fatalf("bytes differ")
	}
}

func TestOCI_PrivateTenantAnonWrite401(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	resp, _ := f.do(t, http.MethodPost, "/v2/"+f.tenant+"/img/blobs/uploads/", "", nil, false)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anon upload start: %d", resp.StatusCode)
	}
}

func TestOCI_InvalidDigestRejected(t *testing.T) {
	f := newFixture(t, tenants.VisibilityPrivate)
	resp, body := f.do(t, http.MethodGet, "/v2/"+f.tenant+"/img/blobs/notadigest", "", nil, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d %s", resp.StatusCode, body)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// keep fmt used so future test edits don't have to reimport
var _ = fmt.Sprintf
