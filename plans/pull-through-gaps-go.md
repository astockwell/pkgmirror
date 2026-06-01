# Go modules pull-through — remaining gaps

Companion to the shipped Go pull-through implementation
([`internal/packages/goproxy/upstream.go`](../internal/packages/goproxy/upstream.go),
implementation details in [`plans/upstream-pull-through.md` §9.3](upstream-pull-through.md)).

**Status:** Go pull-through has been shipped for a while; the
`subjectFor` regression-class fix landed in commit `f3af13a` (May 2026).
All gaps below are post-ship observations from the gap-analysis pass
done alongside the RubyGems pull-through ship.

**Methodology:** Walked the shipped code against the non-negotiables
in [`docs/adding-jit-pull-through.md`](../docs/adding-jit-pull-through.md)
and the per-format hazards in
[`upstream-pull-through.md` §9.3](upstream-pull-through.md). Format-
specific gaps come first; cross-format gaps shared with
[RubyGems](pull-through-gaps-ruby.md) and
[PyPI](pull-through-gaps-python.md) are listed once at the bottom for
self-containment.

---

## Format-specific gaps

### F1. sumdb (`sum.golang.org`) is not proxied; no module hash verification

**Current:** [`internal/packages/goproxy/upstream.go:28-30`](../internal/packages/goproxy/upstream.go#L28-L30)
states the design choice:

> sumdb (sum.golang.org) is INTENTIONALLY NOT proxied in v1.
> Operators who want sumdb verification should leave GOSUMDB
> pointing at the canonical sum.golang.org; pkgmirror only handles
> the proxy plane.

Two consequences of this:

1. **Egress hole.** A `go get` client talking to pkgmirror still
   makes outbound requests to `sum.golang.org` for every module
   resolution. Operators who run pkgmirror precisely to centralize
   upstream contact get less than they think — half the traffic still
   bypasses pkgmirror.
2. **No hash verification on ingest.** Because we don't talk to
   sumdb, pkgmirror has no canonical SHA for the `.zip` it persists.
   [`pullThroughIngest`](../internal/packages/goproxy/upstream.go#L546-L648)
   buffers the upstream zip through `NewHashedBuffer` (which computes
   our local hash for blob storage dedup) but **never compares against
   a known-good value**. Compare with PyPI's
   [`pullThroughDownload`](../internal/packages/pypi/upstream.go#L487-L489),
   which has:

   ```go
   if want := entry.Hashes["sha256"]; want != "" && !strings.EqualFold(want, sha256Hex) {
       return false, fmt.Errorf("sha256 mismatch: ...")
   }
   ```

   and with [RubyGems's verification](../internal/packages/rubygems/upstream.go#L495-L505)
   against the compact-index `checksum:` field.

**Desired:** A sumdb-proxy mode that:
- Forwards `GET /lookup/<module>@<version>` to `sum.golang.org` (with
  signature verification via the embedded sumdb public key).
- Caches signed tree-heads + lookup responses with the same
  metadata-cache TTL semantics as `/@v/list`.
- Provides hashes to `pullThroughIngest` so the persisted zip can be
  verified before write.

**Severity:** Security. The hash-verification gap is the more
serious half — a compromised `proxy.golang.org` (or MITM between us
and it) can ship bytes that we'll happily persist with no opportunity
to detect tampering.

**Effort:** M. The Go sumdb protocol is well-specified
([go.dev/ref/mod#checksum-database](https://go.dev/ref/mod#checksum-database))
and existing libraries (`golang.org/x/mod/sumdb`) handle the
signature verification. Plumbing it into our fetcher + ingest is
straightforward; the gotcha is the Merkle-tree consistency proofs
require some persistent state (we have to remember tree heads to
verify they advance monotonically).

**Documented?** Yes — explicitly deferred. The deferral is OK; the
hash-verification gap that falls out of it is the part that should
get tightened. Even without a full sumdb proxy, we could do a
synchronous lookup-and-verify against `sum.golang.org` from the
pkgmirror process on ingest (no need to proxy the *client*'s sumdb
traffic to also start verifying ingest hashes).

---

### F2. `retract` directives in upstream `go.mod` files are ignored

**Current:** Go modules can declare retractions in their own go.mod:

```go
retract v1.0.0 // contains data-destroying bug
retract [v1.0.1, v1.0.5] // CVE-XXXX
```

`go list -m -versions` against `proxy.golang.org` filters retracted
versions out of the result by default. pkgmirror's
[`servePullThroughList`](../internal/packages/goproxy/upstream.go#L260-L276)
does NOT — it serves the bare `/@v/list` upstream response, which by
the protocol IS supposed to include retractions, and trusts the
client to parse each version's `.mod` and check.

The local handler's [`list`](../internal/packages/goproxy/handler.go#L142-L162)
inherits this — local resolution just iterates `models.ListVersions`
without consulting the latest version's `go.mod` retract block.

**Desired:** When emitting `/@v/list`, fetch the latest version's
`.mod`, parse the `retract` directives, and emit retracted versions
last (or omit them). The `go` toolchain does this client-side, but
admins can't see retracted versions in the console either, so a
server-side filter would surface them more usefully.

**Severity:** Correctness. The `go` toolchain does its own
retract-filtering on the client side, so the user-visible blast
radius is limited. But: the policy engine never sees "this version
is retracted upstream," so cooldown / blocklist rules can't react
to retractions. A package the maintainer pulled because of a security
issue still flows through pkgmirror unchallenged.

**Effort:** M. Requires parsing `go.mod` (use `golang.org/x/mod/modfile`)
on every `/@v/list` cold-path request, which is one extra `.mod`
fetch. The metadata cache absorbs the cost on subsequent requests.

**Documented?** No — unstated gap.

---

### F3. Pseudo-version format not validated before upstream contact

**Current:** Pseudo-versions look like
`v0.0.0-20240514123045-abc123def456` (date + 12-char commit prefix).
[`fetchUpstreamInfo`](../internal/packages/goproxy/upstream.go#L113-L141)
accepts any version string and passes it through unchanged. A
malicious or buggy client could:

- Submit a malformed version like `v1.0.0-very-long-attacker-controlled-string`
  and trigger an upstream lookup.
- Use a version string with path traversal characters (`v1.0.0/../`).
  The router likely catches the latter but it's defense-in-depth we
  don't have.

**Desired:** Validate against `golang.org/x/mod/module.IsValidVersion`
(or the same regex `proxy.golang.org` uses) BEFORE issuing the
upstream call. Reject malformed at our edge.

**Severity:** Convenience leaning toward security. We're not the
authoritative validator (upstream rejects too) but defense-in-depth
matters; we'd reduce upstream load and audit-log noise.

**Effort:** S. Add `module.IsValidVersion(version)` checks at the
top of each cold-path handler (info, mod, zip).

**Documented?** No — unstated gap.

---

### F4. `+incompatible` versions are passed through without special handling

**Current:** Go modules without semantic-import-versioning support
get a `+incompatible` suffix when used outside their major-version
path (e.g. `github.com/Sirupsen/logrus v1.0.5+incompatible`).
pkgmirror treats `+incompatible` as just-another-version-string —
which works because the `go` toolchain handles the suffix.

**Desired:** Possibly worth surfacing in the admin UI ("this module
is in incompatible mode; consider upgrading to a SIV-aware version")
but functionally nothing's broken.

**Severity:** Convenience.

**Effort:** S if we ever want admin-side surface.

**Documented?** No — but this is more "nice-to-have insight" than
a real gap. Listed for completeness.

---

### F5. No proxy of `proxy.golang.org`'s extended ops (`/lookup`, etc.)

**Current:** The Go module proxy protocol has exactly the five GET
endpoints we proxy: `/<module>/@v/list`, `/@v/<v>.info`, `.mod`,
`.zip`, and `/@latest`. There are no other endpoints in v1 of the
protocol. So this is not a gap per se.

**Desired:** N/A.

**Severity:** N/A.

**Effort:** N/A.

**Documented?** Listed for completeness — the Go module proxy
protocol IS small, and we cover all of it. The sumdb proxy (F1) is
a separate protocol, not part of GOPROXY.

---

### F6. `@latest` resolved to a locally-quarantined version returns 404

**Current:** [`latest`](../internal/packages/goproxy/handler.go#L355-L383)
handles the case where local has the package but every locally-cached
version is policy-blocked: it falls through to
`servePullThroughLatest`, which talks to upstream. Good. But if
upstream's `@latest` IS the blocked version, we 404 with
`no readable version`. The client never learns there's a stable
older version it could use; it has to know to ask for a specific
version.

**Desired:** When `@latest` is blocked, fall back to the most recent
ALLOWED upstream version (probably via a `/@v/list` + per-version
policy filter), rather than 404'ing. This matches the
"cooldown-defends-against-fresh-bugs" intuition: the user wanted
"latest stable" and we should give them the closest thing.

**Severity:** Convenience. Operators can teach users to pin versions
in their `go.mod`; @latest is a UX nicety.

**Effort:** M. The flow would be: cold `@latest` → fetch upstream's
`@latest` → policy check → on block, fetch `/@v/list` → walk
newest-first applying per-version policy → return the first allowed
version's `.info`. Each step is cheap (we already do them
individually); the orchestration is the work.

**Documented?** No — unstated gap.

---

### F7. Dependency graph not indexed locally

Same shape as RubyGems F6 and PyPI F12. Cross-format issue covered
in **X4** below.

---

## Cross-format gaps (shared with RubyGems + PyPI)

These gaps affect all three shipped pull-through formats identically.
Each is listed once here per format for completeness; closing any of
them should ideally be done in one PR that touches all three adapters.

### X1. `cache_only` mode is not honored by the per-format handlers

**Current:** [`internal/upstream/upstream.go:33-37`](../internal/upstream/upstream.go#L33-L37)
defines `ModeCacheOnly` ("fetches on miss and persists as
quarantined"). The Go handler's
[`passthroughEnabled`](../internal/packages/goproxy/upstream.go#L57-L66)
checks only `cfg.Mode != ModeOff` and treats `cache_only` identically
to `cache_and_serve`.

**Desired:** Persist the version, then 403 the inflight client with
"version pending admin review" — same shape as the post-ingest
`ActionRead` gate, but triggered by mode instead of a policy rule.

**Severity:** Correctness. Operators configuring `cache_only` today
get `cache_and_serve` behavior — silently the wrong thing.

**Effort:** S per format. After ingest, check the resolved
`TenantConfig.Mode`; if `cache_only`, 403.

**Documented?** No — gap discovered during this gap-analysis pass.
Most security-relevant cross-format issue.

---

### X2. ETag / `If-None-Match` revalidation not used by adapters

**Current:** The fetcher supports conditional GETs at the API level
([`Request.IfNoneMatch`](../internal/upstream/upstream.go#L89),
forwarded at [`fetcher.go:155-156`](../internal/upstream/fetcher.go#L155-L156),
response ETag captured at [`fetcher.go:215`](../internal/upstream/fetcher.go#L215),
ETag stored in cache at [`fetcher.go:242`](../internal/upstream/fetcher.go#L242)).
But the Go adapter (and the other two) never POPULATE `IfNoneMatch`,
and the metadata cache never CONSULTS its stored ETags on TTL expiry.
Every TTL-expired metadata fetch re-downloads the full body.

For Go specifically: `/@v/list` and `/@latest` are the chatty
endpoints. A module with 50 versions has its `/@v/list` re-fetched
in full every 5 minutes (default metadata TTL); the response is
maybe 1 KB but the redundant traffic adds up.

**Desired:** Cache layer should issue a conditional GET on TTL
expiry with the stored ETag and treat 304 as "bump TTL, return
cached bytes."

**Severity:** Convenience.

**Effort:** M. Shared fetcher + metadata cache change, not per-format.
Closing this fixes all three adapters in one PR.

**Documented?** Partial. [`upstream-pull-through.md §7.1`](upstream-pull-through.md)
states the design intent; the implementation shipped without it.

---

### X3. `429 Too Many Requests` lacks `Retry-After`

**Current:** Per-tenant upstream rate limit fires
([`ratelimit.go`](../internal/upstream/ratelimit.go)),
[`mapUpstreamErr`](../internal/packages/goproxy/upstream.go) (and
the corresponding helpers in PyPI + RubyGems) translates to `429`,
but no `Retry-After` header is set. Clients can't tell when to retry.
The console login rate-limiter sets the header
([`internal/console/middleware/ratelimit.go:155`](../internal/console/middleware/ratelimit.go#L155))
— precedent exists.

**Severity:** Convenience.

**Effort:** XS once we expose a "seconds until next token" method on
the rate limiter; XS per format thereafter.

**Documented?** Partial — comment at
[`upstream.go:137-139`](../internal/upstream/upstream.go#L137-L139)
records design intent; implementation missing.

---

### X4. Dependency graph not indexed locally

**Current:** Pull-through ingest stores `go.mod` as a property
([`pullThroughIngest`](../internal/packages/goproxy/upstream.go#L627-L646)
sets `VersionProperties[PropertyGoMod]`) but doesn't parse the
require/replace directives into a cross-package-queryable table.
Same shape across the other two formats.

**Desired:** A `package_dependencies` table populated on every ingest
path (upload + pull-through), enabling audit queries ("what's the
blast radius of CVE-X?") and an admin UI surface.

**Severity:** Convenience — useful for compliance, not load-bearing.

**Effort:** M. Schema migration, populate on every ingest path, wire
a query API. Same shape for all three formats.

**Documented?** No — unstated gap.

---

### X5. No per-tenant upstream authentication

**Current:** `TenantConfig` has no auth credential fields; the
fetcher doesn't add an `Authorization` header. Only public upstreams
are reachable. For Go specifically: no support for private GOPROXY
endpoints (Athens behind auth, JFrog GoCenter behind tokens, etc.).

**Severity:** Correctness for the airgapped/private-mirror use case.

**Effort:** M for cross-format infrastructure; XS per format.

**Documented?** Yes — [`upstream-pull-through.md §5.5`](upstream-pull-through.md)
defers to "PR Q." Listed for completeness.

---

## Triage view

If you're picking gaps to close, the order I'd suggest:

1. **F1 partial — at-ingest hash verification via sumdb lookup**
   (security, M). Don't need the full sumdb proxy to start verifying
   zip hashes server-side. Highest-priority security gap in Go
   pull-through.
2. **X1 — `cache_only` mode honoring** (correctness, S each format).
3. **F2 — retract handling on `/@v/list`** (correctness, M).
   Particularly valuable for cooldown/blocklist rules that should
   defensively react to retractions.
4. **F6 — `@latest` fallback to newest-allowed** (convenience, M).
   UX win for users on a strict cooldown.
5. **X2 — ETag revalidation** (convenience, M shared). Cross-format
   one-PR win.
6. **F1 full — sumdb proxy** (security, M-L). The bigger sister of
   the F1-partial above; closes the egress hole.
7. **X3 — `Retry-After` on 429** (convenience, XS).
8. **F3 — pseudo-version validation** (defense-in-depth, S).
9. **X4 — dependency graph indexing** (convenience, M shared).
10. Everything else (F4, F5, X5) — either no real-world issue or
    intentionally deferred.

Nothing here is a release-blocker for the normal `go get` flow. The
sumdb-shaped gap (F1) is the closest to one; mitigation today is
"operators leave GOSUMDB pointing at the canonical sum.golang.org,"
which is documented but only as effective as the client honors it.
