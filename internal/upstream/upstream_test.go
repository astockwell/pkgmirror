package upstream

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ============================================================
// Allowlist + scheme gate
// ============================================================

func TestAllowlist_CompiledInDefaults(t *testing.T) {
	al := NewAllowlist(nil)
	mustParse := func(raw string) *url.URL {
		t.Helper()
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		return u
	}

	t.Run("permits PyPI canonical hosts", func(t *testing.T) {
		if !al.Permits(mustParse("https://pypi.org/simple/")) {
			t.Error("expected pypi.org to be allowlisted")
		}
		if !al.Permits(mustParse("https://files.pythonhosted.org/packages/x.whl")) {
			t.Error("expected files.pythonhosted.org to be allowlisted")
		}
	})

	t.Run("rejects arbitrary hosts", func(t *testing.T) {
		if al.Permits(mustParse("https://evil.example.com/")) {
			t.Error("expected evil.example.com NOT to be allowlisted")
		}
	})

	t.Run("case-insensitive on hostname", func(t *testing.T) {
		if !al.Permits(mustParse("https://PyPI.ORG/simple/")) {
			t.Error("expected case-insensitive matching")
		}
	})

	t.Run("strips port", func(t *testing.T) {
		if !al.Permits(mustParse("https://pypi.org:8443/simple/")) {
			t.Error("expected port to be ignored")
		}
	})

	t.Run("rejects empty host (relative URLs)", func(t *testing.T) {
		u, _ := url.Parse("/simple/x")
		if al.Permits(u) {
			t.Error("expected empty host to be rejected")
		}
	})
}

func TestAllowlist_EnvExtra(t *testing.T) {
	al := NewAllowlist([]string{"mirror.internal.example.com", " "})
	u, _ := url.Parse("https://mirror.internal.example.com/x")
	if !al.Permits(u) {
		t.Error("expected env-supplied host to be allowlisted")
	}
}

func TestCheckSchemeAndHost(t *testing.T) {
	al := NewAllowlist(nil)
	mk := func(raw string) *url.URL {
		u, _ := url.Parse(raw)
		return u
	}

	cases := []struct {
		name     string
		url      *url.URL
		plain    bool
		wantErr  bool
		wantKind error
	}{
		{name: "https allowed host", url: mk("https://pypi.org/x"), wantErr: false},
		{name: "http rejected by default", url: mk("http://pypi.org/x"), wantErr: true, wantKind: ErrUpstreamForbidden},
		{name: "http allowed when plaintext on", url: mk("http://pypi.org/x"), plain: true, wantErr: false},
		{name: "ftp rejected even when plaintext on", url: mk("ftp://pypi.org/x"), plain: true, wantErr: true, wantKind: ErrUpstreamForbidden},
		{name: "https rejected for non-allowlisted host", url: mk("https://evil.example.com/x"), wantErr: true, wantKind: ErrUpstreamForbidden},
		{name: "nil url rejected", url: nil, wantErr: true, wantKind: ErrUpstreamForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkSchemeAndHost(tc.url, al, tc.plain)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantKind != nil && !errors.Is(err, tc.wantKind) {
				t.Fatalf("error %v not matched by errors.Is(%v)", err, tc.wantKind)
			}
		})
	}
}

// ============================================================
// Private IP guard
// ============================================================

