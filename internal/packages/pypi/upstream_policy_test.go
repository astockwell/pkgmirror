// Tests that policy gates apply on the cold pull-through paths:
// /simple/<name>/ filters out versions denied by the policy engine, and
// /files/<name>/<version>/<file> refuses to fetch+persist a denied
// upstream artifact. Without these checks a freshly-released upstream
// version (the exact case cooldown is meant to defend against) would
// be served on first sync before the rule ever ran.

package pypi_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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

// policyFixture is a pull-through fixture wired with a real
// ChainEngine + cooldown evaluator, so the cold pull-through path
// actually has a policy to consult. Upstream serves two versions of
// `requests` with explicit upload times (45+ days old "freshish" and
// 60+ days old "stable"); the per-version + package-level Warehouse
// JSON endpoints expose those times so the cooldown evaluator can
// resolve time_source: upstream_publish before any blob fetch.
type policyFixture struct {
	pkgmirror      *httptest.Server
	fakePypi       *httptest.Server
	adminToken     string
	tenantName     string
	tenantID       int64
	freshVersion   string
	freshSha       string
	freshUnix      int64
	staleVersion   string
	staleSha       string
	staleUnix      int64
	simpleHits     *int32
	blobHits       *int32
	warehouseHits  *int32
	warehouseAllHs *int32
}

