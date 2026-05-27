//go:build blackbox

package goproxy_blackbox_test

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/astockwell/pkgmirror/tests/blackbox/harness"
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

// TestGoProxyConformance brings up a real pkgmirror in a container, uploads
// a Go module zip, and then asks an official `golang:1.22-bookworm`
// container to fetch that module via `GOPROXY` and build against it.
//
// This proves wire-format compatibility: any byte-level deviation in our
// .info / .mod / .zip responses will cause `go` to refuse the download or
// fail the build.
func TestGoProxyConformance(t *testing.T) {
	ctx := context.Background()
	stack := harness.Start(ctx, t)

	const (
		module  = "example.com/foo"
		version = "v1.2.3"
	)
	goMod := "module example.com/foo\n\ngo 1.22\n"
	zipBytes := buildModuleZip(t, module, version, goMod, map[string]string{
		"foo.go": "package foo\n\n// Greet returns a friendly message.\nfunc Greet() string { return \"hello from \" + Name }\n\nconst Name = \"example.com/foo\"\n",
	})

	// --- populate the mirror ---
	harness.UploadBytes(t, stack, "/api/packages/go/upload", "application/zip", zipBytes)

	// --- drive the real go toolchain ---
	client := stack.NewClient(ctx, t, harness.ClientSpec{
		Image:   "golang:1.22-bookworm",
		WorkDir: "/work",
		Env: map[string]string{
			"GOPROXY":    stack.InternalBaseURL + "/api/packages/go",
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

	// 1. The proxy is reachable from inside the client container.
	out := client.MustExec(t, "sh", "-c",
		`apt-get update >/dev/null 2>&1 || true; `+
			`command -v curl >/dev/null || apt-get install -y curl >/dev/null 2>&1 || true; `+
			`curl -sSf `+stack.InternalBaseURL+`/-/healthz`)
	if !strings.Contains(out, "ok") {
		t.Fatalf("healthz from inside client container: %q", out)
	}

	// 2. `go mod download` succeeds and pulls all three protocol files.
	out = client.MustExec(t, "go", "mod", "download", "-x", module)
	for _, expect := range []string{
		"/api/packages/go/example.com/foo/@v/v1.2.3.info",
		"/api/packages/go/example.com/foo/@v/v1.2.3.mod",
		"/api/packages/go/example.com/foo/@v/v1.2.3.zip",
	} {
		if !strings.Contains(out, expect) {
			t.Fatalf("expected `go mod download -x` to fetch %q\nfull output:\n%s", expect, out)
		}
	}

	// 3. `go list -m` resolves to the version we uploaded.
	out = client.MustExec(t, "go", "list", "-m", module)
	if !strings.Contains(out, "example.com/foo v1.2.3") {
		t.Fatalf("go list -m: %q want %q", strings.TrimSpace(out), "example.com/foo v1.2.3")
	}

	// 4. Code that imports the module compiles cleanly.
	out = client.MustExec(t, "go", "build", "-o", "/work/consumer", "./...")
	t.Logf("go build:\n%s", out)

	// 5. And runs, producing the expected output.
	out = client.MustExec(t, "/work/consumer")
	if !strings.Contains(out, "hello from example.com/foo") {
		t.Fatalf("consumer output: %q", strings.TrimSpace(out))
	}
}

// TestGoProxyMultipleVersions verifies the `@v/list` ordering and `@latest`
// resolution match what the go client expects when multiple versions exist.
func TestGoProxyMultipleVersions(t *testing.T) {
	ctx := context.Background()
	stack := harness.Start(ctx, t)

	const module = "example.com/bar"
	for _, v := range []string{"v0.1.0", "v0.2.0", "v1.0.0"} {
		z := buildModuleZip(t, module, v, "module example.com/bar\n\ngo 1.22\n",
			map[string]string{"bar.go": "package bar\n"})
		harness.UploadBytes(t, stack, "/api/packages/go/upload", "application/zip", z)
	}

	client := stack.NewClient(ctx, t, harness.ClientSpec{
		Image:   "golang:1.22-bookworm",
		WorkDir: "/work",
		Env: map[string]string{
			"GOPROXY":    stack.InternalBaseURL + "/api/packages/go",
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
	// Output looks like: "example.com/bar v0.1.0 v0.2.0 v1.0.0"
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
