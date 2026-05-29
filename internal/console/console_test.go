package console_test

import (
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astockwell/pkgmirror/internal/console"

	"github.com/gin-gonic/gin"
)

// fixtureConfig returns a Config valid for password-mode tests. Keys
// are deterministic so cookies survive across requests in a single
// test. SQLite + users/tenants stores aren't needed for PR 1.
func fixtureConfig(t *testing.T) console.Config {
	t.Helper()
	cfg := console.Config{
		Enabled:    true,
		AuthMode:   "password",
		SessionTTL: 0, // Config.Validate fills the default
		DarkModeDefault: "auto",
	}
	cfg.Session.AuthKey = mustHex(t, strings.Repeat("ab", 64)) // 64 bytes
	cfg.Session.EncKey = mustHex(t, strings.Repeat("cd", 32))  // 32 bytes
	cfg.CSRFKey = mustHex(t, strings.Repeat("ef", 32))         // 32 bytes
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	return cfg
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex decode fixture: %v", err)
	}
	return b
}

func newServer(t *testing.T, cfg console.Config) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, err := console.New(console.Deps{
		Config:     cfg,
		AppVersion: "test",
	})
	if err != nil {
		t.Fatalf("console.New: %v", err)
	}
	r := gin.New()
	if err := c.Register(r); err != nil {
		t.Fatalf("register: %v", err)
	}
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func TestPing_Renders200WithLayout(t *testing.T) {
	srv := newServer(t, fixtureConfig(t))

	resp, err := http.Get(srv.URL + "/console/_ping")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "pkgmirror console") {
		t.Errorf("expected page title in body; got %q", body)
	}
	if !strings.Contains(string(body), "<aside class=\"app-sidebar\"") {
		t.Errorf("expected sidebar partial; got %q", body)
	}
}

func TestPing_SetsSecurityHeaders(t *testing.T) {
	srv := newServer(t, fixtureConfig(t))

	resp, err := http.Get(srv.URL + "/console/_ping")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	checks := []struct {
		header, wantContains string
	}{
		{"Content-Security-Policy", "default-src 'self'"},
		{"Content-Security-Policy", "script-src 'self'"},
		{"Content-Security-Policy", "frame-ancestors 'none'"},
		{"X-Content-Type-Options", "nosniff"},
		{"X-Frame-Options", "DENY"},
		{"Referrer-Policy", "strict-origin-when-cross-origin"},
		{"Cache-Control", "no-store"},
	}
	for _, ck := range checks {
		got := resp.Header.Get(ck.header)
		if !strings.Contains(got, ck.wantContains) {
			t.Errorf("%s: want substring %q, got %q", ck.header, ck.wantContains, got)
		}
	}
}

func TestPing_SetsRequestID(t *testing.T) {
	srv := newServer(t, fixtureConfig(t))

	resp, err := http.Get(srv.URL + "/console/_ping")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	if got := resp.Header.Get("X-Request-ID"); got == "" {
		t.Error("expected X-Request-ID response header to be set")
	}
}

func TestPing_HonorsClientRequestID(t *testing.T) {
	srv := newServer(t, fixtureConfig(t))

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/console/_ping", nil)
	req.Header.Set("X-Request-ID", "client-supplied-id")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	if got := resp.Header.Get("X-Request-ID"); got != "client-supplied-id" {
		t.Errorf("expected echoed client request id, got %q", got)
	}
}

func TestStatic_ServesEmbeddedCSS(t *testing.T) {
	srv := newServer(t, fixtureConfig(t))

	resp, err := http.Get(srv.URL + "/console/static/css/console.css")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "--bg") {
		t.Errorf("expected theme token --bg in CSS body; first 200 bytes: %q", body[:min(200, len(body))])
	}
}

func TestConfig_RejectsBadAuthMode(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.AuthMode = "floob"
	if err := cfg.Validate(); err == nil {
		t.Error("expected validation error for unknown auth mode")
	}
}

func TestConfig_RequiresTrustedProxiesInProxyMode(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.AuthMode = "proxy-header"
	cfg.TrustedProxies = nil
	if err := cfg.Validate(); err == nil {
		t.Error("expected validation error: proxy-header mode requires TRUSTED_PROXIES")
	}
}

