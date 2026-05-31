//go:build integration

// Package integration provides the wrapper around the blackbox harness
// that opts INTO real-network pull-through testing.
//
// Build tag: integration. Files here only compile when the suite is
// requested explicitly:
//
//	go test -tags=integration ./tests/integration/...
//
// The package is deliberately tiny - all container plumbing lives in
// tests/blackbox/harness (PR H widened that harness's build tag so it
// compiles under either suite). Here we layer on:
//
//   - pull-through enabled (PKGMIRROR_UPSTREAM_DEFAULT_MODE=cache_and_serve)
//   - a distinctive, polite User-Agent so upstream operators can
//     identify our traffic
//   - a preflight that checks upstream reachability and t.Skip's the
//     test (rather than failing red) when a public registry is down -
//     a "we cannot tell whether our code broke" signal should NOT
//     wake anyone up
//
// Etiquette (see docs/integration-testing.md):
//   - exercise only a tiny canary set of well-known stable packages
//   - run weekly, not daily, against public registries
//   - identify ourselves via User-Agent
//   - never hammer: the per-tenant rate limit is set to a conservative
//     30 req/min in the wrapper to defend against test-loop runaway
package integration
