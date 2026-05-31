//go:build blackbox || integration

// Package harness wires up disposable docker containers for pkgmirror's
// black-box conformance tests AND the internet-connected integration
// suite (tests/integration/...). The two suites share container
// orchestration; integration just opts into pull-through with a
// distinctive User-Agent (see StartWithOptions).
//
// See docs/blackbox-testing.md for the design and intended usage, and
// docs/integration-testing.md for the integration suite specifically.
package harness