func TestPrivateIPGuard(t *testing.T) {
	g := NewPrivateIPGuard(false)
	cases := []struct {
		ip   string
		want bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"127.0.0.1", false},
		{"10.0.0.1", false},
		{"172.16.0.1", false},
		{"192.168.1.1", false},
		{"169.254.169.254", false}, // EC2 IMDS
		{"::1", false},
		{"fd00::1", false},
	}
	for _, tc := range cases {
		t.Run(tc.ip, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("parse %q", tc.ip)
			}
			if got := g.AllowsIP(ip); got != tc.want {
				t.Errorf("AllowsIP(%s) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}
}

func TestPrivateIPGuard_PermittedOverride(t *testing.T) {
	g := NewPrivateIPGuard(true)
	if !g.AllowsIP(net.ParseIP("127.0.0.1")) {
		t.Error("expected loopback to pass when guard is permitted")
	}
}

// ============================================================
// Defaults table
// ============================================================

func TestLookupDefault(t *testing.T) {
	d, ok := LookupDefault("pypi")
	if !ok {
		t.Fatal("pypi default missing")
	}
	if d.URL == "" || len(d.Hosts) == 0 {
		t.Error("pypi default incomplete")
	}
	if !d.PullThroughSupported {
		t.Error("pypi should be marked pull-through-supported (PR F)")
	}

	if _, ok := LookupDefault("generic"); ok {
		t.Error("generic should NOT have a default upstream")
	}
}

func TestLookupDefault_CaseInsensitive(t *testing.T) {
	if _, ok := LookupDefault("PyPI"); !ok {
		t.Error("expected case-insensitive lookup")
	}
}

// ============================================================
// Rate limiter
// ============================================================

func TestRateLimiter_AllowsBurstThenThrottles(t *testing.T) {
	r := newRateLimiter(60) // 60/min => 1/sec, capacity 60
	tenant := int64(1)
	allowed := 0
	for i := 0; i < 60; i++ {
		if r.Allow(tenant) {
			allowed++
		}
	}
	if allowed != 60 {
		t.Errorf("expected 60 initial allows, got %d", allowed)
	}
	if r.Allow(tenant) {
		t.Error("expected 61st call to be throttled")
	}
}

func TestRateLimiter_PerTenantIsolation(t *testing.T) {
	r := newRateLimiter(2)
	if !r.Allow(1) || !r.Allow(1) {
		t.Fatal("tenant 1 should get its 2 tokens")
	}
	if r.Allow(1) {
		t.Error("tenant 1 should be out")
	}
	if !r.Allow(2) {
		t.Error("tenant 2 should still have tokens")
	}
}

// ============================================================
// Metadata cache
// ============================================================

func TestMetadataCache_PutGet(t *testing.T) {
	c := newMetadataCache(1024)
	body := []byte("hello")
	c.put("k", body, "text/plain", `"etag"`, time.Now(), time.Minute)
	e, ok := c.get("k")
	if !ok {
		t.Fatal("missed after put")
	}
	if string(e.body) != "hello" || e.etag != `"etag"` {
		t.Errorf("round trip wrong: %+v", e)
	}
}

func TestMetadataCache_TTLExpiry(t *testing.T) {
	c := newMetadataCache(1024)
	c.put("k", []byte("hello"), "", "", time.Time{}, time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	if _, ok := c.get("k"); ok {
		t.Error("expected expired entry to miss")
	}
}

func TestMetadataCache_LRUEviction(t *testing.T) {
	c := newMetadataCache(20) // tiny budget
	c.put("a", []byte("0123456789"), "", "", time.Time{}, time.Minute) // 10 bytes
	c.put("b", []byte("0123456789"), "", "", time.Time{}, time.Minute) // 10 bytes, fits
	c.put("c", []byte("0123456789"), "", "", time.Time{}, time.Minute) // evicts a
	if _, ok := c.get("a"); ok {
		t.Error("expected a to be evicted")
	}
	if _, ok := c.get("b"); !ok {
		t.Error("expected b to remain")
	}
	if _, ok := c.get("c"); !ok {
		t.Error("expected c to be present")
	}
}

func TestMetadataCache_OversizedBypass(t *testing.T) {
	c := newMetadataCache(10)
	c.put("big", []byte("0123456789ABCDEF"), "", "", time.Time{}, time.Minute)
	if _, ok := c.get("big"); ok {
		t.Error("expected oversized put to be skipped")
	}
}

func TestMetadataCache_Disabled(t *testing.T) {
	c := newMetadataCache(0)
	c.put("k", []byte("x"), "", "", time.Time{}, time.Minute)
	if _, ok := c.get("k"); ok {
		t.Error("cache with maxBytes=0 should never hit")
	}
}

// ============================================================
// Config store + resolver
// ============================================================

// fakeStore is an in-memory ConfigStore.
type fakeStore struct {
	rows map[string]*PersistedConfig // key: "tenant|format"
	err  error
}

func (f *fakeStore) LookupTenantUpstream(_ context.Context, tenantID int64, format string) (*PersistedConfig, error) {
	if f.err != nil {
		return nil, f.err
	}
	if pc, ok := f.rows[key(tenantID, format)]; ok {
		return pc, nil
	}
	return nil, nil
}

func key(t int64, f string) string {
	return strings.ToLower(f) + "@" + strconv.FormatInt(t, 10)
}

func TestResolveConfig_FallsBackToDefaultWhenRowAbsent(t *testing.T) {
	store := &fakeStore{rows: map[string]*PersistedConfig{}}
	cfg, err := resolveConfig(context.Background(), store, ModeCacheAndServe, time.Minute, 1, "pypi")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.Mode != ModeCacheAndServe {
		t.Errorf("mode = %v, want cache_and_serve", cfg.Mode)
	}
	if cfg.UpstreamBaseURL == nil || cfg.UpstreamBaseURL.Host != "pypi.org" {
		t.Errorf("base url = %v, want pypi.org default", cfg.UpstreamBaseURL)
	}
	if cfg.MetadataTTL != time.Minute {
		t.Errorf("ttl = %v, want default", cfg.MetadataTTL)
	}
}

func TestResolveConfig_RowOverridesURLAndMode(t *testing.T) {
	store := &fakeStore{rows: map[string]*PersistedConfig{
		key(1, "pypi"): {
			Mode:           string(ModeCacheOnly),
			UpstreamURL:    sql.NullString{Valid: true, String: "https://mirror.internal.example.com/pypi"},
			MetadataTTLSec: 30,
		},
	}}
	t.Setenv("PKGMIRROR_UPSTREAM_ALLOWED_HOSTS", "mirror.internal.example.com")
	cfg, err := resolveConfig(context.Background(), store, ModeCacheAndServe, time.Minute, 1, "pypi")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.Mode != ModeCacheOnly {
		t.Errorf("mode = %v, want cache_only", cfg.Mode)
	}
	if cfg.UpstreamBaseURL.Host != "mirror.internal.example.com" {
		t.Errorf("base url not overridden: %v", cfg.UpstreamBaseURL)
	}
	if cfg.MetadataTTL != 30*time.Second {
		t.Errorf("ttl = %v, want 30s", cfg.MetadataTTL)
	}
}

func TestResolveConfig_FormatWithoutDefaultIsOff(t *testing.T) {
	cfg, err := resolveConfig(context.Background(), nil, ModeCacheAndServe, time.Minute, 1, "generic")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.Mode != ModeOff {
		t.Errorf("expected generic to resolve to off, got %v", cfg.Mode)
	}
}

func TestResolveConfig_UnsupportedAdapterIsOff(t *testing.T) {
	cfg, err := resolveConfig(context.Background(), nil, ModeCacheAndServe, time.Minute, 1, "npm")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// npm has a default URL but PullThroughSupported=false until PR L,
	// so the resolver must clamp to ModeOff.
	if cfg.Mode != ModeOff {
		t.Errorf("expected npm (no adapter yet) to resolve to off, got %v", cfg.Mode)
	}
}

// ============================================================
// Fetcher (end-to-end via httptest)
// ============================================================

// newTestFetcher builds a fetcher with the test server's host on the
// allowlist and the private-IP guard disabled (httptest binds to
// 127.0.0.1).
func newTestFetcher(t *testing.T, ts *httptest.Server, opts ...func(*Config)) (Fetcher, *fakeStore) {
	t.Helper()
	u, _ := url.Parse(ts.URL)
	cfg := DefaultConfig()
	cfg.AllowedHostsExtra = []string{u.Hostname()}
	cfg.AllowPrivateIPs = true // httptest binds 127.0.0.1
	cfg.AllowPlaintext = strings.HasPrefix(ts.URL, "http://")
	cfg.FetchTimeout = 2 * time.Second
	cfg.ConnectTimeout = 500 * time.Millisecond
	cfg.MaxBytesPerFetch = 1 << 20
	cfg.MetadataCacheMaxBytes = 64 * 1024
	cfg.DefaultMetadataTTL = time.Minute
	for _, opt := range opts {
		opt(&cfg)
	}
	store := &fakeStore{rows: map[string]*PersistedConfig{
		key(1, "pypi"): {
			Mode:           string(ModeCacheAndServe),
			UpstreamURL:    sql.NullString{Valid: true, String: ts.URL},
			MetadataTTLSec: 60,
		},
	}}
	return New(cfg, store), store
}

func TestFetcher_MetadataHappyPath(t *testing.T) {
	hits := int32(0)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("ETag", `"abc"`)
		_, _ = w.Write([]byte("<html>simple index</html>"))
	}))
	defer ts.Close()

	f, _ := newTestFetcher(t, ts)
	res, err := f.Fetch(context.Background(), Request{
		TenantID:     1,
		Format:       "pypi",
		Kind:         KindMetadata,
		UpstreamPath: "/simple/requests/",
		CanonicalKey: "pypi:1:requests:simple",
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "simple index") {
		t.Errorf("body = %q", body)
	}
	if res.ETag != `"abc"` {
		t.Errorf("etag = %q", res.ETag)
	}

	// Second fetch should hit the in-memory cache, not the upstream.
	res2, err := f.Fetch(context.Background(), Request{
		TenantID:     1,
		Format:       "pypi",
		Kind:         KindMetadata,
		UpstreamPath: "/simple/requests/",
		CanonicalKey: "pypi:1:requests:simple",
	})
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	defer res2.Body.Close()
	if !res2.FromCache {
		t.Error("expected second fetch to be served from cache")
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("upstream hit count = %d, want 1 (cache should have served the second)", hits)
	}
}

func TestFetcher_BlobHappyPath(t *testing.T) {
	payload := strings.Repeat("a", 1024)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "1024")
		_, _ = w.Write([]byte(payload))
	}))
	defer ts.Close()

	f, _ := newTestFetcher(t, ts)
	res, err := f.Fetch(context.Background(), Request{
		TenantID:     1,
		Format:       "pypi",
		Kind:         KindBlob,
		UpstreamPath: "/packages/x/requests-2.32.4-py3-none-any.whl",
		CanonicalKey: "pypi:1:requests:2.32.4:wheel",
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(body) != payload {
		t.Errorf("body length = %d, want %d", len(body), len(payload))
	}
}

func TestFetcher_404IsErrUpstreamNotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer ts.Close()
	f, _ := newTestFetcher(t, ts)
	_, err := f.Fetch(context.Background(), Request{
		TenantID:     1,
		Format:       "pypi",
		Kind:         KindMetadata,
		UpstreamPath: "/simple/nonexistent/",
		CanonicalKey: "pypi:1:nonexistent:simple",
	})
	if !errors.Is(err, ErrUpstreamNotFound) {
		t.Errorf("err = %v, want ErrUpstreamNotFound", err)
	}
}