// newPolicyFixture spins up the fixture. The cooldown rule is
// installed by the caller with whatever (min_age_days, time_source)
// combo it wants to test.
func newPolicyFixture(t *testing.T, ruleConfig cooldown.Config, ruleAction string) *policyFixture {
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

	// Two release artifacts with known publish times.
	freshVersion := "2.34.2"
	staleVersion := "2.33.1"
	freshBody := []byte("PK\x03\x04 fake fresh wheel bytes for " + freshVersion)
	staleBody := []byte("PK\x03\x04 fake stale wheel bytes for " + staleVersion)
	freshSum := sha256.Sum256(freshBody)
	staleSum := sha256.Sum256(staleBody)
	freshSha := hex.EncodeToString(freshSum[:])
	staleSha := hex.EncodeToString(staleSum[:])
	now := time.Now()
	freshUnix := now.Add(-5 * 24 * time.Hour).Unix()  // fresh: 5 days old
	staleUnix := now.Add(-60 * 24 * time.Hour).Unix() // stale: 60 days old

	var simpleHits, blobHits, warehouseHits, warehouseAllHits int32

	mux := http.NewServeMux()
	mux.HandleFunc("/simple/requests/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&simpleHits, 1)
		// PEP 691 JSON when negotiated; HTML otherwise.
		base := "BASEURL"
		body := fmt.Sprintf(`{"name":"requests","meta":{"api-version":"1.0"},
			"versions":["%s","%s"],
			"files":[
				{"filename":"requests-%s-py3-none-any.whl","url":"%s/packages/requests-%s.whl","hashes":{"sha256":"%s"}},
				{"filename":"requests-%s-py3-none-any.whl","url":"%s/packages/requests-%s.whl","hashes":{"sha256":"%s"}}
			]}`,
			staleVersion, freshVersion,
			staleVersion, base, staleVersion, staleSha,
			freshVersion, base, freshVersion, freshSha,
		)
		if strings.Contains(r.Header.Get("Accept"), "application/vnd.pypi.simple.v1+json") {
			w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
			_, _ = io.WriteString(w, body)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!DOCTYPE html><html><body>
<a href="%s/packages/requests-%s.whl#sha256=%s">requests-%s-py3-none-any.whl</a>
<a href="%s/packages/requests-%s.whl#sha256=%s">requests-%s-py3-none-any.whl</a>
</body></html>`,
			base, staleVersion, staleSha, staleVersion,
			base, freshVersion, freshSha, freshVersion,
		)
	})

	// Per-version Warehouse JSON. Used by the /files/ pull-through
	// path to populate Subject.Attrs["upstream_published_unix"] before
	// evaluating ActionIngest.
	mux.HandleFunc("/pypi/requests/"+freshVersion+"/json", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&warehouseHits, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"urls":[{"upload_time_iso_8601":"%s"}]}`,
			time.Unix(freshUnix, 0).UTC().Format(time.RFC3339))
	})
	mux.HandleFunc("/pypi/requests/"+staleVersion+"/json", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&warehouseHits, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"urls":[{"upload_time_iso_8601":"%s"}]}`,
			time.Unix(staleUnix, 0).UTC().Format(time.RFC3339))
	})

	// Package-level Warehouse JSON. The cold /simple/ filter calls
	// this once to learn the publish time of every version in a single
	// upstream round-trip.
	mux.HandleFunc("/pypi/requests/json", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&warehouseAllHits, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w,
			`{"releases":{
				"%s":[{"upload_time_iso_8601":"%s"}],
				"%s":[{"upload_time_iso_8601":"%s"}]
			}}`,
			staleVersion, time.Unix(staleUnix, 0).UTC().Format(time.RFC3339),
			freshVersion, time.Unix(freshUnix, 0).UTC().Format(time.RFC3339),
		)
	})

	mux.HandleFunc("/packages/requests-"+freshVersion+".whl", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&blobHits, 1)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(freshBody)
	})
	mux.HandleFunc("/packages/requests-"+staleVersion+".whl", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&blobHits, 1)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(staleBody)
	})

	fakePypi := httptest.NewServer(mux)
	t.Cleanup(fakePypi.Close)

	// Swap the BASEURL sentinel for the fake's real URL by wrapping
	// the mux. Same pattern as upstream_test.go's newUpstreamFixture.
	realMux := http.NewServeMux()
	for path, h := range map[string]http.HandlerFunc{
		"/simple/requests/":                            mux.ServeHTTP,
		"/pypi/requests/" + freshVersion + "/json":    mux.ServeHTTP,
		"/pypi/requests/" + staleVersion + "/json":    mux.ServeHTTP,
		"/pypi/requests/json":                          mux.ServeHTTP,
		"/packages/requests-" + freshVersion + ".whl": mux.ServeHTTP,
		"/packages/requests-" + staleVersion + ".whl": mux.ServeHTTP,
	} {
		h := h
		realMux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			rec := httptest.NewRecorder()
			h(rec, r)
			body := strings.ReplaceAll(rec.Body.String(), "BASEURL", fakePypi.URL)
			for k, vs := range rec.Header() {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(rec.Code)
			_, _ = io.WriteString(w, body)
		})
	}
	fakePypi.Config.Handler = realMux

	// Upstream fetcher pointed at the fake; allowlist + private-IP +
	// plaintext overrides because httptest binds 127.0.0.1 HTTP.
	fakeURL, _ := url.Parse(fakePypi.URL)
	cfg := upstream.DefaultConfig()
	cfg.AllowedHostsExtra = []string{fakeURL.Hostname()}
	cfg.AllowPrivateIPs = true
	cfg.AllowPlaintext = true
	store := stubUpstreamStore{
		rows: map[string]*upstream.PersistedConfig{
			fmt.Sprintf("%d|pypi", res.DefaultTenant.ID): {
				Mode:        string(upstream.ModeCacheAndServe),
				UpstreamURL: sql.NullString{Valid: true, String: fakePypi.URL},
			},
		},
	}
	fetcher := upstream.New(cfg, store)

	// Install the cooldown rule from the caller's config.
	ruleStore := policy.NewRuleStore(db)
	configJSON, err := json.Marshal(ruleConfig)
	if err != nil {
		t.Fatalf("marshal rule config: %v", err)
	}
	if _, err := ruleStore.Upsert(context.Background(), policy.Rule{
		Name:       "test-cooldown",
		Kind:       cooldown.Kind,
		Action:     ruleAction,
		ConfigJSON: configJSON,
		Enabled:    true,
	}); err != nil {
		t.Fatalf("upsert cooldown rule: %v", err)
	}
	core := policy.NewChainEngine(cooldown.New())
	if err := core.PullFromStore(context.Background(), ruleStore); err != nil {
		t.Fatalf("pull rules: %v", err)
	}
	auditLogger := audit.New(db, 256)
	t.Cleanup(func() { _ = auditLogger.Close() })
	engine := audit.WrapEngine(core, auditLogger, tenStore)

	blobs, _ := storage.NewLocalStorage(context.Background(), filepath.Join(dir, "blobs"))
	svc := pkgsvc.NewService(pkgModels, blobs)
	router, err := server.New(server.Deps{
		Service:   svc,
		Models:    pkgModels,
		Tenants:   tenStore,
		Templates: assets.Templates(),
		Authenticator: &auth.TokenAuthenticator{
			Tokens: tokenStore, Users: userStore, Tenants: tenStore,
		},
		Upstream: fetcher,
		Engine:   engine,
	})
	if err != nil {
		t.Fatalf("build server: %v", err)
	}
	pkgmirror := httptest.NewServer(router)
	t.Cleanup(pkgmirror.Close)

	return &policyFixture{
		pkgmirror:      pkgmirror,
		fakePypi:       fakePypi,
		adminToken:     res.GeneratedAdminToken,
		tenantName:     res.DefaultTenant.Name,
		tenantID:       res.DefaultTenant.ID,
		freshVersion:   freshVersion,
		freshSha:       freshSha,
		freshUnix:      freshUnix,
		staleVersion:   staleVersion,
		staleSha:       staleSha,
		staleUnix:      staleUnix,
		simpleHits:     &simpleHits,
		blobHits:       &blobHits,
		warehouseHits:  &warehouseHits,
		warehouseAllHs: &warehouseAllHits,
	}
}

func (f *policyFixture) get(t *testing.T, path string) (*http.Response, []byte) {
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

// TestPullThroughIndex_UpstreamPublishCooldownFiltersFresh proves the
// cold-cache /simple/ filter hides a fresh upstream version while
// keeping older ones - the scenario someone configures a cooldown to
// defend against (a just-published version that may be compromised).
func TestPullThroughIndex_UpstreamPublishCooldownFiltersFresh(t *testing.T) {
	f := newPolicyFixture(t,
		cooldown.Config{MinAgeDays: 45, TimeSource: cooldown.TimeSourceUpstreamPublish},
		"deny",
	)
	base := "/api/packages/" + f.tenantName + "/pypi"

	resp, body := f.get(t, base+"/simple/requests/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), f.freshVersion) {
		t.Errorf("expected fresh version %s to be filtered out of cold /simple/ index; got %s",
			f.freshVersion, body)
	}
	if !strings.Contains(string(body), f.staleVersion) {
		t.Errorf("expected stale version %s to survive the filter; got %s",
			f.staleVersion, body)
	}
	if atomic.LoadInt32(f.warehouseAllHs) != 1 {
		t.Errorf("expected 1 package-level Warehouse fetch for publish-time enrichment, got %d",
			*f.warehouseAllHs)
	}
}

// TestPullThroughDownload_DeniesFreshUpstream proves the download
// miss path rejects a freshly-published upstream artifact BEFORE
// fetching it, so the policy decision shows up as a 403 and the wheel
// is never persisted (would otherwise be served on every later
// request from the local cache).
func TestPullThroughDownload_DeniesFreshUpstream(t *testing.T) {
	f := newPolicyFixture(t,
		cooldown.Config{MinAgeDays: 45, TimeSource: cooldown.TimeSourceUpstreamPublish},
		"deny",
	)
	base := "/api/packages/" + f.tenantName + "/pypi"

	resp, body := f.get(t, base+"/files/requests/"+f.freshVersion+"/requests-"+f.freshVersion+"-py3-none-any.whl")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 on fresh version download, got status=%d body=%s",
			resp.StatusCode, body)
	}
	if !strings.Contains(strings.ToLower(string(body)), "cooldown") {
		t.Errorf("expected 'cooldown' in 403 reason, got: %s", body)
	}
	if atomic.LoadInt32(f.blobHits) != 0 {
		t.Errorf("expected 0 upstream blob fetches on denied ingest, got %d", *f.blobHits)
	}
}

// TestPullThroughDownload_AllowsStaleUpstream proves the gate is
// version-aware - the same rule that blocks 2.34.2 lets 2.33.1
// through because it's older than the cooldown threshold by upstream
// publish time.
func TestPullThroughDownload_AllowsStaleUpstream(t *testing.T) {
	f := newPolicyFixture(t,
		cooldown.Config{MinAgeDays: 45, TimeSource: cooldown.TimeSourceUpstreamPublish},
		"deny",
	)
	base := "/api/packages/" + f.tenantName + "/pypi"

	resp, body := f.get(t, base+"/files/requests/"+f.staleVersion+"/requests-"+f.staleVersion+"-py3-none-any.whl")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on stale (allowed) version download, got status=%d body=%s",
			resp.StatusCode, body)
	}
	if atomic.LoadInt32(f.blobHits) != 1 {
		t.Errorf("expected exactly 1 blob fetch for the allowed version, got %d", *f.blobHits)
	}
}

// TestPullThroughIndex_IngestSourceCooldownBlocksEverything documents
// the (probably-surprising) behavior of the default cooldown
// time_source: it measures from the local row's created_unix, so on
// the cold pull-through path - where nothing has been ingested yet -
// ingest age is 0 for every candidate version and the entire upstream
// catalog is filtered out. Operators should use
// time_source: upstream_publish (as in the test above) when the
// intent is "block recent upstream releases" rather than "force a
// dwell after I first see a version".
func TestPullThroughIndex_IngestSourceCooldownBlocksEverything(t *testing.T) {
	f := newPolicyFixture(t,
		cooldown.Config{MinAgeDays: 45}, // default time_source: ingest
		"deny",
	)
	base := "/api/packages/" + f.tenantName + "/pypi"

	resp, body := f.get(t, base+"/simple/requests/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), f.freshVersion) || strings.Contains(string(body), f.staleVersion) {
		t.Errorf("expected ingest-source cooldown to block every cold-path version; got %s", body)
	}
}

// Keep the imports honest if cooldown package symbols ever change.
var _ = pkgsvc.CreationInfo{}