func TestConfig_RejectsBadSessionKeyLength(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Session.AuthKey = []byte("too short")
	if err := cfg.Validate(); err == nil {
		t.Error("expected validation error for short session auth key")
	}
}

func TestEnsureKeys_FailsInProductionWithoutKeys(t *testing.T) {
	cfg := console.Config{Enabled: true, AuthMode: "password"}
	// Don't populate keys; intentionally leave them missing.
	_ = cfg.Validate()
	_, err := cfg.EnsureKeys(false, deterministicRandom)
	if err == nil {
		t.Error("expected error when production-mode + missing keys")
	}
}

func TestEnsureKeys_GeneratesInDevMode(t *testing.T) {
	cfg := console.Config{Enabled: true, AuthMode: "password"}
	_ = cfg.Validate()
	generated, err := cfg.EnsureKeys(true, deterministicRandom)
	if err != nil {
		t.Fatalf("EnsureKeys: %v", err)
	}
	if !generated {
		t.Error("expected generated=true when no keys set in dev mode")
	}
	if len(cfg.Session.AuthKey) != 64 || len(cfg.Session.EncKey) != 32 || len(cfg.CSRFKey) != 32 {
		t.Errorf("post-generate key lengths wrong: auth=%d enc=%d csrf=%d",
			len(cfg.Session.AuthKey), len(cfg.Session.EncKey), len(cfg.CSRFKey))
	}
}

func TestEnsureKeys_AllowsOverrideInProduction(t *testing.T) {
	cfg := console.Config{Enabled: true, AuthMode: "password", AllowEphemeralKeys: true}
	_ = cfg.Validate()
	_, err := cfg.EnsureKeys(false, deterministicRandom)
	if err != nil {
		t.Errorf("AllowEphemeralKeys should let production boot generate keys; got %v", err)
	}
}

func TestDisabledConsoleSkipsRegistration(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Enabled = false
	c, err := console.New(console.Deps{Config: cfg, AppVersion: "test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r := gin.New()
	if err := c.Register(r); err != nil {
		t.Fatalf("Register: %v", err)
	}
	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/console/_ping")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 with console disabled, got %d", resp.StatusCode)
	}
}

func TestDevDir_LoadsTemplatesFromDisk(t *testing.T) {
	// Create a minimal dev-dir override and verify templates parse from
	// disk instead of the embedded FS. We just need the layout +
	// _ping + sidebar/flash/breadcrumb so the page renders.
	dir := t.TempDir()
	for path, content := range minimalDevTemplates() {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	// static/ subdir for the static handler
	if err := os.MkdirAll(filepath.Join(dir, "static", "css"), 0o755); err != nil {
		t.Fatalf("mkdir static: %v", err)
	}
	_ = os.WriteFile(filepath.Join(dir, "static", "css", "console.css"), []byte("/* dev */"), 0o644)

	cfg := fixtureConfig(t)
	cfg.DevDir = dir
	srv := newServer(t, cfg)

	resp, err := http.Get(srv.URL + "/console/_ping")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "DEV-DIR PING") {
		t.Errorf("expected dev-dir template to render, got %s", body)
	}
}

func minimalDevTemplates() map[string]string {
	return map[string]string{
		"layouts/base.tmpl": `{{ define "layouts/base" }}<html><body>{{ tmpl .Page .Body }}</body></html>{{ end }}`,
		"partials/sidebar.tmpl": `{{ define "partials/sidebar" }}{{ end }}`,
		"partials/flash.tmpl":   `{{ define "partials/flash" }}{{ end }}`,
		"partials/breadcrumb.tmpl": `{{ define "partials/breadcrumb" }}{{ end }}`,
		"pages/_ping.tmpl":      `{{ define "pages/_ping" }}DEV-DIR PING{{ end }}`,
	}
}

func deterministicRandom(n int) ([]byte, error) {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i & 0xff)
	}
	return b, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Sanity: keep gin import used (no behavioral assertion).
var _ = fmt.Sprintf