func TestFetcher_ModeOffShortCircuits(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream should not be hit when mode=off")
	}))
	defer ts.Close()
	f, _ := newTestFetcher(t, ts, func(c *Config) {
		c.DefaultMode = ModeOff
	})
	// Clear the store so the row's mode doesn't override the default.
	f.(*fetcherImpl).store = &fakeStore{}
	_, err := f.Fetch(context.Background(), Request{
		TenantID:     1,
		Format:       "pypi",
		Kind:         KindMetadata,
		UpstreamPath: "/simple/x/",
		CanonicalKey: "k",
	})
	if !errors.Is(err, ErrUpstreamOff) {
		t.Errorf("err = %v, want ErrUpstreamOff", err)
	}
}

func TestFetcher_RejectsNonAllowlistedAbsolutePath(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be hit")
	}))
	defer ts.Close()
	f, _ := newTestFetcher(t, ts)
	_, err := f.Fetch(context.Background(), Request{
		TenantID:     1,
		Format:       "pypi",
		Kind:         KindBlob,
		UpstreamPath: "https://evil.example.com/x.whl",
		CanonicalKey: "k",
	})
	if !errors.Is(err, ErrUpstreamForbidden) {
		t.Errorf("err = %v, want ErrUpstreamForbidden", err)
	}
}

