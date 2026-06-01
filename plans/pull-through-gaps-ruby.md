# RubyGems pull-through — remaining gaps

Companion to [`rubygems-pull-through.md`](rubygems-pull-through.md). That
plan documented what we built; this one documents what we DIDN'T build,
so a future engineer (probably future-me) has a clear inventory before
deciding whether to close any gap.

**Status:** RubyGems pull-through shipped in commits `ad8a7e6` +
`9ea76fa` (May 2026). All gaps below are post-ship observations.

**Methodology:** [`docs/adding-jit-pull-through.md`](../docs/adding-jit-pull-through.md)
enumerates non-negotiable behaviors for a complete adapter. I walked
the shipped code against that list and against the per-format hazards
in [`plans/upstream-pull-through.md` §9.7](upstream-pull-through.md)
and noted every behavior that's either (a) not implemented, (b) only
partially implemented, or (c) explicitly deferred.

Gaps are grouped: **format-specific** are unique to RubyGems;
**cross-format** affect all three shipped adapters identically and are
mirrored in [`pull-through-gaps-go.md`](pull-through-gaps-go.md) +
[`pull-through-gaps-python.md`](pull-through-gaps-python.md).

For each gap: severity (security / correctness / convenience), effort
(XS / S / M / L), and a note on whether the deferral is documented or
new news.

---

## Format-specific gaps

### F1. Platform-tagged `.gem` filenames return 404 on cold miss

