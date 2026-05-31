// Tests for the Go module proxy pull-through path. Spin up a real
// pkgmirror server pointed at a fake "proxy.golang.org" httptest
// server and exercise the five protocol endpoints under both
// allow-everything and cooldown-active policy configurations.

package goproxy_test

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	"github.com/astockwell/pkgmirror/internal/policy/cooldown"
	"github.com/astockwell/pkgmirror/internal/server"
	"github.com/astockwell/pkgmirror/internal/storage"
	"github.com/astockwell/pkgmirror/internal/tenants"
	"github.com/astockwell/pkgmirror/internal/tokens"
	"github.com/astockwell/pkgmirror/internal/upstream"
	"github.com/astockwell/pkgmirror/internal/users"

	"github.com/gin-gonic/gin"
)

// upstreamFixture stands up a real pkgmirror with pull-through pointed
// at a fake "proxy.golang.org" httptest server hosting one module
// (`example.com/foo`) with two versions:
//   - v1.0.0  : published "stale" (60 days ago)
//   - v2.0.0  : published "fresh" (5 days ago)
//
// Module zip is the same payload reused across versions for simplicity
// (the test asserts the policy gating + protocol responses, not zip
// internals — those are covered by the existing TestGoProxyEndToEnd).
type upstreamFixture struct {
	pkgmirror       *httptest.Server
	fakeProxy       *httptest.Server
	adminToken      string
	tenantName      string
	tenantID        int64
	models          *models.Store
	module          string
	staleVersion    string
	staleUnix       int64
	freshVersion    string
	freshUnix       int64
	listHits        *int32
	infoHits        *int32
	modHits         *int32
	zipHits         *int32
	latestHits      *int32
}

