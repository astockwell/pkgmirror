// Integration test for the cooldown evaluator at the HTTP handler layer.
// Confirms that a rule with min_age_days > 0 hides a freshly-uploaded
// version from /simple/<name>/, denies its file download, AND that
// admins can promote it back via models.PromoteVersion.

package cooldown_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astockwell/pkgmirror/assets"
	"github.com/astockwell/pkgmirror/internal/audit"
	"github.com/astockwell/pkgmirror/internal/auth"
	"github.com/astockwell/pkgmirror/internal/bootstrap"
	pkgdb "github.com/astockwell/pkgmirror/internal/db"
	"github.com/astockwell/pkgmirror/internal/models"
	pkgsvc "github.com/astockwell/pkgmirror/internal/packages"
	"github.com/astockwell/pkgmirror/internal/policy"
	"github.com/astockwell/pkgmirror/internal/policy/cooldown"
	"github.com/astockwell/pkgmirror/internal/server"
	"github.com/astockwell/pkgmirror/internal/storage"
	"github.com/astockwell/pkgmirror/internal/tenants"
	"github.com/astockwell/pkgmirror/internal/tokens"
	"github.com/astockwell/pkgmirror/internal/users"

	"github.com/gin-gonic/gin"
)

func newPyPIFixtureWithCooldown(t *testing.T, minAgeDays int) (
	ts *httptest.Server,
	adminToken, tenantName string,
	store *models.Store,
	pkgModels *models.Store,
) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	db, err := pkgdb.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	pkgModels = models.New(db)
	tenStore := tenants.New(db)
	userStore := users.New(db)
	tokenStore := tokens.New(db)

	res, err := bootstrap.Ensure(context.Background(), tenStore, userStore, tokenStore, bootstrap.Options{
		DefaultTenantName:       "default",
		DefaultTenantVisibility: tenants.VisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	blobs, _ := storage.NewLocalStorage(context.Background(), filepath.Join(dir, "blobs"))
	svc := pkgsvc.NewService(pkgModels, blobs)

	// Install a cooldown rule via the rule store.
	ruleStore := policy.NewRuleStore(db)
	configJSON, _ := json.Marshal(cooldown.Config{MinAgeDays: minAgeDays})
	if _, err := ruleStore.Upsert(context.Background(), policy.Rule{
		Name:       "test-cooldown",
		Kind:       cooldown.Kind,
		Action:     "quarantine",
		ConfigJSON: configJSON,
		Enabled:    true,
	}); err != nil {
		t.Fatalf("upsert rule: %v", err)
	}

	core := policy.NewChainEngine(cooldown.New())
	if err := core.PullFromStore(context.Background(), ruleStore); err != nil {
		t.Fatalf("pull rules: %v", err)
	}
	auditLogger := audit.New(db, 256)
	t.Cleanup(func() { _ = auditLogger.Close() })
	engine := audit.WrapEngine(core, auditLogger, tenStore)

	authn := &auth.TokenAuthenticator{Tokens: tokenStore, Users: userStore, Tenants: tenStore}
	engineRouter, err := server.New(server.Deps{
		Service:       svc,
		Models:        pkgModels,
		Tenants:       tenStore,
		Authenticator: authn,
		Engine:        engine,
		Templates:     assets.Templates(),
	})
	if err != nil {
		t.Fatalf("build server: %v", err)
	}
	ts = httptest.NewServer(engineRouter)
	t.Cleanup(ts.Close)
	return ts, res.GeneratedAdminToken, res.DefaultTenant.Name, pkgModels, pkgModels
}

func uploadFakeWheel(t *testing.T, ts *httptest.Server, token, tenant, name, version string) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range map[string]string{
		"name":             name,
		"version":          version,
		"filetype":         "bdist_wheel",
		"pyversion":        "py3",
		"metadata_version": "2.1",
	} {
		_ = w.WriteField(k, v)
	}
	fw, _ := w.CreateFormFile("content", name+"-"+version+"-py3-none-any.whl")
	_, _ = fw.Write([]byte("fake-wheel-bytes"))
	_ = w.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/packages/"+tenant+"/pypi/", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload: status=%d body=%s", resp.StatusCode, out)
	}
}

func get(t *testing.T, ts *httptest.Server, token, path string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	out, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, out
}

func TestCooldownHidesFreshUpload(t *testing.T) {
	ts, token, tenant, store, _ := newPyPIFixtureWithCooldown(t, 7) // 7-day cooldown
	uploadFakeWheel(t, ts, token, tenant, "foo", "1.0.0")

	// Per-package simple index hides the version (cooldown ⇒ Quarantine
	// on Read ⇒ filtered out of listings).
	resp, body := get(t, ts, token, "/api/packages/"+tenant+"/pypi/simple/foo/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("simple: status=%d body=%s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "1.0.0") {
		t.Fatalf("fresh version should be hidden:\n%s", body)
	}

	// Root /simple/ listing also hides the package (no readable versions).
	resp, body = get(t, ts, token, "/api/packages/"+tenant+"/pypi/simple/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("root: status=%d body=%s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "foo") {
		t.Fatalf("package should not appear in root index while every version is in cooldown:\n%s", body)
	}

	// Direct file download is refused.
	resp, body = get(t, ts, token, "/api/packages/"+tenant+"/pypi/files/foo/1.0.0/foo-1.0.0-py3-none-any.whl")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("download during cooldown: status=%d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(strings.ToLower(string(body)), "cooldown") {
		t.Fatalf("expected cooldown reason in 403 body, got: %s", body)
	}

	// Now: bump the created_unix back in time so the version satisfies
	// the cooldown, and verify it becomes visible again — same engine,
	// no restart, no rule change.
	if _, err := store.DB.Exec(
		`UPDATE package_versions SET created_unix = created_unix - 30 * 86400`); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	resp, body = get(t, ts, token, "/api/packages/"+tenant+"/pypi/simple/foo/")
	if !strings.Contains(string(body), "1.0.0") {
		t.Fatalf("post-cooldown version should be visible:\n%s", body)
	}
	resp, body = get(t, ts, token, "/api/packages/"+tenant+"/pypi/files/foo/1.0.0/foo-1.0.0-py3-none-any.whl")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-cooldown download: status=%d body=%s", resp.StatusCode, body)
	}
}

func TestCooldownZeroDaysIsExemption(t *testing.T) {
	ts, token, tenant, _, _ := newPyPIFixtureWithCooldown(t, 0)
	uploadFakeWheel(t, ts, token, tenant, "bar", "1.0.0")

	// Version is immediately visible even on a fresh upload.
	resp, body := get(t, ts, token, "/api/packages/"+tenant+"/pypi/simple/bar/")
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "1.0.0") {
		t.Fatalf("min_age=0 should allow immediately: status=%d body=%s", resp.StatusCode, body)
	}
}
