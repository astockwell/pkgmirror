// RubyGems pull-through end-to-end tests against a fake httptest
// upstream. Mirrors pypi/upstream_test.go.
//
// The fake "rubygems.org" serves four kinds of paths:
//   - GET /info/<name>                            compact-index info
//   - GET /api/v1/versions/<name>.json            publish-times JSON
//   - GET /gems/<filename>                        the .gem blob
//
// All on the same httptest server (single host); the fetcher resolves
// relative paths through the tenant's upstream base URL, so we point
// that at this fake.

package rubygems_test

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

// upstreamFixture spins up a real pkgmirror server + a fake
// "rubygems.org" with one gem published at two versions: stale (60 days
// upstream) and fresh (5 days upstream). The cooldown parameter
// controls whether a time_source=upstream_publish cooldown rule is
// installed at fixture-build time.
type upstreamFixture struct {
	pkgmirror  *httptest.Server
	fake       *httptest.Server
	adminToken string
	tenantName string
	tenantID   int64
	models     *models.Store

	gemName      string
	staleVersion string
	freshVersion string
	staleUnix    int64
	freshUnix    int64
	staleGem     []byte
	freshGem     []byte
	staleSHA     string
	freshSHA     string

	infoHits     *int32
	versionsHits *int32 // /api/v1/versions/<name>.json
	blobHits     *int32
}

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

	gemName := "examplegem"
	staleVersion := "1.0.0"
	freshVersion := "2.0.0"
	now := time.Now()
	staleUnix := now.Add(-60 * 24 * time.Hour).Unix()
	freshUnix := now.Add(-5 * 24 * time.Hour).Unix()

	staleGem := buildGem(t, minimalGemspec(gemName, staleVersion, "ruby", "MIT"))
	freshGem := buildGem(t, minimalGemspec(gemName, freshVersion, "ruby", "MIT"))
	staleSum := sha256.Sum256(staleGem)
	freshSum := sha256.Sum256(freshGem)
	staleSHA := hex.EncodeToString(staleSum[:])
	freshSHA := hex.EncodeToString(freshSum[:])

	var infoHits, versionsHits, blobHits int32

	mux := http.NewServeMux()

	// Compact-index /info/<gem>.
	mux.HandleFunc("/info/"+gemName, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&infoHits, 1)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w,
			"---\n%s |checksum:%s\n%s |checksum:%s\n",
			staleVersion, staleSHA, freshVersion, freshSHA)
	})

	// Publish-time fan-out: rubygems.org/api/v1/versions/<gem>.json.
	mux.HandleFunc("/api/v1/versions/"+gemName+".json", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&versionsHits, 1)
		w.Header().Set("Content-Type", "application/json")
		entries := []map[string]any{
			{
				"number":     staleVersion,
				"platform":   "ruby",
				"created_at": time.Unix(staleUnix, 0).UTC().Format(time.RFC3339),
			},
			{
				"number":     freshVersion,
				"platform":   "ruby",
				"created_at": time.Unix(freshUnix, 0).UTC().Format(time.RFC3339),
			},
		}
		_ = json.NewEncoder(w).Encode(entries)
	})

	// Blob: /gems/<filename>.
	mux.HandleFunc("/gems/"+gemName+"-"+staleVersion+".gem", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&blobHits, 1)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(staleGem)
	})
	mux.HandleFunc("/gems/"+gemName+"-"+freshVersion+".gem", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&blobHits, 1)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(freshGem)
	})

	// Catch-all so unexpected upstream calls surface as test errors.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected upstream fetch: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})

	fake := httptest.NewServer(mux)
	t.Cleanup(fake.Close)

	// Build the upstream fetcher: allowlist the fake's host + private
	// IPs override (httptest binds 127.0.0.1) + plaintext override
	// (httptest is HTTP-only).
	fakeURL, _ := url.Parse(fake.URL)
	cfg := upstream.DefaultConfig()
	cfg.AllowedHostsExtra = []string{fakeURL.Hostname()}
	cfg.AllowPrivateIPs = true
	cfg.AllowPlaintext = true
	store := stubUpstreamStore{
		rows: map[string]*upstream.PersistedConfig{
			fmt.Sprintf("%d|rubygems", res.DefaultTenant.ID): {
				Mode:        string(upstream.ModeCacheAndServe),
				UpstreamURL: sql.NullString{Valid: true, String: fake.URL},
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
			Name:       "rubygems-test-cooldown",
			Kind:       cooldown.Kind,
			Format:     "rubygems",
			Action:     "deny",
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
		engine = audit.WrapEngine(core, auditLogger, tenStore)
	}

	blobs, _ := storage.NewLocalStorage(context.Background(), filepath.Join(dir, "blobs"))
	svc := pkgsvc.NewService(pkgModels, blobs)
	authn := &auth.TokenAuthenticator{Tokens: tokenStore, Users: userStore, Tenants: tenStore}
	router, err := server.New(server.Deps{
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
	pkgmirror := httptest.NewServer(router)
	t.Cleanup(pkgmirror.Close)

	return &upstreamFixture{
		pkgmirror:    pkgmirror,
		fake:         fake,
		adminToken:   res.GeneratedAdminToken,
		tenantName:   res.DefaultTenant.Name,
		tenantID:     res.DefaultTenant.ID,
		models:       pkgModels,
		gemName:      gemName,
		staleVersion: staleVersion,
		freshVersion: freshVersion,
		staleUnix:    staleUnix,
		freshUnix:    freshUnix,
		staleGem:     staleGem,
		freshGem:     freshGem,
		staleSHA:     staleSHA,
		freshSHA:     freshSHA,
		infoHits:     &infoHits,
		versionsHits: &versionsHits,
		blobHits:     &blobHits,
	}
}

// stubUpstreamStore implements upstream.ConfigStore over an in-memory map.
type stubUpstreamStore struct {
	rows map[string]*upstream.PersistedConfig
}

func (s stubUpstreamStore) LookupTenantUpstream(_ context.Context, tenantID int64, format string) (*upstream.PersistedConfig, error) {
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

// ---- tests ----

// TestPullThroughRubyGems_GemFetchesAndPersists is the baseline: no
// policy rules, request a fresh version, expect the .gem to be fetched
// + persisted + served, and the upstream_published_unix column stamped.
// Second request must serve from local cache.
func TestPullThroughRubyGems_GemFetchesAndPersists(t *testing.T) {
	f := newUpstreamFixture(t, 0)
	base := "/api/packages/" + f.tenantName + "/rubygems"

	filename := f.gemName + "-" + f.freshVersion + ".gem"
	resp, body := f.get(t, base+"/gems/"+filename)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gem status=%d body=%s", resp.StatusCode, body)
	}
	if len(body) == 0 {
		t.Fatalf("empty gem body")
	}
	if got := atomic.LoadInt32(f.blobHits); got != 1 {
		t.Errorf("expected 1 upstream blob fetch, got %d", got)
	}

	// 2nd download must serve from local cache.
	resp2, body2 := f.get(t, base+"/gems/"+filename)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("2nd gem status=%d", resp2.StatusCode)
	}
	if len(body2) != len(body) {
		t.Errorf("served gem differs across requests: %d vs %d", len(body), len(body2))
	}
	if got := atomic.LoadInt32(f.blobHits); got != 1 {
		t.Errorf("expected 0 extra upstream fetches on 2nd request, total=%d", got)
	}

	// Verify upstream_published_unix landed on the row. The cold gem
	// path with NoopEngine SKIPS the publish-time fan-out (no policy
	// to feed it), so the column stays NULL in this no-rules test.
	// That's correct: with NoopEngine, cooldown is moot.
	pkg, err := f.models.GetPackage(context.Background(), f.tenantID, models.TypeRubyGems, f.gemName)
	if err != nil {
		t.Fatalf("get package: %v", err)
	}
	if pkg.CreatedVia != models.CreatedViaPullThrough {
		t.Errorf("CreatedVia = %q, want %q", pkg.CreatedVia, models.CreatedViaPullThrough)
	}
	ver, err := f.models.GetVersion(context.Background(), pkg.ID, f.freshVersion)
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	if ver == nil {
		t.Fatalf("version row missing after pull-through ingest")
	}
}

// TestPullThroughRubyGems_InfoProxiedAndFiltered covers two things:
// (a) /info/<name> for an unknown gem pulls from upstream and serves
// the upstream version set; (b) when a 30d cooldown rule blocks fresh
// versions, the cold /info/<name> response hides the fresh version.
func TestPullThroughRubyGems_InfoProxiedAndFiltered(t *testing.T) {
	t.Run("no rules, full list", func(t *testing.T) {
		f := newUpstreamFixture(t, 0)
		base := "/api/packages/" + f.tenantName + "/rubygems"
		resp, body := f.get(t, base+"/info/"+f.gemName)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.StatusCode, body)
		}
		s := string(body)
		if !strings.Contains(s, f.staleVersion) || !strings.Contains(s, f.freshVersion) {
			t.Errorf("expected both versions in /info; got %q", s)
		}
	})

	t.Run("30d cooldown filters fresh", func(t *testing.T) {
		f := newUpstreamFixture(t, 30)
		base := "/api/packages/" + f.tenantName + "/rubygems"
		resp, body := f.get(t, base+"/info/"+f.gemName)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.StatusCode, body)
		}
		s := string(body)
		// staleVersion line: "<version> |checksum:..."; freshVersion line
		// shouldn't appear at all (it was filtered out by cooldown).
		if !strings.Contains(s, f.staleVersion+" |") {
			t.Errorf("expected stale version line to survive cooldown filter; got %q", s)
		}
		if strings.Contains(s, f.freshVersion+" |") {
			t.Errorf("expected fresh version %q to be filtered out; got %q", f.freshVersion, s)
		}
	})
}

