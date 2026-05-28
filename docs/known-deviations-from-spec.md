# Known deviations from spec

This document is a running list of places where pkgmirror's wire
behavior intentionally differs from the canonical spec for a package
format. The goal is to make those deviations findable so an operator
comparing pkgmirror to a stock upstream registry knows where the
behavior diverges and why.

What counts as a "deviation" for this document:

- We emit a different value or shape than the spec literally says.
- An ordinary client tolerates it (otherwise the conformance test
  would fail), but a strict spec-conformance audit would flag it.
- The deviation is intentional, not a bug. (Bugs go to the issue
  tracker.)

What does NOT belong here:

- Gaps where we haven't implemented an optional spec feature yet
  (e.g. OCI `/v2/_catalog`, Debian by-hash routes, PEP 740
  attestations). Track those in the roadmap / per-format DECISIONS
  entries.
- Internal implementation choices that aren't visible on the wire
  (e.g. how we encode composite keys into file names).

Entries are organized by format, newest first within each format.

---

## Debian (`apt`)

### `Release.Date` is derived from data state, not wall-clock

**Spec:** [Debian Repository Format §
1.7](https://wiki.debian.org/DebianRepository/Format#A.22Release.22_files)
defines `Date` as "the date the release file was created."

**What pkgmirror does:** sets `Date` to the RFC1123 representation of
`max(file.created_unix)` across the distribution — i.e. the
timestamp of the most recent `.deb` upload, not the moment the
Release file was generated. See
[`internal/packages/debian/index.go`](../internal/packages/debian/index.go)
`BuildReleaseFiles`.

**Why.** pkgmirror generates the `Release`, `Release.gpg`, and
`InRelease` triple on demand per request, rather than persisting
them as file rows (which is what Forgejo and most other Debian
registry implementations do). `apt` fetches `Release` and
`Release.gpg` in two independent HTTP requests; if `Date` were
`time.Now()`, the bytes of `Release` would drift between the two
requests, the detached signature wouldn't validate against the body
apt actually sees, and `apt update` would fail with `GPG error`.

Deriving `Date` from the data state guarantees byte-identical
`Release` bodies across requests until a new upload lands, which is
exactly the invariant the signature requires.

**Observable impact.** None for `apt`'s install / update path. apt
uses `Date` only for the `Valid-Until` cache-freshness check (which
pkgmirror doesn't emit) and human-readable display. If you `cat
Release` immediately after publishing a new `.deb`, the `Date` will
match the upload timestamp rather than the current clock — usually
within a few seconds either way.

**Operator hook.** If you need spec-literal `Date` behavior — e.g.
because you've layered a custom apt frontend that does its own
freshness analysis — switch to persisting the Release triple as
file rows. The supporting machinery (synthetic `_debian` package
row, ExclusivePool keygen lock) is already in place; only the
build-on-write trigger and the file row writes would need to be
added.

**Cross-reference.** This is the first instance of the broader
[Stable bytes for signed-on-demand
content](adding-a-format.md#stable-bytes-for-signed-on-demand-content)
pattern. Future signed-on-demand formats (RPM's `repomd.xml.asc`,
NuGet signed packages, etc.) will face the same constraint and
should follow the same data-derived-timestamp approach.

---
