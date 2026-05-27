//go:build blackbox

package goproxy_blackbox_test

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

	"archive/zip"
)

// buildModuleZip produces a minimal Go module zip per the Go module zip
// layout: every file path is prefixed with "<module>@<version>/".
func buildModuleZip(t *testing.T, module, version, goMod string, extra map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	prefix := fmt.Sprintf("%s@%s/", module, version)
	write := func(name, contents string) {
		f, err := w.Create(prefix + name)
		if err != nil {
			t.Fatalf("zip create %q: %v", name, err)
		}
		if _, err := io.WriteString(f, contents); err != nil {
			t.Fatalf("zip write %q: %v", name, err)
		}
	}
	write("go.mod", goMod)
	for name, body := range extra {
		write(name, body)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

// proxyURL returns the GOPROXY URL pointing at pkgmirror's go endpoint
// for the (public) default tenant. The harness sets
// PKGMIRROR_DEFAULT_TENANT_VISIBILITY=public so anonymous reads work; see
// docs/blackbox-testing.md and DECISIONS.md for the rationale.
func proxyURL(s *harness.Stack) string {
	return s.InternalBaseURL + "/api/packages/" + harness.DefaultTenant + "/go"
}

// TestGoProxyConformance proves wire-format compatibility against the real
// go toolchain (golang:1.22-bookworm), end to end.
func TestGoProxyConformance(t *testing.T) {
	ctx := context.Background()
	stack := harness.Start(ctx, t)

	const (
		module  = "example.com/foo"
		version = "v1.2.3"
	)
	goMod := "module example.com/foo\n\ngo 1.22\n"
	zipBytes := buildModuleZip(t, module, version, goMod, map[string]string{
		"foo.go": "package foo\n\nconst Name = \"example.com/foo\"\n\nfunc Greet() string { return \"hello from \" + Name }\n",
	})

	harness.UploadBytes(t, stack,
		fmt.Sprintf("/api/packages/%s/go/upload", harness.DefaultTenant),
		"application/zip", zipBytes)

	client := stack.NewClient(ctx, t, harness.ClientSpec{
		Image:   "golang:1.22-bookworm",
		WorkDir: "/work",
		Env: map[string]string{
			"GOPROXY":    proxyURL(stack),
			"GOSUMDB":    "off",
			"GOFLAGS":    "-mod=mod",
			"GOMODCACHE": "/work/.gomodcache",
			"GOCACHE":    "/work/.gocache",
		},
		Files: map[string]string{
			"/work/go.mod": "module example.com/consumer\n\ngo 1.22\n\nrequire example.com/foo v1.2.3\n",
			"/work/main.go": `package main

import (
    "fmt"
    foo "example.com/foo"
)

func main() {
    fmt.Println(foo.Greet())
}
`,
		},
	})

	out := client.MustExec(t, "go", "mod", "download", "-x", module)
	for _, expect := range []string{
		fmt.Sprintf("/api/packages/%s/go/example.com/foo/@v/v1.2.3.info", harness.DefaultTenant),
		fmt.Sprintf("/api/packages/%s/go/example.com/foo/@v/v1.2.3.mod", harness.DefaultTenant),
		fmt.Sprintf("/api/packages/%s/go/example.com/foo/@v/v1.2.3.zip", harness.DefaultTenant),
	} {
		if !strings.Contains(out, expect) {
			t.Fatalf("expected `go mod download -x` to fetch %q\nfull output:\n%s", expect, out)
		}
	}

	out = client.MustExec(t, "go", "list", "-m", module)
	if !strings.Contains(out, "example.com/foo v1.2.3") {
		t.Fatalf("go list -m: %q want example.com/foo v1.2.3", strings.TrimSpace(out))
	}

	out = client.MustExec(t, "go", "build", "-o", "/work/consumer", "./...")
	t.Logf("go build:\n%s", out)

	out = client.MustExec(t, "/work/consumer")
	if !strings.Contains(out, "hello from example.com/foo") {
		t.Fatalf("consumer output: %q", strings.TrimSpace(out))
	}
}

// TestGoProxyMultipleVersions verifies `go list -m -versions` ordering.
func TestGoProxyMultipleVersions(t *testing.T) {
	ctx := context.Background()
	stack := harness.Start(ctx, t)

	const module = "example.com/bar"
	for _, v := range []string{"v0.1.0", "v0.2.0", "v1.0.0"} {
		z := buildModuleZip(t, module, v, "module example.com/bar\n\ngo 1.22\n",
			map[string]string{"bar.go": "package bar\n"})
		harness.UploadBytes(t, stack,
			fmt.Sprintf("/api/packages/%s/go/upload", harness.DefaultTenant),
			"application/zip", z)
	}

	client := stack.NewClient(ctx, t, harness.ClientSpec{
		Image:   "golang:1.22-bookworm",
		WorkDir: "/work",
		Env: map[string]string{
			"GOPROXY":    proxyURL(stack),
			"GOSUMDB":    "off",
			"GOFLAGS":    "-mod=mod",
			"GOMODCACHE": "/work/.gomodcache",
			"GOCACHE":    "/work/.gocache",
		},
		Files: map[string]string{
			"/work/go.mod": "module example.com/probe\n\ngo 1.22\n",
		},
	})

	out := client.MustExec(t, "go", "list", "-m", "-versions", module)
	fields := strings.Fields(out)
	if len(fields) < 4 || fields[0] != module {
		t.Fatalf("go list -m -versions: unexpected output %q", strings.TrimSpace(out))
	}
	got := strings.Join(fields[1:], " ")
	want := "v0.1.0 v0.2.0 v1.0.0"
	if got != want {
		t.Fatalf("versions order: got %q want %q", got, want)
	}
}

// TestUnauthenticatedUploadIsRejected confirms the auth gate end-to-end:
// uploads to the (public-read) default tenant still require a write token.
func TestUnauthenticatedUploadIsRejected(t *testing.T) {
	stack := harness.Start(context.Background(), t)
	z := buildModuleZip(t, "example.com/secret", "v0.0.1",
		"module example.com/secret\n\ngo 1.22\n", nil)
	url := stack.HostBaseURL + "/api/packages/" + harness.DefaultTenant + "/go/upload"

	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(z))
	req.Header.Set("Content-Type", "application/zip")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 401, got %d: %s", resp.StatusCode, body)
	}
}