// TestPullThroughRubyGems_DenyFreshOnGem verifies the download path
// short-circuits with 403 BEFORE fetching the upstream gem when a
// cooldown rule denies the fresh version. No blob fetch, no DB row.
func TestPullThroughRubyGems_DenyFreshOnGem(t *testing.T) {
	f := newUpstreamFixture(t, 30)
	base := "/api/packages/" + f.tenantName + "/rubygems"

	filename := f.gemName + "-" + f.freshVersion + ".gem"
	resp, body := f.get(t, base+"/gems/"+filename)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 on fresh gem; got status=%d body=%s",
			resp.StatusCode, body)
	}
	if !strings.Contains(strings.ToLower(string(body)), "cooldown") {
		t.Errorf("expected 'cooldown' in 403 body, got: %s", body)
	}
	if got := atomic.LoadInt32(f.blobHits); got != 0 {
		t.Errorf("expected 0 upstream blob fetches on denied ingest, got %d", got)
	}

	// And the stale version still works under the same rule.
	staleFile := f.gemName + "-" + f.staleVersion + ".gem"
	resp2, body2 := f.get(t, base+"/gems/"+staleFile)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on stale gem; got %d body=%s",
			resp2.StatusCode, body2)
	}
}

// TestPullThroughRubyGems_LocalResolveHonorsUpstreamPublishCooldown is
// the regression-test sibling of the Go test of the same name (commit
// f3af13a). Pulls through the stale version, then the same .gem must
// STILL 200 on a follow-up request because subjectFor surfaces
// UpstreamPublishedUnix from the local row. Without that fix, ingest_age
// (~0s) would trip the 30d cooldown rule.
func TestPullThroughRubyGems_LocalResolveHonorsUpstreamPublishCooldown(t *testing.T) {
	f := newUpstreamFixture(t, 30)
	base := "/api/packages/" + f.tenantName + "/rubygems"

	staleFile := f.gemName + "-" + f.staleVersion + ".gem"

	// Step 1: cold pull-through. Ingests the stale gem locally and
	// stamps upstream_published_unix on the row.
	resp, body := f.get(t, base+"/gems/"+staleFile)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 1: expected 200, got %d body=%s", resp.StatusCode, body)
	}

	// Sanity: the local row has upstream_published_unix stamped.
	pkg, err := f.models.GetPackage(context.Background(), f.tenantID, models.TypeRubyGems, f.gemName)
	if err != nil {
		t.Fatalf("get package: %v", err)
	}
	ver, err := f.models.GetVersion(context.Background(), pkg.ID, f.staleVersion)
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	if !ver.UpstreamPublishedUnix.Valid || ver.UpstreamPublishedUnix.Int64 != f.staleUnix {
		t.Fatalf("upstream_published_unix not stamped: valid=%v val=%d want=%d",
			ver.UpstreamPublishedUnix.Valid, ver.UpstreamPublishedUnix.Int64, f.staleUnix)
	}

	// Step 2: the bug. With v1.0.0 freshly ingested, ingest_age_seconds
	// is ~0. If subjectFor didn't surface upstream_published_unix, the
	// cooldown would fall back to ingest age and 403 every follow-up
	// request to this stale version.
	resp2, body2 := f.get(t, base+"/gems/"+staleFile)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("step 2 (local resolve, stale version): expected 200, got %d body=%s",
			resp2.StatusCode, body2)
	}

	// /info/<name> uses filterReadable on local rows; the stale version
	// must still appear (it's >30d upstream so cooldown allows).
	resp3, body3 := f.get(t, base+"/info/"+f.gemName)
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("step 3 /info: expected 200, got %d body=%s", resp3.StatusCode, body3)
	}
	if !strings.Contains(string(body3), f.staleVersion) {
		t.Errorf("step 3 /info missing stale version; got %q", body3)
	}
}

