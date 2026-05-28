//go:build blackbox

package pypi_blackbox_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/astockwell/pkgmirror/tests/blackbox/harness"
)

// uploadDist posts a Python wheel or sdist to the mirror with token auth.
// filetype must be "sdist" or "bdist_wheel".
func uploadDist(t *testing.T, s *harness.Stack, name, version, filename, filetype string, content []byte) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	pyver := "source"
	if filetype == "bdist_wheel" {
		pyver = "py3"
	}
	for k, v := range map[string]string{
		":action":          "file_upload",
		"protocol_version": "1",
		"name":             name,
		"version":          version,
		"filetype":         filetype,
		"pyversion":        pyver,
		"metadata_version": "2.1",
		"summary":          "fixture for blackbox test",
		"requires_python":  ">=3.8",
	} {
		_ = w.WriteField(k, v)
	}
	fw, err := w.CreateFormFile("content", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	_ = w.Close()

	url := s.HostBaseURL + "/api/packages/" + harness.DefaultTenant + "/pypi/"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	if s.AdminToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.AdminToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("upload: status=%d body=%s", resp.StatusCode, out)
	}
}

// TestPyPIConformance proves wire-format compatibility with `pip`:
//   - upload an sdist via the legacy form-upload API
//   - have a real python:3.12 client install it via pip --index-url
//   - import the module and assert it returns the expected value
func TestPyPIConformance(t *testing.T) {
	ctx := context.Background()
	stack := harness.Start(ctx, t)

	const (
		pkg     = "foo"
		version = "1.0.0"
	)
	wheelName := fmt.Sprintf("%s-%s-py3-none-any.whl", pkg, version)
	uploadDist(t, stack, pkg, version, wheelName, "bdist_wheel", buildWheel(t, pkg, version))

	indexURL := stack.InternalBaseURL + "/api/packages/" + harness.DefaultTenant + "/pypi/simple/"

	client := stack.NewClient(ctx, t, harness.ClientSpec{
		Image:   "python:3.12-slim",
		WorkDir: "/work",
		Env: map[string]string{
			"PIP_INDEX_URL":                 indexURL,
			"PIP_TRUSTED_HOST":              "pkgmirror",
			"PIP_NO_CACHE_DIR":              "1",
			"PIP_DISABLE_PIP_VERSION_CHECK": "1",
		},
	})

	out := client.MustExec(t, "pip", "install", "-vvv",
		"--target=/work/site", pkg)
	if !strings.Contains(out, pkg) {
		t.Fatalf("pip install: unexpected output:\n%s", out)
	}

	out = client.MustExec(t, "python", "-c",
		"import sys; sys.path.insert(0, '/work/site'); import foo; print(foo.greet())")
	if !strings.Contains(out, "hello from foo") {
		t.Fatalf("expected 'hello from foo', got: %s", out)
	}
}

// TestPyPISimpleJSONConformance verifies the PEP 691 JSON API the
// `uv` / `pip resolve` clients use.
func TestPyPISimpleJSONConformance(t *testing.T) {
	stack := harness.Start(context.Background(), t)
	uploadDist(t, stack, "bar", "1.0.0", "bar-1.0.0-py3-none-any.whl", "bdist_wheel",
		buildWheel(t, "bar", "1.0.0"))
	uploadDist(t, stack, "bar", "2.0.0", "bar-2.0.0-py3-none-any.whl", "bdist_wheel",
		buildWheel(t, "bar", "2.0.0"))

	url := stack.HostBaseURL + "/api/packages/" + harness.DefaultTenant + "/pypi/simple/bar/"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Accept", "application/vnd.pypi.simple.v1+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	got := string(body)
	for _, want := range []string{`"name":"bar"`, `"bar-1.0.0-py3-none-any.whl"`, `"bar-2.0.0-py3-none-any.whl"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("response missing %q\nfull body:\n%s", want, got)
		}
	}
}

// TestPyPIUnauthenticatedUploadIsRejected confirms the auth gate.
func TestPyPIUnauthenticatedUploadIsRejected(t *testing.T) {
	stack := harness.Start(context.Background(), t)
	url := stack.HostBaseURL + "/api/packages/" + harness.DefaultTenant + "/pypi/"

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	_ = w.WriteField("name", "foo")
	_ = w.WriteField("version", "1.0.0")
	fw, _ := w.CreateFormFile("content", "foo-1.0.0.tar.gz")
	_, _ = fw.Write([]byte("x"))
	_ = w.Close()

	req, _ := http.NewRequest(http.MethodPost, url, &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 401, got %d: %s", resp.StatusCode, out)
	}
}
