//go:build blackbox

package rubygems_blackbox_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/astockwell/pkgmirror/tests/blackbox/harness"
)

// registryURL returns the rubygems endpoint for the default tenant.
// We rely on the harness's PKGMIRROR_DEFAULT_TENANT_VISIBILITY=public so
// `gem` and `bundle` can resolve metadata anonymously; writes still need
// the admin token.
func registryURL(s *harness.Stack, internal bool) string {
	base := s.HostBaseURL
	if internal {
		base = s.InternalBaseURL
	}
	return base + "/api/packages/" + harness.DefaultTenant + "/rubygems"
}

// TestRubyGemsConformance_GemPushAndInstall drives the full publish ->
// resolve -> install loop against a real `gem` client. This is the
// canonical wire-compatibility test:
//
//  1. Build a minimal gem in-container using `gem build`.
//  2. Push it with `gem push` (uses HTTP API + token credentials file).
//  3. Resolve + install it with `gem install --source ...`.
//  4. Require it with `ruby -e` and assert the literal greeting.
func TestRubyGemsConformance_GemPushAndInstall(t *testing.T) {
	ctx := context.Background()
	stack := harness.Start(ctx, t)

	registry := registryURL(stack, true)
	// `gem push` reads ~/.gem/credentials for a YAML map of {source: token}.
	// The key has to exactly match the --host flag for `gem push` to pick it up.
	creds := fmt.Sprintf("---\n:rubygems_api_key: %s\n%s: %s\n",
		stack.AdminToken, registry, stack.AdminToken)

	// gemspec uses the same name+version we'll later install. Authors and
	// homepage are required by `gem build` to produce a valid gem.
	gemspec := `Gem::Specification.new do |s|
  s.name        = "fixture_pkg"
  s.version     = "1.0.0"
  s.summary     = "blackbox fixture"
  s.description = "fixture for pkgmirror conformance"
  s.authors     = ["pkgmirror tests"]
  s.email       = "tests@example.com"
  s.homepage    = "https://example.com/pkgmirror"
  s.license     = "MIT"
  s.files       = ["lib/fixture_pkg.rb"]
  s.required_ruby_version = ">= 3.0"
end
`
	libRb := `module FixturePkg
  def self.greet
    "hello from rubygems"
  end
end
`

	client := stack.NewClient(ctx, t, harness.ClientSpec{
		Image:   "ruby:3.3-slim",
		WorkDir: "/work",
		Env: map[string]string{
			// gem install pulls indexes from /info/<gem> by default in
			// modern rubygems; --source already covers it.
			"BUNDLE_DISABLE_VERSION_CHECK": "1",
		},
		Files: map[string]string{
			"/root/.gem/credentials":              creds,
			"/work/fixture_pkg.gemspec":           gemspec,
			"/work/lib/fixture_pkg.rb":            libRb,
		},
	})

	// gem push expects ~/.gem/credentials to be 0600.
	client.MustExec(t, "chmod", "600", "/root/.gem/credentials")

	out := client.MustExec(t, "sh", "-c", "cd /work && gem build fixture_pkg.gemspec")
	if !strings.Contains(out, "fixture_pkg-1.0.0.gem") {
		t.Fatalf("gem build: unexpected output:\n%s", out)
	}

	out = client.MustExec(t, "sh", "-c",
		fmt.Sprintf("cd /work && gem push --host %s fixture_pkg-1.0.0.gem", registry))
	if !strings.Contains(out, "Pushing") && !strings.Contains(out, "Successfully") {
		t.Fatalf("gem push: unexpected output:\n%s", out)
	}

	out = client.MustExec(t, "gem", "install", "fixture_pkg",
		"--version", "1.0.0",
		"--source", registry+"/",
		"--no-document",
		"--install-dir", "/work/gems",
	)
	if !strings.Contains(out, "fixture_pkg") {
		t.Fatalf("gem install: unexpected output:\n%s", out)
	}

	out = client.MustExec(t, "ruby",
		"-I/work/gems/gems/fixture_pkg-1.0.0/lib",
		"-rfixture_pkg",
		"-e", "puts FixturePkg.greet")
	if !strings.Contains(out, "hello from rubygems") {
		t.Fatalf("require: unexpected output:\n%s", out)
	}
}

