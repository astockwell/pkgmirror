//go:build blackbox

package generic_blackbox_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/astockwell/pkgmirror/tests/blackbox/harness"
)

func registryURL(s *harness.Stack, internal bool) string {
	base := s.HostBaseURL
	if internal {
		base = s.InternalBaseURL
	}
	return base + "/api/packages/" + harness.DefaultTenant + "/generic"
}

// upload writes content to <name>/<version>/<filename> with token auth.
// Used by tests that don't need to drive a real client.
func upload(t *testing.T, s *harness.Stack, name, version, filename string, content []byte) {
	t.Helper()
	url := registryURL(s, false) + "/" + name + "/" + version + "/" + filename
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(content))
	if s.AdminToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.AdminToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT %s: status=%d body=%s", url, resp.StatusCode, out)
	}
}

// TestGenericConformance_CurlRoundTrip drives a real curl client through
// upload + download + diff against the mirror. curl is the canonical
// "generic client" since the format has no ecosystem and any HTTP-aware
// tool that can PUT/GET arbitrary bytes is a legitimate consumer.
func TestGenericConformance_CurlRoundTrip(t *testing.T) {
	ctx := context.Background()
	stack := harness.Start(ctx, t)

	const (
		pkgName = "fixture"
		version = "1.0.0"
		fileNm  = "payload.bin"
	)
	payload := strings.Repeat("the quick brown fox jumps over the lazy dog. ", 32)

	url := registryURL(stack, true) + "/" + pkgName + "/" + version + "/" + fileNm

	client := stack.NewClient(ctx, t, harness.ClientSpec{
		Image:   "curlimages/curl:8.10.1",
		WorkDir: "/tmp", // curlimages/curl runs as a non-root user; /tmp is the
		// only world-writable dir guaranteed to exist.
		Files: map[string]string{
			"/tmp/payload.bin": payload,
		},
	})

	// PUT.
	out := client.MustExec(t, "curl", "-fsS",
		"-X", "PUT",
		"-H", "Authorization: Bearer "+stack.AdminToken,
		"--data-binary", "@/tmp/payload.bin",
		url,
	)
	_ = out

	// GET into a side-by-side file and diff against the original.
	_ = client.MustExec(t, "curl", "-fsS",
		"-H", "Authorization: Bearer "+stack.AdminToken,
		"-o", "/tmp/downloaded.bin",
		url,
	)
	diff := client.MustExec(t, "sh", "-c", "cmp /tmp/payload.bin /tmp/downloaded.bin && echo OK")
	if !strings.Contains(diff, "OK") {
		t.Fatalf("payload mismatch:\n%s", diff)
	}
}

// TestGenericConformance_MultipleFilesPerVersion is the multi-file-per-
// version semantic: a single (name, version) is allowed to carry many
// files (like PyPI sdist + wheels, or a release-bundle of arbitrary
// artifacts).
func TestGenericConformance_MultipleFilesPerVersion(t *testing.T) {
	ctx := context.Background()
	stack := harness.Start(ctx, t)

	const (
		pkgName = "release"
		version = "2.0.0"
	)
	for i, name := range []string{"linux-amd64.tar.gz", "linux-arm64.tar.gz", "darwin-arm64.tar.gz"} {
		upload(t, stack, pkgName, version, name, []byte(fmt.Sprintf("artifact-%d", i)))
	}
	// Each file is independently downloadable from the host.
	for i, name := range []string{"linux-amd64.tar.gz", "linux-arm64.tar.gz", "darwin-arm64.tar.gz"} {
		url := registryURL(stack, false) + "/" + pkgName + "/" + version + "/" + name
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer "+stack.AdminToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", name, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		want := fmt.Sprintf("artifact-%d", i)
		if resp.StatusCode != http.StatusOK || string(body) != want {
			t.Fatalf("GET %s: %d %q", name, resp.StatusCode, body)
		}
	}
}

// TestGenericConformance_DeleteVersion verifies the cascade behavior:
// DELETE /name/version (no filename) removes the version and every
// file attached to it.
func TestGenericConformance_DeleteVersion(t *testing.T) {
	ctx := context.Background()
	stack := harness.Start(ctx, t)

	const (
		pkgName = "delete-target"
		version = "0.1.0"
	)
	upload(t, stack, pkgName, version, "a.bin", []byte("a"))
	upload(t, stack, pkgName, version, "b.bin", []byte("b"))

	// DELETE the version.
	delURL := registryURL(stack, false) + "/" + pkgName + "/" + version
	req, _ := http.NewRequest(http.MethodDelete, delURL, nil)
	req.Header.Set("Authorization", "Bearer "+stack.AdminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE version: %d", resp.StatusCode)
	}

	// Both files now 404.
	for _, name := range []string{"a.bin", "b.bin"} {
		url := registryURL(stack, false) + "/" + pkgName + "/" + version + "/" + name
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer "+stack.AdminToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", name, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404 for %s after version delete, got %d", name, resp.StatusCode)
		}
	}
}

// TestGenericConformance_UnauthenticatedUploadRejected confirms the
// auth gate on writes.
func TestGenericConformance_UnauthenticatedUploadRejected(t *testing.T) {
	stack := harness.Start(context.Background(), t)
	url := registryURL(stack, false) + "/foo/1.0.0/x.bin"

	req, _ := http.NewRequest(http.MethodPut, url, strings.NewReader("x"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 401, got %d: %s", resp.StatusCode, out)
	}
}