**Current:** [`parseGemFilename`](../internal/packages/rubygems/upstream.go#L654-L680)
rejects any filename whose right-of-version segment contains a hyphen,
which is the signature of a platform suffix
(`nokogiri-1.16.0-x86_64-linux.gem`, `ffi-1.16.3-java.gem`). The
caller in [`downloadPackageFile`](../internal/packages/rubygems/handler.go#L487-L508)
then 404s. **Zero upstream contact** — we don't even ask `index.rubygems.org`.

**Desired:** Accept the filename. The clean implementation is to call
`/info/<name>` first to enumerate `(version, platform)` tuples, then
reverse-match against the filename. The information needed is already
in the compact-index lines (we just discard the platform field today
in [`parseCompactIndexLine`](../internal/packages/rubygems/upstream.go#L181-L218)).

**Severity:** Convenience.
- Affects: gems with native extensions (nokogiri, sqlite3, ffi, grpc,
  google-protobuf, sassc, nio4r, etc.). Bundler in production environments
  routinely fetches these.
- Mitigation today: operators `gem fetch` + `gem push` natively-extensioned
  gems manually into pkgmirror; cumbersome.

**Effort:** S. Existing `compactIndexLine` already captures `Platform`;
extend `parseGemFilename` to do an `/info/<name>` lookup when the
no-platform parse fails. The cold-path code in `servePullThroughGem`
already loops over `info.Lines`; the match predicate just needs to
compare `(v.Version, v.Platform)` against the parsed filename's
(version, platform) tuple.

**Documented?** Yes — [`rubygems-pull-through.md` §6](rubygems-pull-through.md#6-whats-intentionally-out-of-scope)
explicitly defers. This is the most clearly-shaped follow-up.

---

### F2. Yanked versions appear in the served compact index

**Current:** When upstream marks a version yanked, the rubygems.org
compact-index `/info/<name>` line is prefixed `-` (e.g.
`-1.0.0 |checksum:abc...`) per the [compact-index spec](https://guides.rubygems.org/rubygems-org-compact-index-api/).
Our [`parseCompactIndexLine`](../internal/packages/rubygems/upstream.go#L181-L218)
discards the leading `-`-prefixed lines... actually it doesn't —
neither parses nor filters them. They flow through into the response
verbatim. The receiving bundler client tolerates this but the version
remains a candidate for resolution.

**Desired:** Recognize and either (a) filter yanked lines out
entirely, or (b) re-emit them with the `-` prefix preserved so bundler
correctly skips them per its own yank semantics. Bundler's behavior:
yanked versions are still considered installable from a lockfile but
not chosen on fresh resolves. So (b) is more spec-compliant.

**Severity:** Correctness. Bundler's yank protocol is broken when our
cold-path filter strips the prefix or doesn't track it.

**Effort:** S. Add a `Yanked bool` field to `compactIndexLine`, detect
the leading `-` in `parseCompactIndexLine`, preserve it when
re-emitting in `servePullThroughInfo`'s line-by-line writeback.

**Documented?** No — unstated gap. Was missed during plan-writing
because the existing local handler doesn't deal with yanks at upstream;
it uses `models.QuarantineVersion` for local yank semantics (see
[`yankPackageVersion`](../internal/packages/rubygems/handler.go#L633-L671)),
which is a different code path.

---

### F3. Legacy `.gz` specs files are local-only

**Current:** `/specs.4.8.gz`, `/latest_specs.4.8.gz`,
`/prerelease_specs.4.8.gz` ([handler.go:242-271](../internal/packages/rubygems/handler.go#L242-L271))
all return the local-only view of what's in this tenant. No upstream
pull-through.

**Desired:** Could proxy + filter, but the upstream files are tens of
MB and change constantly. The use case for proxying them is for
clients that don't speak the compact index (older `gem` releases <
2.7.0 from ~2018) — vanishingly rare in 2026.

**Severity:** Convenience — only matters for ancient `gem` clients.

**Effort:** L. Streaming Ruby Marshal decode + policy filter + re-encode
+ gzip. Not worth it for the audience.

**Documented?** Yes — [`rubygems-pull-through.md` §6](rubygems-pull-through.md#6-whats-intentionally-out-of-scope)
defers explicitly. Listed here for completeness.

---

### F4. `GET /versions` global file is local-only

**Current:** [`serveVersionsFile`](../internal/packages/rubygems/handler.go#L293-L329)
emits the compact-index "what packages exist, with which versions and
md5 over the /info file" map from local rows only. Cold tenants see
an empty file. Bundler then 404→pull-through-200s each gem it cares
about individually, which works but is chatty.

**Desired:** Either (a) a bandersnatch-style admin-triggered "sync
from upstream" populates this lazily, or (b) merge upstream's global
file with local state on every request. Both are substantial:

- (a) needs its own scheduler, sync job runner, and partial-failure
  recovery. Probably its own plan.
- (b) requires re-computing every package's md5 on every request,
  because the md5 in `/versions` is over the corresponding `/info/<name>`
  body — which we filter through policy and may emit differently from
  upstream.

**Severity:** Convenience.

**Effort:** L for either path.

**Documented?** Yes — [`rubygems-pull-through.md` §6](rubygems-pull-through.md#6-whats-intentionally-out-of-scope).
Listed for completeness.

---

### F5. `.gemspec.rz` cold miss falls through to 404

**Current:** [`servePackageSpecification`](../internal/packages/rubygems/handler.go#L331-L382)
synthesizes the gemspec from local `versionMetadata` when the gem is
already ingested. On a cold miss it 404s.

**Desired:** Could fetch the upstream `/quick/Marshal.4.8/<file>.gemspec.rz`
directly. But the surrounding flow is: client asks for `.gemspec.rz`
→ cold miss → 404. Real clients don't do this — they ask for the
`.gem` first (which DOES pull through), at which point the local
synthesis path takes over for the spec.

**Severity:** Convenience — no real-world client hits this path
without first having the gem.

**Effort:** XS if we ever decide it's worth it (`fetchUpstreamGemspec`
+ one route wire). Probably never worth it.

**Documented?** Yes — [`rubygems-pull-through.md` §3.3](rubygems-pull-through.md)
deliberately omits the wiring.

---

### F6. Dependency graph not indexed locally

**Current:** Pull-through ingest stores the gemspec metadata blob
(license, summary, runtime/dev deps) in
`package_versions.metadata_json` via
[`servePullThroughGem`](../internal/packages/rubygems/upstream.go#L496-L530).
The deps are surfaced when re-rendering `/info/<name>`
([`buildRequirementLine`](../internal/packages/rubygems/handler.go#L401-L450))
but not queryable cross-package — there's no table to ask "what
locally-cached gems depend on rack >= 2.0?"

**Desired:** A `package_dependencies` table populated on ingest +
upload. Enables audit queries ("what's in the blast radius of CVE-X
across all my tenants?") and an admin UI surface.

**Severity:** Convenience — useful for audit/compliance, not load-bearing
for the install flow.

**Effort:** M. Schema migration, populate on every ingest path (upload
+ pull-through), wire a query API. Same shape for all three formats.

**Documented?** No — unstated gap. This is a cross-format hole but
RubyGems' metadata is rich enough to be worth indexing on its own.

---

### F7. Pre-release filtering not surfaced

**Current:** RubyGems pre-release versions (anything containing a
letter after the first digit-sequence, e.g. `1.0.0.beta1`,
`5.0.0.rc.1`) flow through `/info/<name>` unfiltered. Locally, the
existing `prerelease_specs.4.8.gz` handler returns empty
([handler.go:266-271](../internal/packages/rubygems/handler.go#L266-L271))
— a deliberate v1 cut.

**Desired:** A per-tenant or per-rule toggle to hide pre-releases
from the cold-path index. The plumbing already exists (the policy
engine could read a `is_prerelease` Subject attribute).

**Severity:** Convenience.

**Effort:** S. Add `is_prerelease` to the Subject built in
`passthroughBlocked`, parse the version with a small regex, register
a `kind: prerelease_filter` evaluator (or reuse a generic
"version-attribute" rule).

**Documented?** No — unstated gap.

---

## Cross-format gaps (shared with Go + PyPI)

These gaps affect all three shipped pull-through formats identically.
Each is listed once here per format for completeness; closing any of
them should ideally be done in one PR that touches all three adapters
to keep the surface consistent.

### X1. `cache_only` mode is not honored by the per-format handlers

**Current:** [`internal/upstream/upstream.go:33-37`](../internal/upstream/upstream.go#L33-L37)
defines three modes — `off`, `cache_and_serve`, `cache_only` — but the
RubyGems handler treats anything that's not `off` as cache-and-serve.
[`passthroughEnabled`](../internal/packages/rubygems/upstream.go#L96-L106)
returns `true` for both modes, and `servePullThroughGem` always serves
the bytes after ingest.

**Desired:** In `cache_only` mode, the version IS persisted (so an
admin can later promote it) but the inflight client gets 403. This
matches the documented quarantine semantics: same shape as the
post-ingest `ActionRead` gate, but triggered by the mode instead of
a policy rule.

**Severity:** Correctness. Operators configuring `cache_only` today
get `cache_and_serve` behavior — silently the wrong thing.

**Effort:** S per format. After ingest, check the resolved
`TenantConfig.Mode`; if `cache_only`, 403 + the "version persisted
for admin review" message.

**Documented?** No — gap discovered during this gap-analysis pass.
This is the most security-relevant cross-format issue: a tenant
explicitly opting into a more restrictive mode gets the less
restrictive one.

---

### X2. ETag / `If-None-Match` revalidation not used by adapters

**Current:** The fetcher infrastructure DOES support conditional GETs:
- [`upstream.Request.IfNoneMatch`](../internal/upstream/upstream.go#L89)
  is forwarded to the upstream call
  ([`fetcher.go:155-156`](../internal/upstream/fetcher.go#L155-L156))
- The response's `ETag` is captured
  ([`fetcher.go:215`](../internal/upstream/fetcher.go#L215))
- The metadata cache stores the ETag for retrieval
  ([`fetcher.go:242`](../internal/upstream/fetcher.go#L242))

But the per-format adapters never POPULATE `IfNoneMatch` and the
metadata cache never CONSULTS the stored ETag on TTL expiry. The
result: every TTL-expired metadata fetch re-downloads the full body
instead of doing a `If-None-Match` round-trip and bumping the cache
TTL on `304 Not Modified`.

**Desired:** When the metadata cache has an entry whose TTL has
expired (but whose body+ETag are still in memory), the fetcher should
issue a conditional GET with that ETag and treat a `304` as
"refresh TTL, return cached bytes."

**Severity:** Convenience — bandwidth waste, not a correctness issue.
Wasted on the order of `~5 KB per package per cache-TTL period`,
multiplied by every tenant pulling the same package.

**Effort:** M. Touches the shared fetcher + metadata cache, not
per-format code, so closing this fixes RubyGems + Go + PyPI in one
PR.

**Documented?** Partial. [`plans/upstream-pull-through.md §7.1`](upstream-pull-through.md)
calls out the design intent ("On revalidation (TTL expired):
conditional GET with `If-None-Match` / `If-Modified-Since`. 304 →
bump fetched_at, return cached bytes.") but the implementation
shipped without it. The infrastructure is half-done — `Request` has
the field, the wire path uses it, but the cache layer doesn't read
its own stored ETags.

---

### X3. `429 Too Many Requests` lacks `Retry-After`

**Current:** When the per-tenant upstream rate limit fires
([`internal/upstream/ratelimit.go:24-44`](../internal/upstream/ratelimit.go#L24)),
the fetcher returns `ErrUpstreamRateLimit`. Each per-format
`mapUpstreamErr` translates that to `429`
([RubyGems](../internal/packages/rubygems/upstream.go#L860-L877)
+ [Go](../internal/packages/goproxy/upstream.go) +
[PyPI](../internal/packages/pypi/upstream.go#L593-L607)) but
no `Retry-After` header is set. Clients can't tell when to retry.

The header IS used elsewhere in the codebase — the login rate-limiter
sets it
([`internal/console/middleware/ratelimit.go:155`](../internal/console/middleware/ratelimit.go#L155))
— so the precedent exists.

**Desired:** Compute time-to-next-token from the bucket state and
emit `Retry-After: <seconds>`.

**Severity:** Convenience — affects client behavior under load.

**Effort:** XS once we expose a "seconds until next token" method on
the rate limiter; XS per format thereafter.

**Documented?** Partial. The plan
([`upstream.go:137-139`](../internal/upstream/upstream.go#L137))
comments `// 429 to the client with Retry-After.` — design intent
recorded, implementation missing.

---

### X4. No per-tenant upstream authentication

**Current:** All three adapters can only pull through from public
upstreams. The [`TenantConfig`](../internal/upstream/upstream.go#L191-L196)
has no auth credential fields, and the fetcher doesn't add an
`Authorization` header to upstream requests.

**Desired:** Per-tenant credentials so operators can pull from private
mirrors (corporate Gem repos like Gemfury / Cloudsmith / JFrog
Artifactory). Schema migration + admin UI + fetcher integration.

**Severity:** Correctness for the airgapped/private-mirror use case;
not affecting anyone today who's running against the public canonical
registries.

**Effort:** M for the cross-format infrastructure (schema + UI +
fetcher); XS per format thereafter.

**Documented?** Yes — [`upstream-pull-through.md §5.5`](upstream-pull-through.md)
defers to "PR Q." Listed here for completeness.

---

## Triage view

If you're picking gaps to close, the order I'd suggest:

1. **X1 — `cache_only` mode honoring** (correctness, S each format).
   Silent misconfiguration is the worst kind of bug.
2. **F2 — yanked-version handling** (correctness, S). Bundler relies
   on this; we're breaking the upstream protocol contract by stripping
   the marker.
3. **F1 — platform gems on cold miss** (convenience, S). Highest
   user-visible value; the gap is already documented + scoped.
4. **X2 — ETag revalidation** (convenience, M shared). The
   infrastructure is half-done; finishing it is one PR for all three
   formats and is a clear win.
5. **X3 — `Retry-After` on 429** (convenience, XS). Quick win once
   anything else here ships.
6. **F7 — pre-release filtering** (convenience, S). Useful for
   security-conscious shops.
7. **F6 — dependency graph indexing** (convenience, M). Real value
   for compliance/audit; not load-bearing.
8. Everything else (F3, F4, F5, X4) — either intentionally deferred
   or solving a problem no one has today.

Nothing here is a release-blocker; the shipped adapter handles the
main `gem install` / `bundle install` flow correctly. These are the
edges.