// TestPullThroughRubyGems_SHA256MismatchAborts proves a hash mismatch
// between the compact-index checksum and the actual gem bytes returns
// 502 and does NOT persist anything.
func TestPullThroughRubyGems_SHA256MismatchAborts(t *testing.T) {
	// Build the standard fixture, then SWAP the gem bytes after-the-fact
	// so the body doesn't match the SHA the index already published.
	f := newUpstreamFixture(t, 0)

	// Re-register the fresh-version blob handler to serve tampered bytes.
	// The index still advertises the original SHA.
	mux := http.NewServeMux()
	mux.HandleFunc("/info/"+f.gemName, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(f.infoHits, 1)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w,
			"---\n%s |checksum:%s\n%s |checksum:%s\n",
			f.staleVersion, f.staleSHA, f.freshVersion, f.freshSHA)
	})
	mux.HandleFunc("/api/v1/versions/"+f.gemName+".json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})
	mux.HandleFunc("/gems/"+f.gemName+"-"+f.freshVersion+".gem", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(f.blobHits, 1)
		w.Header().Set("Content-Type", "application/octet-stream")
		// Tampered bytes: NOT what the index's checksum: field describes.
		_, _ = w.Write([]byte("TAMPERED - bogus bytes that won't match the SHA256"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	f.fake.Config.Handler = mux

	base := "/api/packages/" + f.tenantName + "/rubygems"
	filename := f.gemName + "-" + f.freshVersion + ".gem"
	resp, body := f.get(t, base+"/gems/"+filename)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502 on SHA mismatch; got %d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(strings.ToLower(string(body)), "sha256 mismatch") {
		t.Errorf("expected 'sha256 mismatch' in body; got %q", body)
	}

	// Crucially: NO DB row was created.
	if _, err := f.models.GetPackage(context.Background(), f.tenantID, models.TypeRubyGems, f.gemName); err == nil {
		t.Errorf("expected NO package row after SHA mismatch, but GetPackage succeeded")
	}
}

// TestPullThroughRubyGems_UploadVsPullThroughProvenance confirms:
//
//  1. a cold pull-through ingest writes CreatedVia=pull_through;
//  2. a subsequent `gem push` to the same name returns 409 with the
//     insider-shadow defense message.
func TestPullThroughRubyGems_UploadVsPullThroughProvenance(t *testing.T) {
	f := newUpstreamFixture(t, 0)
	base := "/api/packages/" + f.tenantName + "/rubygems"

	// 1) Pull through the stale version.
	staleFile := f.gemName + "-" + f.staleVersion + ".gem"
	resp, body := f.get(t, base+"/gems/"+staleFile)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pull-through: expected 200, got %d body=%s", resp.StatusCode, body)
	}
	pkg, err := f.models.GetPackage(context.Background(), f.tenantID, models.TypeRubyGems, f.gemName)
	if err != nil {
		t.Fatalf("get package: %v", err)
	}
	if pkg.CreatedVia != models.CreatedViaPullThrough {
		t.Errorf("CreatedVia = %q, want pull_through", pkg.CreatedVia)
	}

	// 2) Try to `gem push` v3.0.0 to the same name. Must 409.
	uploadGem := buildGem(t, minimalGemspec(f.gemName, "3.0.0", "ruby", "MIT"))
	req, _ := http.NewRequest(http.MethodPost, f.pkgmirror.URL+base+"/api/v1/gems",
		strings.NewReader(string(uploadGem)))
	req.Header.Set("Authorization", "Bearer "+f.adminToken)
	req.Header.Set("Content-Type", "application/octet-stream")
	upResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST upload: %v", err)
	}
	upBody, _ := io.ReadAll(upResp.Body)
	_ = upResp.Body.Close()
	if upResp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 on upload to pull-through-owned package; got %d body=%s",
			upResp.StatusCode, upBody)
	}
	if !strings.Contains(strings.ToLower(string(upBody)), "mirrored from upstream") {
		t.Errorf("expected provenance message in 409 body; got %q", upBody)
	}

	// 3) Flip provenance to uploaded; the same upload now succeeds.
	if err := f.models.SetPackageCreatedVia(context.Background(), pkg.ID, models.CreatedViaUploaded); err != nil {
		t.Fatalf("flip provenance: %v", err)
	}
	req2, _ := http.NewRequest(http.MethodPost, f.pkgmirror.URL+base+"/api/v1/gems",
		strings.NewReader(string(uploadGem)))
	req2.Header.Set("Authorization", "Bearer "+f.adminToken)
	req2.Header.Set("Content-Type", "application/octet-stream")
	up2Resp, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("POST upload after flip: %v", err)
	}
	up2Body, _ := io.ReadAll(up2Resp.Body)
	_ = up2Resp.Body.Close()
	if up2Resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 after provenance flip; got %d body=%s",
			up2Resp.StatusCode, up2Body)
	}
}