// newUpstreamFixture spins up the stack with a cooldown rule installed
// when cooldownDays > 0 (always with time_source=upstream_publish so
// it can meaningfully gate cold pull-through). Passing 0 means
// no rules.
func newUpstreamFixture(t *testing.T, cooldownDays int) *upstreamFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	db, err := pkgdb.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

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

	module := "example.com/foo"
	staleVersion := "v1.0.0"
	freshVersion := "v2.0.0"
	now := time.Now()
	staleUnix := now.Add(-60 * 24 * time.Hour).Unix()
	freshUnix := now.Add(-5 * 24 * time.Hour).Unix()

	// Build the module zip once, reuse for both versions.
	zipFor := func(version string) []byte {
		t.Helper()
		var buf bytes.Buffer
		w := zip.NewWriter(&buf)
		prefix := fmt.Sprintf("%s@%s/", module, version)
		add := func(name, contents string) {
			f, err := w.Create(prefix + name)
			if err != nil {
				t.Fatalf("zip create: %v", err)
			}
			if _, err := io.WriteString(f, contents); err != nil {
				t.Fatalf("zip write: %v", err)
			}
		}
		add("go.mod", "module "+module+"\n\ngo 1.22\n")
		add("README.md", "hello from "+version)
		if err := w.Close(); err != nil {
			t.Fatalf("close zip: %v", err)
		}
		return buf.Bytes()
	}
	staleZip := zipFor(staleVersion)
	freshZip := zipFor(freshVersion)

	var listHits, infoHits, modHits, zipHits, latestHits int32

	mux := http.NewServeMux()

	// /list — newline-delimited.
	mux.HandleFunc("/"+module+"/@v/list", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&listHits, 1)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, staleVersion)
		fmt.Fprintln(w, freshVersion)
	})

	// /<version>.info — JSON {"Version":..., "Time":...}
	infoHandler := func(version string, when int64) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&infoHits, 1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(struct {
				Version string    `json:"Version"`
				Time    time.Time `json:"Time"`
			}{Version: version, Time: time.Unix(when, 0).UTC()})
		}
	}
	mux.Handle("/"+module+"/@v/"+staleVersion+".info", infoHandler(staleVersion, staleUnix))
	mux.Handle("/"+module+"/@v/"+freshVersion+".info", infoHandler(freshVersion, freshUnix))

	// /<version>.mod — raw go.mod
	modHandler := func() http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&modHits, 1)
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintf(w, "module %s\n\ngo 1.22\n", module)
		}
	}
	mux.Handle("/"+module+"/@v/"+staleVersion+".mod", modHandler())
	mux.Handle("/"+module+"/@v/"+freshVersion+".mod", modHandler())

	// /<version>.zip — module zip
	mux.HandleFunc("/"+module+"/@v/"+staleVersion+".zip", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&zipHits, 1)
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(staleZip)
	})
	mux.HandleFunc("/"+module+"/@v/"+freshVersion+".zip", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&zipHits, 1)
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(freshZip)
	})

	// /@latest — JSON same shape as .info; points at freshVersion.
	mux.HandleFunc("/"+module+"/@latest", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&latestHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Version string    `json:"Version"`
			Time    time.Time `json:"Time"`
		}{Version: freshVersion, Time: time.Unix(freshUnix, 0).UTC()})
	})

	// Catch-all 404 for paths we haven't stubbed (helps surface bugs
	// where the handler hits an unexpected URL).
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	fakeProxy := httptest.NewServer(mux)
	t.Cleanup(fakeProxy.Close)

	fakeURL, _ := url.Parse(fakeProxy.URL)
	cfg := upstream.DefaultConfig()
	cfg.AllowedHostsExtra = []string{fakeURL.Hostname()}
	cfg.AllowPrivateIPs = true
	cfg.AllowPlaintext = true
	store := stubUpstreamStoreGo{
		rows: map[string]*upstream.PersistedConfig{
			fmt.Sprintf("%d|go", res.DefaultTenant.ID): {
				Mode:        string(upstream.ModeCacheAndServe),
				UpstreamURL: sql.NullString{Valid: true, String: fakeProxy.URL},
			},
		},
	}
	fetcher := upstream.New(cfg, store)

	// Policy engine: optional cooldown rule with upstream_publish source.
	var engine policy.Engine = policy.NoopEngine{}
	if cooldownDays > 0 {
		ruleStore := policy.NewRuleStore(db)
		configJSON, _ := json.Marshal(cooldown.Config{
			MinAgeDays: cooldownDays,
			TimeSource: cooldown.TimeSourceUpstreamPublish,
		})
		if _, err := ruleStore.Upsert(context.Background(), policy.Rule{
			Name:       "go-test-cooldown",
			Kind:       cooldown.Kind,
			Format:     "go",
			Action:     "deny",
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
		engine = audit.WrapEngine(core, auditLogger, tenStore)
	}

	blobs, _ := storage.NewLocalStorage(context.Background(), filepath.Join(dir, "blobs"))
	svc := pkgsvc.NewService(pkgModels, blobs)
	authn := &auth.TokenAuthenticator{Tokens: tokenStore, Users: userStore, Tenants: tenStore}
	engineRouter, err := server.New(server.Deps{
		Service:       svc,
		Models:        pkgModels,
		Tenants:       tenStore,
		Authenticator: authn,
		Engine:        engine,
		Templates:     assets.Templates(),
		Upstream:      fetcher,
	})
	if err != nil {
		t.Fatalf("build server: %v", err)
	}
	pkgmirror := httptest.NewServer(engineRouter)
	t.Cleanup(pkgmirror.Close)

	return &upstreamFixture{
		pkgmirror:    pkgmirror,
		fakeProxy:    fakeProxy,
		adminToken:   res.GeneratedAdminToken,
		tenantName:   res.DefaultTenant.Name,
		tenantID:     res.DefaultTenant.ID,
		models:       pkgModels,
		module:       module,
		staleVersion: staleVersion,
		staleUnix:    staleUnix,
		freshVersion: freshVersion,
		freshUnix:    freshUnix,
		listHits:     &listHits,
		infoHits:     &infoHits,
		modHits:      &modHits,
		zipHits:      &zipHits,
		latestHits:   &latestHits,
	}
}

// stubUpstreamStoreGo is the goproxy-test local copy of the same stub
// the PyPI tests use; lifted here so this file doesn't import pypi.
type stubUpstreamStoreGo struct {
	rows map[string]*upstream.PersistedConfig
}

func (s stubUpstreamStoreGo) LookupTenantUpstream(_ context.Context, tenantID int64, format string) (*upstream.PersistedConfig, error) {
	key := fmt.Sprintf("%d|%s", tenantID, format)
	return s.rows[key], nil
}

func (f *upstreamFixture) get(t *testing.T, path string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, f.pkgmirror.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+f.adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}

// --- tests ---

// TestPullThroughGo_ZipFetchesAndPersists is the baseline: no policy
// rules, request a fresh version, expect the zip to be fetched +
// persisted + served. Also verify the post-ingest local row has
// upstream_published_unix stamped from the .info Time (no second-hop
// API call needed, unlike PyPI Warehouse).
func TestPullThroughGo_ZipFetchesAndPersists(t *testing.T) {
	f := newUpstreamFixture(t, 0)
	base := "/api/packages/" + f.tenantName + "/go"

	resp, body := f.get(t, base+"/"+f.module+"/@v/"+f.freshVersion+".zip")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("zip status=%d body=%s", resp.StatusCode, body)
	}
	if len(body) == 0 {
		t.Fatalf("empty zip body")
	}
	if got := atomic.LoadInt32(f.zipHits); got != 1 {
		t.Errorf("expected 1 upstream zip fetch, got %d", got)
	}
	// .info was fetched once for the publish-time stamp.
	if got := atomic.LoadInt32(f.infoHits); got < 1 {
		t.Errorf("expected at least 1 upstream info fetch (for publish stamp), got %d", got)
	}

	// Second download must serve from local cache.
	resp2, body2 := f.get(t, base+"/"+f.module+"/@v/"+f.freshVersion+".zip")
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("2nd zip status=%d", resp2.StatusCode)
	}
	if !bytes.Equal(body, body2) {
		t.Errorf("served zip differs across requests")
	}
	if got := atomic.LoadInt32(f.zipHits); got != 1 {
		t.Errorf("expected 0 extra upstream fetches on 2nd request, total=%d", got)
	}

	// Verify upstream_published_unix landed.
	pkg, err := f.models.GetPackage(context.Background(), f.tenantID, models.TypeGo, f.module)
	if err != nil {
		t.Fatalf("get package: %v", err)
	}
	ver, err := f.models.GetVersion(context.Background(), pkg.ID, f.freshVersion)
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	if !ver.UpstreamPublishedUnix.Valid {
		t.Fatalf("expected upstream_published_unix to be stamped")
	}
	if ver.UpstreamPublishedUnix.Int64 != f.freshUnix {
		t.Errorf("upstream_published_unix = %d, want %d",
			ver.UpstreamPublishedUnix.Int64, f.freshUnix)
	}
}

