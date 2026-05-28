package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/astockwell/pkgmirror/assets"
	"github.com/astockwell/pkgmirror/internal/audit"
	"github.com/astockwell/pkgmirror/internal/auth"
	"github.com/astockwell/pkgmirror/internal/bootstrap"
	pkgdb "github.com/astockwell/pkgmirror/internal/db"
	"github.com/astockwell/pkgmirror/internal/models"
	pkgsvc "github.com/astockwell/pkgmirror/internal/packages"
	"github.com/astockwell/pkgmirror/internal/policy"
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
	tenant     *tenants.Tenant
	rules      *policy.RuleStore
	models     *models.Store
	userStore  *users.Store
	tokenStore *tokens.Store
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	db, err := pkgdb.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		// Explicit RemoveAll before t.TempDir's auto-cleanup to dodge
		// a macOS APFS race on fixtures that close fast.
		_ = os.RemoveAll(dir)
	})

	pkgModels := models.New(db)
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
	ruleStore := policy.NewRuleStore(db)
	auditLogger := audit.New(db, 256)
	t.Cleanup(func() { _ = auditLogger.Close() })

	core := policy.NewChainEngine()
	engine := audit.WrapEngine(core, auditLogger, tenStore)

	authn := &auth.TokenAuthenticator{Tokens: tokenStore, Users: userStore, Tenants: tenStore}
	router, err := server.New(server.Deps{
		Service:       svc,
		Models:        pkgModels,
		Tenants:       tenStore,
		Authenticator: authn,
		Engine:        engine,
		Rules:         ruleStore,
		Audit:         auditLogger,
		Templates:     assets.Templates(),
	})
	if err != nil {
		t.Fatalf("build server: %v", err)
	}
	ts := httptest.NewServer(router)
	t.Cleanup(ts.Close)

	return &fixture{
		ts:         ts,
		adminToken: res.GeneratedAdminToken,
		tenant:     res.DefaultTenant,
		rules:      ruleStore,
		models:     pkgModels,
		userStore:  userStore,
		tokenStore: tokenStore,
	}
}

func (f *fixture) do(t *testing.T, method, path string, body []byte, token string) (*http.Response, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, _ := http.NewRequest(method, f.ts.URL+path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	out, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, out
}

func TestAdmin_RequiresSystemAdmin(t *testing.T) {
	f := newFixture(t)

	// No auth at all -> 403
	resp, _ := f.do(t, http.MethodGet, "/admin/rules", nil, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no-auth: status=%d want 403", resp.StatusCode)
	}

	// Create a non-admin user with a non-admin token, try again -> 403.
	ctx := context.Background()
	u, err := f.userStore.Create(ctx, users.CreateOptions{
		Name: "alice", Kind: users.KindHuman, IsAdmin: false,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	plaintext, _, err := f.tokenStore.Issue(ctx, tokens.CreateOptions{
		UserID: u.ID, Name: "alice-tok",
		Scopes: []tokens.Scope{tokens.ScopeRead, tokens.ScopeWrite},
	})
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	resp, _ = f.do(t, http.MethodGet, "/admin/rules", nil, plaintext)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin: status=%d want 403", resp.StatusCode)
	}

	// Admin bootstrap token -> 200
	resp, body := f.do(t, http.MethodGet, "/admin/rules", nil, f.adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin: status=%d body=%s", resp.StatusCode, body)
	}
}

func TestAdmin_RuleCRUD(t *testing.T) {
	f := newFixture(t)
	body, _ := json.Marshal(map[string]any{
		"name":     "global-cd",
		"kind":     "cooldown",
		"action":   "quarantine",
		"config":   map[string]any{"min_age_days": 7},
		"enabled":  true,
	})
	resp, out := f.do(t, http.MethodPost, "/admin/rules", body, f.adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upsert: %d %s", resp.StatusCode, out)
	}
	var ur struct{ ID int64 }
	_ = json.Unmarshal(out, &ur)
	if ur.ID == 0 {
		t.Fatalf("expected id; got %s", out)
	}

	// List shows the rule.
	resp, out = f.do(t, http.MethodGet, "/admin/rules", nil, f.adminToken)
	if !strings.Contains(string(out), "global-cd") {
		t.Fatalf("list missing rule: %s", out)
	}

	// Disable it.
	disabled, _ := json.Marshal(map[string]any{"enabled": false})
	resp, out = f.do(t, http.MethodPost, "/admin/rules/"+itoa(ur.ID)+"/enabled", disabled, f.adminToken)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("disable: %d %s", resp.StatusCode, out)
	}
	resp, out = f.do(t, http.MethodGet, "/admin/rules", nil, f.adminToken)
	if strings.Contains(string(out), "global-cd") {
		t.Fatalf("disabled rule still in list: %s", out)
	}

	// Delete it.
	resp, _ = f.do(t, http.MethodDelete, "/admin/rules/"+itoa(ur.ID), nil, f.adminToken)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: status=%d", resp.StatusCode)
	}
}

func TestAdmin_AuditQuery(t *testing.T) {
	f := newFixture(t)
	// Generate some audit events by upserting + deleting rules.
	for _, name := range []string{"a", "b", "c"} {
		body, _ := json.Marshal(map[string]any{
			"name": name, "kind": "cooldown", "action": "warn",
			"config": map[string]any{"min_age_days": 1},
		})
		_, _ = f.do(t, http.MethodPost, "/admin/rules", body, f.adminToken)
	}

	// The audit logger is asynchronous (buffered + drained by a
	// goroutine), so the rows aren't guaranteed to be visible the
	// instant the POST returns. Poll briefly until we see at least
	// the expected count or hit the timeout. 2 s is generous; the
	// drainer typically flushes within a few ms.
	deadline := time.Now().Add(2 * time.Second)
	var got struct{ Events []audit.Event }
	for {
		resp, out := f.do(t, http.MethodGet, "/admin/audit?action=rule_upsert", nil, f.adminToken)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.StatusCode, out)
		}
		got.Events = nil
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(got.Events) >= 3 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after %s: expected >=3 rule_upsert events, got %d: %+v",
				time.Since(deadline.Add(-2*time.Second)), len(got.Events), got.Events)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAdmin_QuarantineLifecycle(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Insert a package + version directly via the model layer.
	pkg, err := f.models.GetOrCreatePackage(ctx, f.tenant.ID, models.TypePyPI, "foo")
	if err != nil {
		t.Fatalf("create pkg: %v", err)
	}
	ver, err := f.models.CreateVersion(ctx, pkg.ID, "1.0.0", `{}`)
	if err != nil {
		t.Fatalf("create version: %v", err)
	}
	if err := f.models.QuarantineVersion(ctx, ver.ID, 0, "test quarantine"); err != nil {
		t.Fatalf("quarantine: %v", err)
	}

	// list shows it
	resp, out := f.do(t, http.MethodGet, "/admin/quarantine", nil, f.adminToken)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(out), `"test quarantine"`) {
		t.Fatalf("list: status=%d body=%s", resp.StatusCode, out)
	}

	// promote it
	resp, _ = f.do(t, http.MethodPost, "/admin/quarantine/"+itoa(ver.ID)+"/promote", nil, f.adminToken)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("promote: %d", resp.StatusCode)
	}

	// list is now empty
	resp, out = f.do(t, http.MethodGet, "/admin/quarantine", nil, f.adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list2: %d", resp.StatusCode)
	}
	// Either no "quarantined" key or an empty/null array.
	if strings.Contains(string(out), `"test quarantine"`) {
		t.Fatalf("expected promotion to clear: %s", out)
	}
}

func itoa(n int64) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
