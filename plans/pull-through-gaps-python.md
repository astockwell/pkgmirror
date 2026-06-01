# PyPI pull-through — remaining gaps

Companion to the shipped PyPI pull-through implementation
([`internal/packages/pypi/upstream.go`](../internal/packages/pypi/upstream.go);
implementation details in [`plans/upstream-pull-through.md` §9.1](upstream-pull-through.md)).

**Status:** PyPI was the first format to ship pull-through (PR F).
All gaps below are post-ship observations from the gap-analysis pass
done alongside the RubyGems pull-through ship.

**Methodology:** Walked the shipped code against the non-negotiables
in [`docs/adding-jit-pull-through.md`](../docs/adding-jit-pull-through.md)
and the per-format hazards in [`upstream-pull-through.md` §9.1](upstream-pull-through.md).
Format-specific gaps come first; cross-format gaps shared with
[Go](pull-through-gaps-go.md) and
[RubyGems](pull-through-gaps-ruby.md) are listed once at the bottom
for self-containment.

PyPI is the most-feature-complete pull-through adapter (it was the
prototype + reference), so this list is the shortest of the three.

---

## Format-specific gaps

### F1. PEP 691 JSON not requested via `Accept` header

**Current:** The fetcher's
[`upstream.Request`](../internal/upstream/upstream.go#L63-L91) struct
has no field for arbitrary request headers, so the per-format
adapters can't ask upstream for the PEP 691 JSON variant of the
simple index. [`fetchUpstreamSimple`](../internal/packages/pypi/upstream.go#L62-L101)
notes this explicitly:

```go
// We always force PEP 691 via the Accept header. The default
// fetcher doesn't set custom headers so we have to do a second
// request via the fetcher's Fetch... actually no. The fetcher
// doesn't expose Accept-setting today, so we fall back to parsing
// HTML if the response is HTML.
```

The fallback path is [`parseSimpleHTML`](../internal/packages/pypi/upstream.go#L113-L167)
— a hand-rolled HTML scraper that walks `<a href="...">filename</a>`
tags. It works against pypi.org's current HTML output but is
*structurally* more fragile than JSON parsing and has no `yanked`
field at all (see F2).

**Desired:** Add an `Accept` (or generic `Headers map[string]string`)
field to `upstream.Request`. Set
`Accept: application/vnd.pypi.simple.v1+json` in
`fetchUpstreamSimple`. The HTML fallback can stay as a defense
against mirrors that don't speak JSON.

**Severity:** Convenience leaning toward correctness. The HTML
fallback works today, but JSON gives us strictly more data (notably
the `yanked` field — see F2) and is significantly more robust to
upstream rendering changes.

**Effort:** XS. Add the field to `upstream.Request`, plumb through
to `http.NewRequest`, set in PyPI's adapter. The other formats don't
need this today but it's a small enabler.

**Documented?** No — but the gap is explicitly called out in the
code comment as a known limitation.

---

### F2. PEP 691 `yanked` field not parsed or honored

**Current:** PEP 691 / PEP 700 give each file entry an optional
`yanked` field (boolean or string-with-reason) indicating the file
should not be considered for normal resolution.
[`upstreamSimpleFile`](../internal/packages/pypi/upstream.go#L46-L50)
does NOT declare a `Yanked` field:

```go
type upstreamSimpleFile struct {
    Filename string            `json:"filename"`
    URL      string            `json:"url"`
    Hashes   map[string]string `json:"hashes"`
}
```

So yanked entries flow through the index unchanged. pip's resolver
DOES handle the field client-side, so user-visible behavior is
mostly OK — but:

- The policy engine never sees "this version was yanked," so a
  cooldown / blocklist rule can't react to a yanked package the way
  the maintainer (or PyPI security team) intended.
- Our HTML-parsing fallback path
  ([`parseSimpleHTML`](../internal/packages/pypi/upstream.go#L113-L167))
  has no way to surface yanks at all — they're communicated via
  `data-yanked` attributes in PEP 503 HTML, which we don't parse.

**Desired:** Add `Yanked any` (json:"yanked,omitempty") to
`upstreamSimpleFile`; expose to the Subject as a boolean attribute
`is_yanked`; either drop yanked entries from `upstreamFileEntries`
or pass them through with the marker preserved so policy can decide.

**Severity:** Correctness. Yank is a documented protocol contract
we're silently dropping.

**Effort:** S. JSON-side field addition + `upstreamFileEntries`
predicate; HTML-side parser update to read `data-yanked`. Add a
`is_yanked` attribute to the Subject so cooldown's
"upstream_publish" time source isn't the only thing that gets to
react to upstream metadata.

**Documented?** No — unstated gap. The code comment at
[`upstream.go:22`](../internal/packages/pypi/upstream.go#L22)
acknowledges "yanked versions" are one of the "edge cases that
surround index merging" we wanted to avoid, but the merge is
implemented (see F4 below) and the yank field isn't.

---

### F3. `requires-python` not surfaced to the cold-path index filter

**Current:** On a known-local package, the index handler reads
`PropRequiresPython` and emits `data-requires-python="..."` on each
`<a>` tag
([`handler.go:362-371`](../internal/packages/pypi/handler.go#L362-L371)).
But the cold-path PEP 691 parser's
[`upstreamSimpleFile`](../internal/packages/pypi/upstream.go#L46-L50)
doesn't have a `RequiresPython` field, and
[`upstreamFileEntries`](../internal/packages/pypi/upstream.go#L288-L320)
sets `pypiFileEntry.RequiresPython` to the zero value:

```go
entries = append(entries, pypiFileEntry{
    Filename: f.Filename,
    URL:      fmt.Sprintf("../../files/%s/%s/%s", pkg.Name, ver, f.Filename),
    SHA256:   f.Hashes["sha256"],
})
```

Cold-path served indexes have no `requires-python` info even though
upstream provides it.

**Desired:** Add `RequiresPython string \`json:"requires-python,omitempty"\``
to `upstreamSimpleFile`, plumb through. The PEP 691 JSON includes
it; we just drop it. Cost is one struct field.

**Severity:** Convenience. pip's resolver gets the info on the
follow-up `/files/` request via the file's own metadata. But
having it in the simple index is a meaningful optimization for
solvers (uv in particular).

**Effort:** XS.

**Documented?** No — unstated gap.

---

### F4. Local↔upstream merge intentionally loses some upstream metadata

**Current:** Unlike Go and RubyGems (cold-only pull-through), PyPI
DOES merge local + upstream views when a package exists locally with
`CreatedVia=pull_through` ([`packageIndex`](../internal/packages/pypi/handler.go#L395-L433)
calls [`mergeUpstreamIntoLocalIndex`](../internal/packages/pypi/upstream.go#L334-L362)).
The merge is good but loses information:

- Files already-present locally win over upstream versions (de-dup
  by filename). Hashes from upstream are dropped for those entries
  even if upstream's hash differs (this is actually desirable — it
  catches upstream republishes).
- Upstream entries that "win" (i.e. don't have a local counterpart)
  carry only `(filename, URL, sha256)` — see F2/F3 for the missing
  fields.

This is a design choice, not a bug, but worth surfacing: the merge
hides upstream's "this file was republished with a new hash" signal,
which is a legitimate audit interest.

**Desired:** When local hash differs from upstream hash for the same
filename, emit an audit row + admin-console flag. The merge keeps
local for serving (correct), but the divergence is a useful signal.

**Severity:** Convenience — audit/compliance. Most divergences are
maintainer-republished wheels with metadata-only changes; some are
genuine supply-chain tampering signals.

**Effort:** M. Requires extending the merge path to record + audit
divergences, then a console UI to view them.

**Documented?** Partial. The merge behavior itself is documented;
the divergence-handling is a noted edge case in the upstream.go
header comment but never implemented.

---

### F5. Sdist vs wheel not distinguished in the policy `Subject`

**Current:** The policy `Subject` built by
[`subjectFor`](../internal/packages/pypi/handler.go#L614-L643) has
`Filename` (e.g. `requests-2.32.4-py3-none-any.whl`) but not a
parsed `file_type` attribute. A rule that wants to say "only allow
wheels for production tenants" has to do its own filename inspection.

**Desired:** Add `file_type` (`"wheel"` / `"sdist"` / `"egg"` /
`"unknown"`) to the Subject's Attrs. Trivial parse of the filename
suffix.

**Severity:** Convenience. Some orgs ban sdist (no build-from-source
in CI) or ban wheel (must build from source for reproducibility);
this would let them express the rule directly.

**Effort:** XS.

**Documented?** No — unstated gap.

---

### F6. Pre/post/dev release filtering not surfaced

**Current:** PEP 440 defines pre-releases (`1.0.0a1`, `1.0.0rc1`),
post-releases (`1.0.0.post1`), and dev releases (`1.0.0.dev1`). All
flow through the index unfiltered. The policy engine can't see
"this version is a pre-release" — only the raw version string.

**Desired:** A version-attribute parser populates Subject.Attrs with
`is_prerelease`, `is_postrelease`, `is_devrelease`. Rule kinds can
then say "warn on pre-releases" or "block dev releases for tenant
X."

**Severity:** Convenience. pip's resolver respects `--pre` flags,
so user-visible behavior is OK. But a security-conscious org
configuring "no pre-releases in prod" today has no clean way to
express it.

**Effort:** S. PEP 440 parsing in Go isn't built-in but is
straightforward (regex). Add to Subject.Attrs in `passthroughBlocked`
+ `subjectFor`.

**Documented?** No — unstated gap.

---

### F7. Dependency graph not indexed locally

Same shape as RubyGems F6 and Go X4. Cross-format issue covered in
**X4** below.

---

## Cross-format gaps (shared with Go + RubyGems)

These gaps affect all three shipped pull-through formats identically.
Each is listed once here per format for completeness; closing any of
them should ideally be done in one PR that touches all three adapters.

### X1. `cache_only` mode is not honored by the per-format handlers

**Current:** [`internal/upstream/upstream.go:33-37`](../internal/upstream/upstream.go#L33-L37)
defines `ModeCacheOnly` ("fetches on miss and persists as
quarantined"). PyPI's
[`passthroughEnabled`](../internal/packages/pypi/upstream.go#L613-L624)
checks only `cfg.Mode != ModeOff` and treats `cache_only` identically
to `cache_and_serve`.

**Desired:** Persist the version, then 403 the inflight client with
"version pending admin review" — same shape as the post-ingest
`ActionRead` gate, but triggered by mode instead of a policy rule.

**Severity:** Correctness. Operators configuring `cache_only` today
get `cache_and_serve` behavior — silently the wrong thing.

**Effort:** S per format.

**Documented?** No — gap discovered during this gap-analysis pass.
Most security-relevant cross-format issue.

---

### X2. ETag / `If-None-Match` revalidation not used by adapters

**Current:** The fetcher supports conditional GETs at the API level
([`Request.IfNoneMatch`](../internal/upstream/upstream.go#L89),
forwarded at [`fetcher.go:155-156`](../internal/upstream/fetcher.go#L155-L156),
response ETag captured at [`fetcher.go:215`](../internal/upstream/fetcher.go#L215),
ETag stored in cache at [`fetcher.go:242`](../internal/upstream/fetcher.go#L242)).
But PyPI's adapter never POPULATES `IfNoneMatch`, and the metadata
cache never CONSULTS its stored ETags on TTL expiry. Every
TTL-expired metadata fetch re-downloads the full body.

For PyPI specifically: `/simple/<name>/` is the chatty endpoint, and
on a popular package like `requests` the response is several KB —
larger than Go's `/@v/list` or RubyGems's `/info/`, so the savings
from 304s would be proportionally bigger.

**Desired:** Cache layer issues a conditional GET on TTL expiry with
the stored ETag and treats 304 as "bump TTL, return cached bytes."

**Severity:** Convenience.

**Effort:** M. Shared fetcher + metadata cache change, not per-format.
Closing this fixes all three adapters in one PR.

**Documented?** Partial. [`upstream-pull-through.md §7.1`](upstream-pull-through.md)
states the design intent; the implementation shipped without it.

---

### X3. `429 Too Many Requests` lacks `Retry-After`

**Current:** Per-tenant upstream rate limit fires
([`ratelimit.go`](../internal/upstream/ratelimit.go));
[`mapUpstreamErr`](../internal/packages/pypi/upstream.go#L593-L607)
translates to `429`, but no `Retry-After` header is set. Clients
can't tell when to retry.

**Severity:** Convenience.

**Effort:** XS once we expose a "seconds until next token" method on
the rate limiter.

**Documented?** Partial — comment at
[`upstream.go:137-139`](../internal/upstream/upstream.go#L137-L139)
records design intent; implementation missing.

---

### X4. Dependency graph not indexed locally

**Current:** Pull-through ingest stores PyPI version properties
(author, summary, license, requires_python) but does NOT extract the
wheel's `Requires-Dist` headers or sdist's setup.py/pyproject.toml
dependencies into a cross-package-queryable table.

**Desired:** A `package_dependencies` table populated on every ingest
path (upload + pull-through), enabling audit queries ("what's the
blast radius of CVE-X across my tenants?") and an admin UI surface.

**Severity:** Convenience — useful for compliance.

**Effort:** M (parse wheel `METADATA`, parse sdist `PKG-INFO` /
pyproject.toml). Same shape across formats.

**Documented?** No — unstated gap.

---

### X5. No per-tenant upstream authentication

**Current:** `TenantConfig` has no auth credential fields; the
fetcher doesn't add an `Authorization` header. Only public upstreams
are reachable. For PyPI specifically: no support for private PyPI
indices (Gemfury PyPI repos, Cloudsmith private indices, JFrog
PyPI, devpi-server).

**Severity:** Correctness for the airgapped/private-mirror use case.

**Effort:** M for cross-format infrastructure; XS per format.

**Documented?** Yes — [`upstream-pull-through.md §5.5`](upstream-pull-through.md)
defers to "PR Q." Listed for completeness.

---

## Triage view

If you're picking gaps to close, the order I'd suggest:

1. **X1 — `cache_only` mode honoring** (correctness, S each format).
2. **F2 — `yanked` field parsing + Subject attribute** (correctness, S).
   Highest-priority format-specific gap; we're silently dropping a
   PEP-defined signal that the security ecosystem actively uses.
3. **F1 — PEP 691 JSON via Accept header** (convenience, XS).
   Enabler for F2/F3 and just generally less fragile than HTML
   parsing.
4. **F3 — `requires-python` plumbed through** (convenience, XS).
   Falls out of F1 essentially for free.
5. **X2 — ETag revalidation** (convenience, M shared). Biggest
   bandwidth win for PyPI.
6. **F6 — pre/post/dev release attributes** (convenience, S).
   Useful for security-conscious shops; pairs well with F2.
7. **F5 — file_type attribute** (convenience, XS). Trivial win.
8. **X3 — `Retry-After` on 429** (convenience, XS).
9. **F4 — upstream divergence audit** (audit, M).
10. **X4 — dependency graph indexing** (convenience, M shared).
11. **X5 — per-tenant auth** — deferred to PR Q wave.

Nothing here is a release-blocker. PyPI's pull-through is the most
mature of the three formats — these are mostly papercuts on the
edges of the protocol.