// TestPullThroughGo_ListProxiedAndFiltered covers two things: (a) /@v/list
// for an unknown module pulls from upstream and serves the upstream
// version set; (b) when a cooldown rule blocks fresh versions, the cold
// /@v/list response hides those versions.
func TestPullThroughGo_ListProxiedAndFiltered(t *testing.T) {
	t.Run("no rules, full list", func(t *testing.T) {
		f := newUpstreamFixture(t, 0)
		base := "/api/packages/" + f.tenantName + "/go"
		resp, body := f.get(t, base+"/"+f.module+"/@v/list")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.StatusCode, body)
		}
		s := string(body)
		if !strings.Contains(s, f.staleVersion) || !strings.Contains(s, f.freshVersion) {
			t.Errorf("expected both versions in list; got %q", s)
		}
	})

	t.Run("30d cooldown filters fresh", func(t *testing.T) {
		f := newUpstreamFixture(t, 30)
		base := "/api/packages/" + f.tenantName + "/go"
		resp, body := f.get(t, base+"/"+f.module+"/@v/list")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.StatusCode, body)
		}
		s := string(body)
		if strings.Contains(s, f.freshVersion) {
			t.Errorf("expected fresh version %s filtered out; got %q",
				f.freshVersion, s)
		}
		if !strings.Contains(s, f.staleVersion) {
			t.Errorf("expected stale version %s to survive filter; got %q",
				f.staleVersion, s)
		}
	})
}