func TestFetcher_TooLarge(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte(strings.Repeat("z", 100)))
	}))
	defer ts.Close()
	f, _ := newTestFetcher(t, ts, func(c *Config) {
		c.MaxBytesPerFetch = 32 // smaller than payload
	})
	_, err := f.Fetch(context.Background(), Request{
		TenantID:     1,
		Format:       "pypi",
		Kind:         KindMetadata,
		UpstreamPath: "/big",
		CanonicalKey: "kbig",
	})
	if !errors.Is(err, ErrUpstreamTooLarge) {
		t.Errorf("err = %v, want ErrUpstreamTooLarge", err)
	}
}

func TestFetcher_BlobStreamingCap(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Don't advertise Content-Length so the size check happens
		// on the streaming side.
		w.Header().Set("Transfer-Encoding", "chunked")
		_, _ = w.Write([]byte(strings.Repeat("x", 200)))
	}))
	defer ts.Close()
	f, _ := newTestFetcher(t, ts, func(c *Config) {
		c.MaxBytesPerFetch = 64
	})
	res, err := f.Fetch(context.Background(), Request{
		TenantID:     1,
		Format:       "pypi",
		Kind:         KindBlob,
		UpstreamPath: "/blob",
		CanonicalKey: "kblob",
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer res.Body.Close()
	_, readErr := io.ReadAll(res.Body)
	if !errors.Is(readErr, ErrUpstreamTooLarge) {
		t.Errorf("ReadAll err = %v, want ErrUpstreamTooLarge", readErr)
	}
}

func TestFetcher_SingleFlightCollapsesConcurrentMisses(t *testing.T) {
	hits := int32(0)
	ready := make(chan struct{})
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		// Signal first arrival; wait so the second concurrent call has
		// time to coalesce on the same singleflight key.
		select {
		case <-ready:
		default:
			close(ready)
		}
		<-release
		_, _ = w.Write([]byte("ok"))
	}))
	defer ts.Close()

	// Disable the metadata cache so the second caller really does have
	// to take the slow path (without sf de-dup, we'd see 2 hits).
	f, _ := newTestFetcher(t, ts, func(c *Config) {
		c.MetadataCacheMaxBytes = 0
	})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		res, err := f.Fetch(context.Background(), Request{
			TenantID:     1,
			Format:       "pypi",
			Kind:         KindMetadata,
			UpstreamPath: "/simple/x/",
			CanonicalKey: "pypi:1:x:simple",
		})
		if err != nil {
			t.Errorf("goroutine A: %v", err)
			return
		}
		_, _ = io.ReadAll(res.Body)
		res.Body.Close()
	}()
	<-ready
	go func() {
		defer wg.Done()
		res, err := f.Fetch(context.Background(), Request{
			TenantID:     1,
			Format:       "pypi",
			Kind:         KindMetadata,
			UpstreamPath: "/simple/x/",
			CanonicalKey: "pypi:1:x:simple",
		})
		if err != nil {
			t.Errorf("goroutine B: %v", err)
			return
		}
		_, _ = io.ReadAll(res.Body)
		res.Body.Close()
	}()
	// Give the second goroutine a beat to join the singleflight group.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("upstream hits = %d, want 1 (singleflight should have collapsed)", got)
	}
}

