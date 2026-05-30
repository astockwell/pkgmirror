//go:build integration

package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// RequireReachable issues a short-timeout HEAD against probeURL and
// calls t.Skip if the request fails or returns 5xx. This distinguishes
// "the upstream is down" (skip, do not fail) from "our code regressed"
// (fail loudly).
//
// Use this at the top of each integration test that depends on a
// specific public registry, e.g.:
//
//	func TestPyPIPullThrough_PipInstall(t *testing.T) {
//	    integration.RequireReachable(t, "https://pypi.org/simple/pip/")
//	    ...
//	}
//
// The probeURL should be a known-good, tiny, stable resource on the
// upstream (typically the simple-index page for a long-standing
// package). HEAD is preferred for cache-friendliness; on 405 (some
// registries reject HEAD) we fall back to GET.
func RequireReachable(t *testing.T, probeURL string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	hostLabel := probeURL
	if i := strings.Index(probeURL, "://"); i >= 0 {
		hostLabel = probeURL[i+3:]
	}
	if i := strings.Index(hostLabel, "/"); i >= 0 {
		hostLabel = hostLabel[:i]
	}

	client := &http.Client{Timeout: 10 * time.Second}

	headReq, _ := http.NewRequestWithContext(ctx, http.MethodHead, probeURL, nil)
	headReq.Header.Set("User-Agent", integrationUserAgent)
	resp, err := client.Do(headReq)
	if err == nil && resp.StatusCode == http.StatusMethodNotAllowed {
		// Retry as GET; some indexes don't implement HEAD.
		_ = resp.Body.Close()
		getReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
		getReq.Header.Set("User-Agent", integrationUserAgent)
		resp, err = client.Do(getReq)
	}
	if err != nil {
		t.Skipf("integration: %s unreachable (%v); skipping - cannot tell whether our code regressed", hostLabel, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		t.Skipf("integration: %s returned %d; skipping - upstream issue, not our regression", hostLabel, resp.StatusCode)
		return
	}
	// Anything 2xx/3xx/4xx (other than 5xx) means upstream is alive.
	// A 404 on our specific probe still proves the registry as a
	// whole is up, which is all we need to differentiate the failure
	// modes.
}
