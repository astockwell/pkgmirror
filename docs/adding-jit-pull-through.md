# Adding JIT pull-through to a format

This document is the implementation playbook for adding **upstream
pull-through** to a package format that already has a working
registry/mirror handler in pkgmirror. It's the companion to
[adding-a-format.md](adding-a-format.md) — that doc covers the
upload + serve side; this one covers the "fetch on miss from a
canonical public registry, run through policy, persist, serve" side.

The two PRs already shipped (PyPI, Go) provide working code to
crib from. This doc abstracts the lessons so a third format
doesn't have to re-discover them.

Read this end-to-end before starting work. It will save you
re-inventing wheels and, more importantly, save you re-discovering
the same security gotchas the existing PRs already fixed.

---

## Mental model

A pull-through handler is a **miss-path** retrofit onto an
existing format handler. The happy path (locally-stored package)
stays byte-for-byte unchanged. The miss path gains four new
responsibilities:

1. **Decide whether to call upstream at all** (per-tenant config:
   `off` / `cache_and_serve` / `cache_only`).
2. **Translate the format's index request into one or more
   upstream HTTP calls** (per-format adapter).
3. **Run every fetched version through the policy engine on the
   cold path** (the same policy that runs on locally-uploaded
   versions runs on pulled-through versions too, but the cold
   path has to evaluate it *before* persisting and again *after*).
4. **Persist + serve**, then dedupe on subsequent requests via
   the existing local-cache fast path.

These four responsibilities are the same for every format. What
differs is the protocol shape: PyPI has a JSON simple index +
separate Warehouse API for publish times; Go has five tiny
protocol endpoints with publish time inline; npm has one giant
packument; OCI has `/v2/` with manifests + blob mounts.

---

## Required reading

Before opening your editor:

| Doc | Why |
| --- | --- |
| [plans/upstream-pull-through.md](../plans/upstream-pull-through.md) | The architecture + threat model + decision log. Especially §3 (SSRF + allowlist), §5 (fetcher API), §6 (per-tenant config), §9 (per-format notes). |
| [docs/adding-a-format.md](adding-a-format.md) | Establishes the structure the pull-through code plugs into. Skim if you've already built the upload side. |
| [docs/supply-chain.md](supply-chain.md) §"Cooldown control" + §"Package provenance" | The two policy controls your handler MUST honor on the cold path. |
| Existing PyPI pull-through ([`internal/packages/pypi/upstream.go`](../internal/packages/pypi/upstream.go) + [`internal/packages/pypi/upstream_test.go`](../internal/packages/pypi/upstream_test.go)) | The richer of the two examples — has the package-level + per-version Warehouse two-hop. |
| Existing Go pull-through ([`internal/packages/goproxy/upstream.go`](../internal/packages/goproxy/upstream.go) + [`internal/packages/goproxy/upstream_test.go`](../internal/packages/goproxy/upstream_test.go)) | The simpler example — protocol publishes inline publish time, no second-hop API needed. |

---

## The pieces every pull-through handler ships

A complete pull-through PR for one format adds exactly these things,
in roughly this order:

### 1. Defaults entry

[`internal/upstream/defaults.go`](../internal/upstream/defaults.go)
already has a `defaultUpstreams` map keyed by format. Your format
should already have an entry with `PullThroughSupported: false`.
Flip it to `true` only after the handler + tests are merging.

```go
"npm": {
    URL:                  "https://registry.npmjs.org",
    Hosts:                []string{"registry.npmjs.org"},
    PullThroughSupported: true, // your PR
},
```

If your format's blob host is separate from its index host
(PyPI's `files.pythonhosted.org`, RubyGems' `index.rubygems.org`),
both MUST be in `Hosts` — the allowlist gate is per-hostname.

### 2. `internal/packages/<format>/upstream.go` (new file)

Pattern: one file, ~300–500 LoC, containing **only** the
pull-through logic. The non–pull-through `handler.go` should not
grow much. The shape that worked for PyPI and Go:

| Function | Purpose |
| --- | --- |
| `passthroughEnabled(c, tenant) bool` | Cheap "is upstream on for this (tenant, format)?" check. Mirror it verbatim from PyPI; the body is identical. |
| `fetchUpstream<X>(...)` per protocol endpoint | One function per upstream endpoint your handler needs. Each builds an `upstream.Request`, calls `h.Upstream.Fetch`, parses the response. Best-effort; returns sentinel errors. |
| `servePullThrough<X>(c, tenant, ...)` per index/serve endpoint | The wrapper that's called from `handler.go`'s miss-path branch. Runs the policy gate(s), persists if needed, writes the HTTP response. Returns `(served bool, err error)`. |
| `pullThroughIngest(c, tenant, ...)` | The shared "fetch blob + persist via Service.CreatePackageOrAddFileToExisting" routine. Idempotent: a second call on an already-ingested version is a no-op. |
| `mapUpstreamErr(err) (int, string)` | Translates `upstream.ErrUpstream*` sentinels into the right HTTP status + short message body. Copy from PyPI. |
| Format-specific filter helpers (e.g. `passthroughBlocked`, `filterUpstreamVersions`) | Build the synthetic `policy.Subject` for not-yet-ingested versions; run them through the engine. |

### 3. `handler.go` miss-path wiring

The existing handler grows three small things, **not** a rewrite:

1. An `Upstream upstream.Fetcher` field + `WithUpstream(f)` constructor method.
2. A `passthroughEnabled(...)` guard in each index/serve handler that's currently `c.String(404, ...)` on a local miss — branch into the new `servePullThrough<X>` before returning 404.
3. Inside the per-version handlers (`info`, `mod`, `zip`, etc. for Go; `/files/<name>/<version>/<filename>` for PyPI), the same guard + branch.

### 4. `internal/server/server.go` wiring

One line: pass `d.Upstream` through to your handler's `WithUpstream`.
Already present for every format that has the field — pattern is
`format.NewHandler(...).WithUpstream(d.Upstream).Register(...)`.

### 5. Unit tests (httptest fake upstream)

A test file (`internal/packages/<format>/upstream_test.go`) that
stands up:

- A real pkgmirror via `httptest.NewServer(serverFromDeps)` with
  an upstream fetcher pointed at:
- A fake "the.canonical.registry" via `httptest.NewServer(mux)`
  serving exactly the protocol endpoints your handler calls.
- A real `policy.ChainEngine` with whatever rules each test needs.

**Required test coverage** — every shipped pull-through PR has
all of these, plus format-specifics:

| Test | What it pins |
| --- | --- |
| `Test*_FetchesAndPersists` | Cold miss returns 200, blob persisted, second request served from local cache, `upstream_published_unix` stamped if available. |
| `Test*_IndexProxiedAndFiltered` | Cold index proxy works; with an active cooldown rule, fresh versions are filtered out (use sub-tests for "no rules" + "30d cooldown"). |
| `Test*_DenyFresh` | Policy `deny` short-circuits BEFORE the blob fetch (assert blob-fetch counter == 0). |
| `Test*_AllowsStale` | Same rule lets older versions through. |
| `Test*_QuarantinePersistsButHidesBytes` | Policy `quarantine` action: blob IS fetched + persisted (admin-promotable) but inflight request gets 403, and second request also gets 403 served from local check. |
| `Test*_OffMode` | No `Upstream` wired (or `Mode=off`) returns 404 without upstream contact. |

For PyPI/Go these tests live in `upstream_test.go` and
`upstream_policy_test.go` — split as you prefer; both files run
under the same default build tag.

### 6. Internet-connected integration test

[docs/integration-testing.md](integration-testing.md) covers the
suite shape. Add `tests/integration/<format>/canary_test.go`
(build tag `integration`) with these properties:

- **`//go:build integration`** at the top — never run on push/PR;
  scheduled weekly + on `workflow_dispatch`.
- **A `<format>ProbeURL` constant** pointing at a deeply-stable
  upstream URL. The preflight calls
  `integration.RequireReachable(t, probeURL)` which `t.Skip`s
  (not fails) when upstream is unreachable.
- **Two or three canary tests** that exercise the *real* upstream
  via a sibling container running the real client (`pip`, `go`,
  `npm`, `gem`, etc.) pointed at our pkgmirror instance.
- **One must be `Test*_SecondGetHitsCache`** — two clients, same
  pkgmirror, no error lines in the second install. Proves the
  blob persistence + cache-hit path actually works against real
  upstream content (not just our httptest fixture).
- **No content-byte assertions.** Wheels get rebuilt; tarballs
  get re-tarred. Assert "install succeeded" + "module importable",
  never "wheel has exactly N bytes".

Then wire it into [`.github/workflows/integration.yml`](../.github/workflows/integration.yml):