func TestFetcher_RedirectToDisallowedHostBlocked(t *testing.T) {
	// Redirect to a host that isn't on the allowlist - even though
	// the initial URL was allowlisted.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example.com/x", http.StatusFound)
	}))
	defer ts.Close()
	f, _ := newTestFetcher(t, ts)
	_, err := f.Fetch(context.Background(), Request{
		TenantID:     1,
		Format:       "pypi",
		Kind:         KindBlob,
		UpstreamPath: "/jump",
		CanonicalKey: "k",
	})
	if err == nil {
		t.Fatal("expected redirect to disallowed host to error")
	}
	// CheckRedirect wraps in *url.Error; the underlying redirectBlockedError
	// satisfies errors.Is(ErrUpstreamForbidden).
	if !errors.Is(err, ErrUpstreamForbidden) {
		t.Errorf("err = %v, want chain containing ErrUpstreamForbidden", err)
	}
}

func TestFetcher_NotModifiedRevalidates(t *testing.T) {
	hits := int32(0)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte("body-v1"))
	}))
	defer ts.Close()

	// Use a very short TTL so the second call is forced to revalidate.
	f, _ := newTestFetcher(t, ts, func(c *Config) {
		c.DefaultMetadataTTL = time.Millisecond
	})
	// Match override TTL too.
	f.(*fetcherImpl).store = &fakeStore{rows: map[string]*PersistedConfig{
		key(1, "pypi"): {
			Mode:           string(ModeCacheAndServe),
			UpstreamURL:    sql.NullString{Valid: true, String: ts.URL},
			MetadataTTLSec: 0, // use cfg default = 1ms
		},
	}}

	res, err := f.Fetch(context.Background(), Request{
		TenantID:     1,
		Format:       "pypi",
		Kind:         KindMetadata,
		UpstreamPath: "/idx",
		CanonicalKey: "k-revalidate",
	})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	_, _ = io.ReadAll(res.Body)
	res.Body.Close()
	time.Sleep(5 * time.Millisecond) // expire

	res2, err := f.Fetch(context.Background(), Request{
		TenantID:        1,
		Format:          "pypi",
		Kind:            KindMetadata,
		UpstreamPath:    "/idx",
		CanonicalKey:    "k-revalidate",
		IfNoneMatch:     `"v1"`,
	})
	if err != nil {
		t.Fatalf("revalidate: %v", err)
	}
	defer res2.Body.Close()
	body2, _ := io.ReadAll(res2.Body)
	// We got 304 from upstream; the fetcher should have refreshed cache
	// and returned the body from cache. Either FromCache=true with body
	// or NotModified=true with empty body is acceptable.
	if !res2.FromCache && !res2.NotModified {
		t.Errorf("expected FromCache or NotModified to be set; got body=%q", body2)
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Errorf("upstream hits = %d, want 2 (initial + revalidation)", hits)
	}
}