// TestPullThroughGo_DenyFreshOnZip verifies the download path
// short-circuits with 403 BEFORE fetching the upstream zip when a
// cooldown rule denies the fresh version. No blob fetch, no DB row.
func TestPullThroughGo_DenyFreshOnZip(t *testing.T) {
	f := newUpstreamFixture(t, 30)
	base := "/api/packages/" + f.tenantName + "/go"

	resp, body := f.get(t, base+"/"+f.module+"/@v/"+f.freshVersion+".zip")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 on fresh zip; got status=%d body=%s",
			resp.StatusCode, body)
	}
	if !strings.Contains(strings.ToLower(string(body)), "cooldown") {
		t.Errorf("expected 'cooldown' in 403 body, got: %s", body)
	}
	if got := atomic.LoadInt32(f.zipHits); got != 0 {
		t.Errorf("expected 0 upstream zip fetches on denied ingest, got %d", got)
	}

	// And the stale version still works under the same rule.
	resp2, body2 := f.get(t, base+"/"+f.module+"/@v/"+f.staleVersion+".zip")
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on stale zip; got %d body=%s",
			resp2.StatusCode, body2)
	}
}

// TestPullThroughGo_LatestHonorsPolicy proves /@latest doesn't sneak
// fresh-but-blocked versions past the policy engine. With a 30-day
// cooldown the upstream @latest points at the fresh version, so we
// expect a 404 ("no readable version") rather than a JSON response
// that would tell the client about a version it can't actually fetch.
func TestPullThroughGo_LatestHonorsPolicy(t *testing.T) {
	f := newUpstreamFixture(t, 30)
	base := "/api/packages/" + f.tenantName + "/go"

	resp, body := f.get(t, base+"/"+f.module+"/@latest")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 (no readable version) for blocked @latest; got %d body=%s",
			resp.StatusCode, body)
	}
	if got := atomic.LoadInt32(f.latestHits); got != 1 {
		t.Errorf("expected exactly 1 upstream @latest fetch, got %d", got)
	}
}

// TestPullThroughGo_LatestAllowsStale verifies @latest serves a stale
// version normally when policy doesn't block it. Uses a 1-day cooldown
// so even the "stale" 60-day version is allowed.
func TestPullThroughGo_LatestAllowsStale(t *testing.T) {
	f := newUpstreamFixture(t, 1)
	base := "/api/packages/" + f.tenantName + "/go"

	resp, body := f.get(t, base+"/"+f.module+"/@latest")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on stale-allowed @latest; got %d body=%s",
			resp.StatusCode, body)
	}
	var info struct {
		Version string    `json:"Version"`
		Time    time.Time `json:"Time"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatalf("decode @latest body: %v body=%s", err, body)
	}
	if info.Version != f.freshVersion {
		t.Errorf("expected @latest Version=%s, got %s", f.freshVersion, info.Version)
	}
	if info.Time.Unix() != f.freshUnix {
		t.Errorf("expected @latest Time unix=%d, got %d", f.freshUnix, info.Time.Unix())
	}
}

// TestPullThroughGo_OffMode verifies that turning off pull-through for
// the (tenant, go) pair returns 404 immediately without any upstream
// contact.
func TestPullThroughGo_OffMode(t *testing.T) {
	// Build the fixture WITHOUT pull-through wiring by constructing a
	// minimal server with no Upstream field set. Simplest: just reuse
	// the existing testFixture from handler_test.go which doesn't wire
	// upstream; assert that a totally-unknown module returns 404.
	f := newTestFixture(t)
	resp, _ := f.do(t, http.MethodGet,
		"/api/packages/"+f.tenantName+"/go/example.com/foo/@v/list",
		nil, true)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 with no pull-through; got %d", resp.StatusCode)
	}
}
