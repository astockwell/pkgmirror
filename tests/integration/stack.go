//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/astockwell/pkgmirror/tests/blackbox/harness"
)

// integrationUserAgent identifies our traffic to upstream operators.
// If you find this in your registry's access logs and want us to
// stop, file an issue on github.com/astockwell/pkgmirror.
const integrationUserAgent = "pkgmirror-integration-tests/0.1 " +
	"(+https://github.com/astockwell/pkgmirror; ci weekly)"

// StartStack boots a pkgmirror stack configured for pull-through:
//
//   - PKGMIRROR_UPSTREAM_DEFAULT_MODE=cache_and_serve  (overrides
//     blackbox's off default)
//   - PKGMIRROR_UPSTREAM_USER_AGENT=<polite identifier>
//   - PKGMIRROR_UPSTREAM_FETCH_RPM_PER_TENANT=30  (conservative; the
//     suite makes ~10 requests total per run, so this is purely a
//     runaway-loop circuit breaker)
//
// The compiled-in hostname allowlist already covers pypi.org +
// files.pythonhosted.org; no env extension is needed for PyPI tests.
// Future per-format integration tests (npm: registry.npmjs.org;
// RubyGems: rubygems.org; etc.) get their hosts for free from the
// same compiled-in defaults table.
func StartStack(ctx context.Context, t *testing.T) *harness.Stack {
	t.Helper()
	return harness.StartWithOptions(ctx, t, harness.Options{
		ExtraEnv: map[string]string{
			"PKGMIRROR_UPSTREAM_DEFAULT_MODE":         "cache_and_serve",
			"PKGMIRROR_UPSTREAM_USER_AGENT":           integrationUserAgent,
			"PKGMIRROR_UPSTREAM_FETCH_RPM_PER_TENANT": "30",
		},
		AllowEgress: true,
	})
}