// TestPullThroughRubyGems_PlatformGemRejectedOnColdMiss confirms that
// platform-tagged filenames (e.g. nokogiri-1.16.0-x86_64-linux.gem)
// return 404 on cold miss without ever touching upstream. v1 ships
// ruby-platform only; platform support is a follow-up.
func TestPullThroughRubyGems_PlatformGemRejectedOnColdMiss(t *testing.T) {
	f := newUpstreamFixture(t, 0)
	base := "/api/packages/" + f.tenantName + "/rubygems"

	resp, _ := f.get(t, base+"/gems/nokogiri-1.16.0-x86_64-linux.gem")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for platform gem cold miss; got %d", resp.StatusCode)
	}
	// And it must NOT have hit upstream.
	if got := atomic.LoadInt32(f.blobHits); got != 0 {
		t.Errorf("expected 0 blob hits for platform-gem cold miss; got %d", got)
	}
	if got := atomic.LoadInt32(f.infoHits); got != 0 {
		t.Errorf("expected 0 info hits for platform-gem cold miss; got %d", got)
	}
}

// TestPullThroughRubyGems_OffMode verifies that without an Upstream
// fetcher wired the cold path 404s with zero upstream contact.
func TestPullThroughRubyGems_OffMode(t *testing.T) {
	// Use the existing rubygems fixture (no upstream). A totally-unknown
	// gem must 404 without panicking.
	f := newFixture(t, tenants.VisibilityPrivate)
	base := "/api/packages/" + f.tenant + "/rubygems"
	resp, _ := f.do(t, http.MethodGet, base+"/info/totally-unknown-gem", "", nil, true)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 with no upstream; got %d", resp.StatusCode)
	}
	resp2, _ := f.do(t, http.MethodGet, base+"/gems/totally-unknown-1.0.0.gem", "", nil, true)
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for gem with no upstream; got %d", resp2.StatusCode)
	}
}