1. Add a new job parallel to `pypi` and `go`. Copy one of those
   verbatim and change the format name + client image.
2. Add the format to the `workflow_dispatch.inputs.format` enum
   options.
3. Add the format to the `notify` job's `needs:` list so a failure
   auto-files a tracking issue.

### 7. `README.md` + `docs/integration-testing.md` updates

| File | Change |
| --- | --- |
| `README.md` | The per-format table's JIT pull-through column for your format flips `planned` → `shipped`. |
| `docs/integration-testing.md` | Add a line to the architecture tree under `tests/integration/` listing your new `<format>/canary_test.go`. |

### 8. `docs/demos/<format>--script*--*.sh` (optional)

The python + go demos in `docs/demos/` are runnable single-format
walkthroughs that stand up a fresh pkgmirror in a tmux session
and echo the user-runnable commands. Worth adding one for any
format that has a memorable demo flow (private mirror story,
cooldown story, etc.). Note: `docs/demos/` is `.gitignored`, so
these scripts live in working trees only by design.

---

## Non-negotiable behaviors

These are documented in [plans/upstream-pull-through.md §13.1](../plans/upstream-pull-through.md)
as "rules this plan DOES enforce." Every pull-through handler MUST
honor them.

### Policy runs BEFORE the blob fetch

Cold pull-through of a version that policy denies must NOT contact
the upstream blob endpoint. The order is:

1. Fetch upstream metadata (cheap; needed for publish time / version
   discovery).
2. Build the synthetic `policy.Subject` with `ingest_age_seconds=0`
   and (if available) `upstream_published_unix`.
3. Call `engine.Evaluate(ActionIngest)`.
4. On `Decision >= Deny`: write a 403 with the policy reason. No
   blob fetch.

The "synthetic Subject" is the key idea: cold-path versions don't
have a `package_versions` row yet, so you build the Subject the
same way a post-ingest read would, with `ingest_age_seconds=0`
(matching what the row WILL have once persisted).

### Policy runs AFTER ingest too

A `quarantine`-action rule needs the bytes persisted (so admin can
promote) but the inflight client must NOT get them. The pattern:

1. Pre-ingest: gate on `ActionIngest` (only short-circuits on `Deny`).
2. Persist via `Service.CreatePackageOrAddFileToExisting`.
3. Post-persist: gate on `ActionRead`. On `IsBlocked()`, return 403.
   The blob + DB rows stay; admin's quarantine workflow can promote.

