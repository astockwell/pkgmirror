//go:build blackbox

package npm_blackbox_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/astockwell/pkgmirror/tests/blackbox/harness"
)

// registryURL returns the npm registry URL for the (public) default tenant.
// The harness boots pkgmirror with PKGMIRROR_DEFAULT_TENANT_VISIBILITY=public
// so reads work anonymously; writes still need a token.
func registryURL(s *harness.Stack, internal bool) string {
	base := s.HostBaseURL
	if internal {
		base = s.InternalBaseURL
	}
	// npm requires a trailing slash on the registry URL.
	return base + "/api/packages/" + harness.DefaultTenant + "/npm/"
}

// buildNpmTarball constructs a gzipped tar conforming to the standard
// npm pack layout: every file is rooted under "package/".
func buildNpmTarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		hdr := &tar.Header{
			Name:    "package/" + name,
			Mode:    0o644,
			Size:    int64(len(body)),
			ModTime: time.Unix(0, 0),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %q: %v", name, err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("tar write %q: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return gzBuf.Bytes()
}

// integritySHA512 returns an SRI integrity string for the given bytes,
// matching what `npm publish` would set in the publish document.
func integritySHA512(b []byte) string {
	sum := sha512.Sum512(b)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

// buildPublishDoc constructs the JSON document `npm publish` sends. Going
// direct lets us upload from the host without needing node on the host.
func buildPublishDoc(t *testing.T, name, version, license, description string, tarball []byte) []byte {
	t.Helper()
	filename := strings.ToLower(unscope(name) + "-" + version + ".tgz")
	doc := map[string]any{
		"_id":         name,
		"name":        name,
		"description": description,
		"dist-tags":   map[string]string{"latest": version},
		"versions": map[string]any{
			version: map[string]any{
				"_id":         name + "@" + version,
				"name":        name,
				"version":     version,
				"description": description,
				"license":     license,
				"main":        "index.js",
				"dist": map[string]any{
					"integrity": integritySHA512(tarball),
					"shasum":    "ignored",
					"tarball":   "ignored",
				},
			},
		},
		"_attachments": map[string]any{
			filename: map[string]any{
				"content_type": "application/octet-stream",
				"data":         base64.StdEncoding.EncodeToString(tarball),
				"length":       len(tarball),
			},
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal publish doc: %v", err)
	}
	return b
}

func unscope(name string) string {
	if i := strings.Index(name, "/"); i >= 0 {
		return name[i+1:]
	}
	return name
}

// publish posts the npm publish JSON document to the mirror with token
// auth. The path is the package name (URL-escaping scoped names as `@scope/name`).
func publish(t *testing.T, s *harness.Stack, name string, body []byte) {
	t.Helper()
	url := registryURL(s, false) + name
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if s.AdminToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.AdminToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("publish %s: %v", name, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("publish %s: status=%d body=%s", name, resp.StatusCode, out)
	}
}

// TestNpmConformance_Install proves wire-format compatibility with `npm`:
//   - publish an unscoped package via the publish JSON shape
//   - have node:22-bookworm install it via `npm install`
//   - require the module and assert it returns the expected value
func TestNpmConformance_Install(t *testing.T) {
	ctx := context.Background()
	stack := harness.Start(ctx, t)

	const (
		pkgName = "foo"
		version = "1.0.0"
	)
	tarball := buildNpmTarball(t, map[string]string{
		"package.json": fmt.Sprintf(`{
  "name": %q,
  "version": %q,
  "main": "index.js",
  "license": "MIT"
}`, pkgName, version),
		"index.js": `module.exports.greet = function() { return "hello from foo via npm"; };`,
	})
	publish(t, stack, pkgName, buildPublishDoc(t, pkgName, version, "MIT", "fixture for blackbox", tarball))

	// Set up the consumer container. .npmrc lives in /root because npm
	// expands "~" via $HOME, which is /root in the node images.
	npmrc := fmt.Sprintf("registry=%s\n//%s/api/packages/%s/npm/:_authToken=%s\n",
		registryURL(stack, true),
		// strip scheme so the auth-key form matches npm's expectation:
		strings.TrimPrefix(stack.InternalBaseURL, "http://"),
		harness.DefaultTenant,
		stack.AdminToken,
	)
	consumerPkgJSON := `{"name":"consumer","version":"0.0.0","private":true}`

	client := stack.NewClient(ctx, t, harness.ClientSpec{
		Image:   "node:22-bookworm",
		WorkDir: "/work",
		Env: map[string]string{
			"npm_config_cache":    "/work/.npm-cache",
			"npm_config_progress": "false",
		},
		Files: map[string]string{
			"/root/.npmrc":      npmrc,
			"/work/package.json": consumerPkgJSON,
		},
	})

	out := client.MustExec(t, "npm", "install", "--no-audit", "--no-fund", pkgName+"@"+version)
	if !strings.Contains(out, "added 1 package") && !strings.Contains(out, pkgName) {
		t.Fatalf("npm install: unexpected output:\n%s", out)
	}

	out = client.MustExec(t, "node", "-e", "console.log(require('foo').greet())")
	if !strings.Contains(out, "hello from foo via npm") {
		t.Fatalf("require: unexpected output:\n%s", out)
	}
}

// TestNpmConformance_Scoped exercises the scoped-package routing
// (`@scope/name`) used by every modern published package.
func TestNpmConformance_Scoped(t *testing.T) {
	ctx := context.Background()
	stack := harness.Start(ctx, t)

	const (
		pkgName = "@acme/widget"
		version = "0.2.0"
	)
	tarball := buildNpmTarball(t, map[string]string{
		"package.json": fmt.Sprintf(`{
  "name": %q,
  "version": %q,
  "main": "index.js",
  "license": "Apache-2.0"
}`, pkgName, version),
		"index.js": `module.exports.label = function() { return "scoped widget works"; };`,
	})
	publish(t, stack, pkgName, buildPublishDoc(t, pkgName, version, "Apache-2.0", "scoped fixture", tarball))

	npmrc := fmt.Sprintf("registry=%s\n//%s/api/packages/%s/npm/:_authToken=%s\n",
		registryURL(stack, true),
		strings.TrimPrefix(stack.InternalBaseURL, "http://"),
		harness.DefaultTenant,
		stack.AdminToken,
	)

	client := stack.NewClient(ctx, t, harness.ClientSpec{
		Image:   "node:22-bookworm",
		WorkDir: "/work",
		Env: map[string]string{
			"npm_config_cache":    "/work/.npm-cache",
			"npm_config_progress": "false",
		},
		Files: map[string]string{
			"/root/.npmrc":      npmrc,
			"/work/package.json": `{"name":"consumer","version":"0.0.0","private":true}`,
		},
	})

	_ = client.MustExec(t, "npm", "install", "--no-audit", "--no-fund", pkgName+"@"+version)
	out := client.MustExec(t, "node", "-e", "console.log(require('@acme/widget').label())")
	if !strings.Contains(out, "scoped widget works") {
		t.Fatalf("require @acme/widget: %s", out)
	}
}

// TestNpmConformance_UnauthenticatedPublishRejected confirms the auth gate
// on the npm publish endpoint.
func TestNpmConformance_UnauthenticatedPublishRejected(t *testing.T) {
	stack := harness.Start(context.Background(), t)
	url := registryURL(stack, false) + "foo"

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, url, strings.NewReader(`{"name":"foo"}`))
	req.Header.Set("Content-Type", "application/json")
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

// TestNpmConformance_PackumentJSON verifies the packument metadata document
// served at GET /:name is in the npm-compatible shape (dist.tarball URLs,
// versions map keyed by version string, dist-tags, etc.).
func TestNpmConformance_PackumentJSON(t *testing.T) {
	stack := harness.Start(context.Background(), t)
	tarball := buildNpmTarball(t, map[string]string{
		"package.json": `{"name":"bar","version":"1.0.0","main":"index.js","license":"MIT"}`,
		"index.js":     "module.exports = 1;",
	})
	publish(t, stack, "bar", buildPublishDoc(t, "bar", "1.0.0", "MIT", "metadata fixture", tarball))

	url := registryURL(stack, false) + "bar"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	var meta struct {
		Name     string            `json:"name"`
		DistTags map[string]string `json:"dist-tags"`
		Versions map[string]struct {
			Version string `json:"version"`
			Dist    struct {
				Tarball   string `json:"tarball"`
				Integrity string `json:"integrity"`
			} `json:"dist"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		t.Fatalf("decode packument: %v", err)
	}
	if meta.Name != "bar" || meta.DistTags["latest"] != "1.0.0" {
		t.Fatalf("packument shape: %+v", meta)
	}
	v, ok := meta.Versions["1.0.0"]
	if !ok || v.Dist.Integrity == "" {
		t.Fatalf("versions: %+v", meta.Versions)
	}
	if !strings.Contains(v.Dist.Tarball, "/api/packages/"+harness.DefaultTenant+"/npm/bar/-/bar-1.0.0.tgz") {
		t.Fatalf("tarball url: %s", v.Dist.Tarball)
	}
}
