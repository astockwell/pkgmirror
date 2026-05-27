//go:build blackbox

package harness

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

// UploadBytes PUTs body to <pkgmirror>/<path> via the host-side URL using
// the stack's admin Bearer token, failing the test on a non-2xx response.
func UploadBytes(t *testing.T, s *Stack, path string, contentType string, body []byte) {
	t.Helper()
	url := s.HostBaseURL + path
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build upload request: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if s.AdminToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.AdminToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload %s: %v", url, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("upload %s: status=%d body=%s", url, resp.StatusCode, respBody)
	}
}

// MustGetHost performs a GET against the host URL and returns the body on
// 2xx, or fails the test.
func MustGetHost(t *testing.T, s *Stack, path string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.HostBaseURL+path, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("GET %s: status=%d body=%s", path, resp.StatusCode, body)
	}
	return body
}

// PrintLogs forces a dump of the pkgmirror container's logs to the test log
// (useful when a test is investigating mirror-side behavior even on
// success).
func PrintLogs(t *testing.T, s *Stack, label string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rc, err := s.Container.Logs(ctx)
	if err != nil {
		t.Logf("[%s] logs error: %v", label, err)
		return
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	t.Logf("[%s pkgmirror logs]\n%s", label, body)
}

// ensure compile-time use of fmt to avoid "imported and not used" if the
// helper set gets trimmed in the future.
var _ = fmt.Sprintf