This is the fix shipped in [pypi/upstream.go's quarantine path](../internal/packages/pypi/upstream.go) — both PyPI and Go follow this pattern.

### Cold-path `/index/` filter (not just `/files/`)

If your format has a list-of-versions index endpoint (PyPI's
`/simple/<name>/`, Go's `/<module>/@v/list`, npm's packument,
etc.), the cold-path version of that endpoint MUST evaluate each
upstream version through the policy engine and filter blocked
ones out. Otherwise the client sees fresh versions in the index,
locks one into its resolver, and only discovers the policy when
the blob 403s — too late to back out.

For cooldown rules with `time_source: upstream_publish`, this
filter needs publish times for every upstream version. PyPI does
one package-level Warehouse JSON fetch (`/pypi/<name>/json`) to
hydrate `upstream_published_unix` for every version in a single
upstream round-trip. Go has the time inline in `/@v/<v>.info`.
Whichever shape your format has, the cold-path filter MUST have
a way to populate this attribute or `time_source: upstream_publish`
won't fire on first sight.

When the engine is `policy.NoopEngine` (tests, dev with no rules),
skip the publish-time fan-out — it's a wasted round-trip whose
only purpose is to feed an evaluator that won't reject anything.

### Set `CreatedVia` on pull-through ingest

Per [plans/implemented/created-via-package-ownership.md](../plans/implemented/created-via-package-ownership.md),
your pull-through ingest path MUST pass `CreatedVia:
models.CreatedViaPullThrough` to `pkgsvc.CreationInfo`. Without
this, an attacker who registers a same-named package on the
public registry could shadow a tenant-uploaded package via the
`/index/` merge that follows.

The upload path of the same format should already pass
`models.CreatedViaUploaded` (mirror the PyPI upload handler in
[internal/packages/pypi/handler.go](../internal/packages/pypi/handler.go)).
And it should refuse uploads to packages with
`CreatedVia=pull_through` with a 409 — that's the insider-shadow
defense.

### Errors map to the right HTTP status

The `upstream.ErrUpstream*` sentinel set is exhaustive. Use the
same `mapUpstreamErr` shape PyPI and Go use:

```go
case errors.Is(err, upstream.ErrUpstreamOff),
     errors.Is(err, upstream.ErrUpstreamNotFound):
    return http.StatusNotFound, "not found"
case errors.Is(err, upstream.ErrUpstreamRateLimit):
    return http.StatusTooManyRequests, "upstream pull-through rate limit"
case errors.Is(err, upstream.ErrUpstreamTooLarge):
    return http.StatusBadGateway, "upstream response exceeded size limit"
case errors.Is(err, upstream.ErrUpstreamForbidden):
    return http.StatusBadGateway, "upstream URL not permitted (allowlist or private-IP guard)"
case errors.Is(err, upstream.ErrUpstreamTimeout):
    return http.StatusGatewayTimeout, "upstream pull-through timed out"
default:
    return http.StatusBadGateway, "upstream pull-through failed: " + err.Error()
```

A 5xx leaked from `Upstream.Fetch` that you forget to map will
get rendered as `panic recovered: ...` by the Gin recovery
middleware — not what you want on a flaky pypi.org Friday.

### No silent egress

Every `Upstream.Fetch` call produces an audit row when the engine
is wrapped with `audit.WrapEngine` (cmd/pkgmirror wires this by
default). You don't need to write extra audit code; just make
sure your handler routes its evaluations through the same
`h.Engine` field everyone else uses.

### Hash mismatch handling

When upstream supplies a hash (PyPI: sha256 in the simple-index URL fragment; npm: integrity in packument; etc.), verify it before
calling `CreatePackageOrAddFileToExisting`. On mismatch, abort the
ingest, return a 502, log + audit. Do NOT silently accept the
bytes — the hash is the only thing standing between us and a
mid-flight tamper.

---

## Format-specific gotchas (from the shipped PRs)

### PyPI

- Two upstream hosts in the allowlist: `pypi.org` (index) +
  `files.pythonhosted.org` (blobs). The default registry sometimes
  redirects between them; the fetcher's allowlist needs both.
- Warehouse JSON (`/pypi/<name>/<version>/json`) is the only way
  to get publish times. The fan-out is package-level
  (`/pypi/<name>/json`) for the cold `/simple/` filter and
  per-version (`/pypi/<name>/<version>/json`) for the
  `/files/` ingest path. Cache both via the fetcher's metadata
  cache.
- PEP 691 JSON vs PEP 503 HTML — the upstream might serve either.
  Our adapter parses both. The content-negotiation header is what
  the Accept header asks for; pip/uv negotiate JSON.

### Go modules

- Five protocol endpoints (`/@v/list`, `/@v/<v>.info`,
  `/@v/<v>.mod`, `/@v/<v>.zip`, `/@latest`); each needs a
  pull-through helper.
- `.info`'s `Time` field IS the upstream publish time — no
  second-hop. Easier than PyPI.
- `@latest` is mutable metadata; cache with a short TTL
  (5min default in `internal/upstream/config.go`).
- **sumdb is intentionally not proxied in v1.** Operators leave
  `GOSUMDB=sum.golang.org` (its default). Document this if you
  add a Go demo. A `sum.golang.org` passthrough is a separate
  ~0.5-day PR.
- Pseudo-versions like `v0.0.0-20240515123045-abc123def456`
  resolve via `proxy.golang.org` for free — no extra work.

### npm (not yet shipped — drafted in plan)

- The packument is a single giant JSON document with `dist.tarball`
  URLs that need rewriting to point at our `/-/<...>/-/<...>.tgz`
  endpoint. Don't miss `dist-tags`.
- `npm publish` writes to a different endpoint than `npm install`
  reads — the gate goes in both places.
- npm's `time` map on the packument has per-version publish times;
  no second-hop needed.

---

## Common pitfalls (debugged once, written down)

### "But the merge is supposed to show new versions when I disable the rule!"

The local `/index/` handler USED to be local-only when the package
existed locally. The current handler merges local + upstream when
the package's `created_via='pull_through'`. If you're seeing
local-only behavior despite expecting a merge, check the
package's provenance in the console — a hand-uploaded package is
sealed from the merge by design. See
[plans/implemented/created-via-package-ownership.md](../plans/implemented/created-via-package-ownership.md).

### "Pull-through fetched the wheel but the test sees zero hits"

The `httptest` fake's hit counter only fires on the routes you
registered. If your handler is making an upstream call you didn't
expect (most commonly: a metadata fetch where you assumed a blob
fetch), add a catch-all `/` 404 handler with a `t.Errorf` so you
see the unmocked path.

### "The cooldown rule denies, but the metadata cache still serves the version"

The metadata cache stores upstream RESPONSE bytes, not policy
decisions. If you cache the upstream `/index/` response and serve
it as-is, you bypass the per-version policy filter. Always
re-evaluate policy on every request after pulling from the
metadata cache.

### "uv resolved to a denied version anyway"

The cold `/index/` filter runs at request time, not at version-
ingest time. If a cached upstream `/index/` response from before
the rule was added is still being served, the filter won't fire
for it. Either: (a) re-filter on every serve (the current
pattern, simple), or (b) invalidate the metadata cache when
rules change (more efficient, more complex). PyPI + Go both do
(a) — the metadata cache holds the upstream body, the handler
re-runs the filter on every request.

### "My tests pass but the integration suite fails"

The integration suite uses a real registry that occasionally:

- Yanks the version your canary pinned.
- Rebuilds the wheel with different bytes.
- Rate-limits the test runner's IP.
- Changes its content-negotiation behavior under heavy load.

Pin canaries to deeply-boring versions of deeply-stable packages
(`six`, `pip`, `rsc.io/quote`). Never assert on bytes. If a
real-network test goes red, **first** check the workflow log
for `SKIP` vs `FAIL` — preflight skips are upstream's problem,
fails are ours.

### "I forgot to flip `PullThroughSupported=true` in defaults.go"

Easy mistake; defaults.go is in `internal/upstream/`, not
`internal/packages/<format>/`. Your tests will pass (they don't
read defaults.go) but the README format-status table is now
lying. Grep for `PullThroughSupported: false` to find it.

---

## Recommended PR shape

For consistency with the shipped PyPI + Go PRs:

| File set | Commit |
| --- | --- |
| `internal/packages/<format>/upstream.go` + `handler.go` changes + `internal/server/server.go` line + `internal/upstream/defaults.go` flip + `README.md` row | `feat(<format>): JIT pull-through against <upstream>` |
| `tests/integration/<format>/canary_test.go` + `.github/workflows/integration.yml` job + `docs/integration-testing.md` tree entry | Same commit; integration coverage is part of the deliverable, not a follow-up. |

Both shipped pull-through PRs (`6101315` for Go,
`feat(pypi): gate cold pull-through paths through policy engine`
for PyPI) landed everything together — handler + tests + CI in
one commit. Doing it that way ensures the workflow + canary land
before the format reads "shipped" in the README.

---

## Estimating effort

Two data points, both single-engineer sessions:

| Format | Hours | Notes |
| --- | --- | --- |
| PyPI | ~12 | First pull-through; included building the `internal/upstream/` foundation. Subsequent formats reuse it. |
| Go | ~3 | Half the effort of PyPI. Five tiny protocol endpoints; publish time inline; no URL rewriting in the index. |

Realistic budget for a new format: **4–8 hours of focused work**,
heavily dependent on protocol complexity:

- **Easy** (3–4h): Go-like — small protocol endpoints, publish time
  inline. Examples: Maven (POM metadata is small), CRAN (PACKAGES
  files are flat text).
- **Medium** (5–7h): PyPI-like — JSON index + separate metadata API
  for publish times + URL rewriting. Examples: npm (packument),
  NuGet (service index).
- **Hard** (full plan, possibly multi-PR): OCI/Container, signed
  index formats (Alpine APKINDEX, Debian Release, RPM repomd).
  See [plans/upstream-pull-through.md §9](../plans/upstream-pull-through.md)
  for the per-format hazards.

---

## See also

- [adding-a-format.md](adding-a-format.md) — the upload + serve
  side of adding a format.
- [plans/upstream-pull-through.md](../plans/upstream-pull-through.md) —
  the full architecture + threat model + PR sequence.
- [supply-chain.md](supply-chain.md) — cooldown, license allowlist,
  package provenance; the controls your handler MUST honor.
- [integration-testing.md](integration-testing.md) — the
  weekly-cron suite shape + etiquette policy.
- [internal/packages/pypi/upstream.go](../internal/packages/pypi/upstream.go) +
  [internal/packages/goproxy/upstream.go](../internal/packages/goproxy/upstream.go) —
  the two shipped reference implementations.