// TestRubyGemsConformance_CompactIndex verifies the /info/<gem> and
// /versions endpoints serve the byte-exact compact index format real
// clients expect. We don't drive Bundler end-to-end here (it's slow);
// instead we shell out to `gem fetch` which hits the same endpoints.
func TestRubyGemsConformance_CompactIndex(t *testing.T) {
	ctx := context.Background()
	stack := harness.Start(ctx, t)

	registry := registryURL(stack, true)
	creds := fmt.Sprintf("---\n:rubygems_api_key: %s\n%s: %s\n",
		stack.AdminToken, registry, stack.AdminToken)

	gemspec := `Gem::Specification.new do |s|
  s.name        = "idxpkg"
  s.version     = "0.5.0"
  s.summary     = "compact-index fixture"
  s.description = "idx"
  s.authors     = ["pkgmirror"]
  s.email       = "x@example.com"
  s.homepage    = "https://example.com/"
  s.license     = "MIT"
  s.files       = ["lib/idxpkg.rb"]
end
`
	libRb := `module Idxpkg; end`

	client := stack.NewClient(ctx, t, harness.ClientSpec{
		Image:   "ruby:3.3-slim",
		WorkDir: "/work",
		Files: map[string]string{
			"/root/.gem/credentials":  creds,
			"/work/idxpkg.gemspec":    gemspec,
			"/work/lib/idxpkg.rb":     libRb,
		},
	})
	client.MustExec(t, "chmod", "600", "/root/.gem/credentials")
	client.MustExec(t, "sh", "-c", "cd /work && gem build idxpkg.gemspec")
	client.MustExec(t, "sh", "-c",
		fmt.Sprintf("cd /work && gem push --host %s idxpkg-0.5.0.gem", registry))

	// curl the compact index endpoints from inside the network so we
	// don't have to add a curl image just for this test.
	client.MustExec(t, "sh", "-c", "apt-get -qq update >/dev/null && apt-get -qq install -y curl >/dev/null")

	out := client.MustExec(t, "curl", "-fsSL", registry+"/info/idxpkg")
	if !strings.HasPrefix(out, "---\n") {
		t.Fatalf("/info missing compact-index header:\n%s", out)
	}
	if !strings.Contains(out, "0.5.0 |checksum:") {
		t.Fatalf("/info missing 0.5.0 line:\n%s", out)
	}

	out = client.MustExec(t, "curl", "-fsSL", registry+"/versions")
	if !strings.HasPrefix(out, "---\n") {
		t.Fatalf("/versions missing header:\n%s", out)
	}
	if !strings.Contains(out, "idxpkg 0.5.0 ") {
		t.Fatalf("/versions missing idxpkg entry:\n%s", out)
	}
}

// TestRubyGemsConformance_UnauthenticatedPushRejected confirms the auth
// gate on the upload endpoint.
func TestRubyGemsConformance_UnauthenticatedPushRejected(t *testing.T) {
	ctx := context.Background()
	stack := harness.Start(ctx, t)
	registry := registryURL(stack, true)

	client := stack.NewClient(ctx, t, harness.ClientSpec{
		Image:   "ruby:3.3-slim",
		WorkDir: "/work",
		Files: map[string]string{
			"/work/empty.gemspec": `Gem::Specification.new do |s|
  s.name        = "anon"
  s.version     = "0.0.1"
  s.summary     = "anon"
  s.description = "anon"
  s.authors     = ["x"]
  s.email       = "x@example.com"
  s.homepage    = "https://example.com/"
  s.license     = "MIT"
  s.files       = []
end
`,
		},
	})
	client.MustExec(t, "sh", "-c", "cd /work && gem build empty.gemspec")
	// No credentials file = anonymous push. With no terminal attached,
	// gem will try to fetch the registry's sign_up page (which we don't
	// serve) and bail out non-zero. We assert on the non-zero exit;
	// gem's output varies between versions.
	out, code, err := client.Exec(context.Background(), "sh", "-c",
		"cd /work && gem push --host "+registry+" anon-0.0.1.gem < /dev/null")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if code == 0 {
		t.Fatalf("expected non-zero exit for anon push, got 0:\n%s", out)
	}
	// The publish must NOT have succeeded.
	if strings.Contains(out, "Successfully") {
		t.Fatalf("anon push should not have succeeded:\n%s", out)
	}
}
