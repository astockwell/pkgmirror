# RubyGems JIT pull-through

Implementation plan to add upstream pull-through for the `rubygems`
format, matching the architecture established for `pypi` and `go`.

**Status:** drafted; not started.
**Companion docs:**
- [adding-jit-pull-through.md](../docs/adding-jit-pull-through.md) — the
  generic playbook this plan follows
- [upstream-pull-through.md](upstream-pull-through.md) — global
  architecture + threat model + per-format hazards (rubygems = §9.7)
- [implemented/created-via-package-ownership.md](implemented/created-via-package-ownership.md) —
  the `CreatedVia` contract that pull-through ingest must satisfy

---

## 1. What's there today

The rubygems registry handler already ships the upload + serve side
of the format. From [internal/packages/rubygems/handler.go](../internal/packages/rubygems/handler.go):

| Endpoint | Status | What it does |
| --- | --- | --- |
| `GET /specs.4.8.gz` | shipped | Ruby Marshal-encoded full specs index (legacy) |
| `GET /latest_specs.4.8.gz` | shipped | Ruby Marshal-encoded latest-version index |
| `GET /prerelease_specs.4.8.gz` | shipped | (empty; pkgmirror has no prerelease model) |
| `GET /info/:package` | shipped | Compact-index info file (modern bundler) |
| `GET /versions` | shipped | Compact-index versions file + md5 |
| `GET /quick/Marshal.4.8/:filename` | shipped | zlib-Marshal Gem::Specification skeleton |
| `GET /gems/:filename` | shipped | Raw `.gem` tarball download |
| `POST /api/v1/gems` | shipped | `gem push` upload |
| `DELETE /api/v1/gems/yank` | shipped | `gem yank` (quarantines version) |

The defaults entry at
[internal/upstream/defaults.go:50](../internal/upstream/defaults.go#L50)
already has the format pre-wired but with `PullThroughSupported: false`:

```go
"rubygems": {
    URL:                  "https://rubygems.org",
    Hosts:                []string{"rubygems.org", "index.rubygems.org"},
    PullThroughSupported: false, // PR H
},
```

Two read-path bugs in the current handler that this PR must also fix
(otherwise pull-through will appear to work but cooldown rules with
`time_source=upstream_publish` won't gate cached-from-upstream versions
correctly — same class of bug we just fixed for Go in commit `f3af13a`):

1. `subjectFor` ([rubygems/handler.go:678](../internal/packages/rubygems/handler.go#L678))
   does NOT surface `models.Version.UpstreamPublishedUnix` into
   `Subject.Attrs["upstream_published_unix"]`. Once pull-through is on
   and we start stamping the column on ingest, this needs to plumb
   through. Trivially copy the PyPI/Go fix.
2. `filterReadable` ([rubygems/handler.go:735](../internal/packages/rubygems/handler.go#L735))
   uses the same `subjectFor`, so it inherits the same bug.

---

## 2. Upstream protocol shape

RubyGems.org publishes both the legacy Marshal-encoded format AND the
modern compact-index format on the same origin. Per
[upstream-pull-through.md §9.7](upstream-pull-through.md), the spec
shape we care about for pull-through is the **compact index** — that's
what every modern bundler uses, and it gives us a clean per-package
miss path. The legacy `.gz` specs are full-registry indexes; we will
**not** proxy those on cold miss (see §6 below).

**Endpoints we'll call upstream:**

| Upstream URL | Pkgmirror endpoint that triggers it | Purpose |
| --- | --- | --- |
| `GET https://index.rubygems.org/versions` | `GET /versions` (cold) | Full per-package version map; used to discover what versions exist. We do NOT proxy this entire file — too large + changes constantly. We call it only when we need a "does this package exist upstream?" answer, and even then we cache. Actually: prefer per-package `/info/<name>` for that signal; reserve `/versions` for the (admin-driven) cross-registry sync flow that's out of scope for this PR. |
| `GET https://index.rubygems.org/info/<name>` | `GET /info/:package` (cold) | Per-package compact-index info. ONE round-trip gives us every version + every dep + the SHA256 of every `.gem`. **This is the primary metadata source.** |
| `GET https://rubygems.org/api/v1/gems/<name>.json` | called by cold `/info` handler | Per-package metadata API. Publish times live here (the compact `/info` lines don't carry them). Analogous to PyPI's package-level Warehouse JSON. **One round-trip per cold package** hydrates `upstream_published_unix` for every version. |
| `GET https://rubygems.org/gems/<filename>.gem` | `GET /gems/:filename` (cold) | The blob itself. SHA256-verified against the value from `/info/<name>`. |
| `GET https://rubygems.org/quick/Marshal.4.8/<filename>.gemspec.rz` | `GET /quick/Marshal.4.8/:filename` (cold, optional) | Per-gem Marshal-encoded spec. Only fetched if a client asks for it AND we don't already have the gem persisted; the Marshal output we synthesize from local metadata after ingest is fine for cached versions. |

**Endpoints we will NOT proxy:**

- `GET /specs.4.8.gz`, `GET /latest_specs.4.8.gz`,
  `GET /prerelease_specs.4.8.gz` — full-registry indexes ~tens of MB
  each; tools fetch them on every `gem update --system`. Proxying
  them would slam upstream and force us to deserialize Ruby Marshal
  just to filter through policy. Cold response for these stays
  "your local view", same as today. Tenants that want the full
  upstream index already have `index.rubygems.org` as a normal
  source on their `~/.gemrc`.
- `GET /api/v1/dependencies?gems=...` — Bundler's legacy
  dependency-graph query. Modern Bundler uses the compact index;
  pkgmirror's current rubygems handler doesn't implement this
  endpoint at all and we won't start now.

**Headers + content negotiation:**

- The compact index supports HTTP `Range` requests and `If-None-Match`
  (ETag-based). The fetcher's metadata cache already does cache-key
  -based dedup; we don't need to forward Range upstream in v1.
- All responses are `text/plain; charset=utf-8` except blobs which
  are `application/octet-stream`. No JSON content negotiation.

---

## 3. Pieces this PR adds

Following the playbook in [adding-jit-pull-through.md](../docs/adding-jit-pull-through.md):

### 3.1 Defaults flip

[internal/upstream/defaults.go](../internal/upstream/defaults.go) — change
`PullThroughSupported: false` → `true` for `rubygems`. No host list
change needed (`rubygems.org` + `index.rubygems.org` already there).

### 3.2 New file: `internal/packages/rubygems/upstream.go`

Roughly 450–550 LoC. Functions follow the PyPI shape (PyPI is the
better template here because, like PyPI, RubyGems needs a two-hop
fetch to get publish times):

| Function | Mirror of (PyPI) | Purpose |
| --- | --- | --- |
| `passthroughEnabled(c, tenant) bool` | `pypi/upstream.go:passthroughEnabled` | "Is upstream on for (tenant, rubygems)?". Identical body. |
| `fetchUpstreamInfo(c, tenant, name) (*compactIndexInfo, error)` | `pypi/upstream.go:fetchSimpleIndex` | GET `index.rubygems.org/info/<name>`, parse the compact-index lines. Returns `(versions, depsPerVersion, checksumsPerVersion, error)`. Metadata-cached. |
| `fetchUpstreamGemJSON(c, tenant, name) (*gemAPIResponse, error)` | `pypi/upstream.go:fetchPyPIJSON` | GET `rubygems.org/api/v1/gems/<name>.json`. Returns the published-at timestamps for every version. Metadata-cached. Best-effort; nil + error tolerated so cooldown falls back to ingest age rather than rejecting outright. |
| `fetchUpstreamGem(c, tenant, name, filename) (io.ReadCloser, sha256, error)` | `pypi/upstream.go:fetchUpstreamWheel` | GET `rubygems.org/gems/<filename>.gem`. Blob fetch. SHA256 verified against the value the caller passes (from `/info/<name>`). |
| `passthroughBlocked(c, tenant, name, version, pubUnix) bool` | `pypi/upstream.go:passthroughBlocked` | Build synthetic `policy.Subject` with `ingest_age_seconds=0` + (if available) `upstream_published_unix`; evaluate `ActionRead`. |
| `filterUpstreamVersions(c, tenant, name, versions, pubMap) []string` | `pypi/upstream.go:filterUpstreamVersions` | Apply per-version policy gate to the parsed `/info` lines. Uses the publish-time map from the `/api/v1/gems/<name>.json` fan-out (single round-trip for the whole package). Skips the fan-out when `Engine` is `NoopEngine`. |
| `servePullThroughInfo(c, tenant, name) bool` | `pypi/upstream.go:servePullThroughSimpleIndex` | Cold-path `/info/<name>`. Fetches upstream info + gem JSON, filters through policy, serves the filtered compact-index lines. |
| `servePullThroughGem(c, tenant, name, version, filename) bool` | `pypi/upstream.go:servePullThroughFileDownload` | Cold-path `/gems/<filename>.gem`. Pre-ingest `ActionIngest` gate → blob fetch → SHA256 verify → persist via `Service.CreatePackageOrAddFileToExisting` with `CreatedVia: CreatedViaPullThrough` → post-ingest `ActionRead` gate → serve. |
| `pullThroughIngest(...)` | `pypi/upstream.go:pullThroughIngest` | Shared helper used by `servePullThroughGem`. Persists metadata + blob + stamps `upstream_published_unix` via `Models.SetUpstreamPublishedUnix`. Idempotent. |
| `mapUpstreamErr(err) (int, string)` | `pypi/upstream.go:mapUpstreamErr` | Sentinel-to-HTTP-status. Copy verbatim. |

### 3.3 Handler wiring (`internal/packages/rubygems/handler.go`)

Small targeted changes — **no rewrite**:

1. Add `Upstream upstream.Fetcher` field on `Handler` struct.
2. Add `WithUpstream(f upstream.Fetcher) *Handler` builder method.
3. In `servePackageInfo` (line 256-ish): on `ErrPackageNotExist`,
   if `passthroughEnabled`, call `servePullThroughInfo`. If it
   returns `served=true`, return. Else fall through to current
   404.
4. In `downloadPackageFile` (line 487-ish): on `findByFilename`
   error, if `passthroughEnabled`, call `servePullThroughGem` with
   the parsed (name, version, filename). Note: parsing the filename
   back into (name, version, platform) is the only tricky bit —
   `FullFilename(name, version, platform)` is invertible if you know
   the package name, but on a cold miss we don't. **Decision:** for
   v1, accept only `<name>-<version>.gem` (no platform suffix); reject
   platform-tagged filenames with 404 on cold miss. Bundler resolving
   from `/info/<name>` always uses the platform-stripped filename for
   the default `ruby` platform, and that covers 95% of usage. Native
   gems (`<name>-<version>-x86_64-linux.gem`) are a follow-up.
5. **Do not** wire pull-through into `servePackageSpecification`
   (`/quick/Marshal.4.8/<filename>.gemspec.rz`). After step 4
   ingests the gem, the existing local handler synthesizes the
   spec from `versionMetadata` — that path already works.
6. **Do not** wire pull-through into `enumeratePackages*` (the
   `.gz` specs) or `serveVersionsFile`. See §6.
7. Fix `subjectFor` to surface `UpstreamPublishedUnix.Int64` into
   `Subject.Attrs["upstream_published_unix"]`. Match the
   `pypi/handler.go:subjectFor` pattern exactly.

### 3.4 Server wiring

[internal/server/server.go:96](../internal/server/server.go#L96) —
add `.WithUpstream(d.Upstream)` between `NewHandler(...)` and
`.Register(...)`. One line.

### 3.5 README update

[README.md:39](../README.md#L39) — flip the rubygems row's JIT
pull-through column from `planned` → `shipped`. Update the Notes
column to mention compact-index proxy + Warehouse-style two-hop
for publish times.

### 3.6 Unit tests: `internal/packages/rubygems/upstream_test.go`

Test fixture pattern: copy `pypi/upstream_test.go`'s
`newUpstreamFixture` shape verbatim, then swap the fake-upstream
mux routes:

```go
mux.HandleFunc("/info/"+packageName, ...)            // compact-index
mux.HandleFunc("/api/v1/gems/"+packageName+".json", ...) // gem JSON (publish times)
mux.HandleFunc("/gems/"+filename, ...)               // .gem blob
```

Tests to implement (mirroring the shipped PyPI + Go suites):

| Test | Pins |
| --- | --- |
| `TestPullThroughRubyGems_GemFetchesAndPersists` | Cold `/gems/<n>-<v>.gem` returns 200, blob persisted, 2nd request served from local cache, `upstream_published_unix` stamped from gem JSON. |
| `TestPullThroughRubyGems_InfoProxiedAndFiltered` | Cold `/info/<n>` proxies upstream lines. With 30d cooldown, fresh versions filtered from response. Two sub-tests: "no rules" + "30d cooldown". |
| `TestPullThroughRubyGems_DenyFreshOnGem` | Cooldown denies → 403 BEFORE blob fetch (assert blob hit counter == 0). |
| `TestPullThroughRubyGems_AllowsStale` | Older version under same rule still 200s. |
| `TestPullThroughRubyGems_QuarantinePersistsButHidesBytes` | Quarantine action: blob fetched + DB row exists, but inflight request 403s; second request also 403s from local check. |
| `TestPullThroughRubyGems_OffMode` | No `Upstream` wired → 404 with no upstream contact. |
| `TestPullThroughRubyGems_LocalResolveHonorsUpstreamPublishCooldown` | The regression-test sibling of `TestPullThroughGo_LocalResolveHonorsUpstreamPublishCooldown` (commit `f3af13a`): pull-through ingests v1.0.0, then `.gem` for v1.0.0 must STILL 200 because the local row's `upstream_published_unix` was stamped. Without the `subjectFor` fix in §3.3.7, this test fails with the exact production-style "fell back to ingest age" error. |
| `TestPullThroughRubyGems_SHA256MismatchAborts` | Fake upstream serves a `.gem` whose bytes don't match the SHA256 in `/info/<n>` → handler returns 502, NO DB row created, NO blob persisted. |
| `TestPullThroughRubyGems_UploadVsPullThroughProvenance` | A pull-through ingest writes `created_via=pull_through`. A subsequent `gem push` to the same name returns 409 (insider-shadow defense). Admin flips provenance via console → upload now succeeds. |
| `TestPullThroughRubyGems_PlatformGemRejectedOnColdMiss` | Cold request for `nokogiri-1.16.0-x86_64-linux.gem` returns 404 without upstream contact (follow-up out of scope). |

### 3.7 Integration test: `tests/integration/rubygems/canary_test.go`

`//go:build integration`. Mirrors `pypi/canary_test.go` and `go/canary_test.go`. Uses a sidecar `ruby:3.3` container:

- **Probe URL:** `https://index.rubygems.org/info/rake` (deeply stable; `rake` has been on the index for >15 years).
- **`TestRubyGemsCanary_BundleInstallViaPkgmirror`**: write a tiny `Gemfile` that depends on `rake ~> 13.0`, run `bundle install` pointing at our pkgmirror, assert exit 0 + `rake` importable.
- **`TestRubyGemsCanary_SecondInstallHitsCache`**: same Gemfile, two separate `bundle install` runs into different bundle dirs against the same pkgmirror; second run must complete without any upstream contact (assert via pkgmirror's metric counter or by killing the egress route between runs).
- **NO content-byte assertions.** `gem build` is reproducible-modulo-timestamp but the canonical RubyGems.org gem can be re-uploaded; assert on functional behavior only.

### 3.8 CI: `.github/workflows/integration.yml`

Add a `rubygems` job parallel to the existing `pypi` and `go` jobs.
Copy one of them verbatim, change format name + sidecar image
(`ruby:3.3`). Add `rubygems` to the `workflow_dispatch.inputs.format`
enum and to the `notify` job's `needs:` list.

### 3.9 Optional: `docs/demos/rubygems--script*--*.sh`

Two scripts in line with the PyPI + Go demos:

1. `rubygems--script01--bundle-install.sh` — no rules, baseline
   pull-through demo with a tiny `Gemfile`.
2. `rubygems--script02--bundle-install-cooldown.sh` — 45-day
   blanket cooldown rule, then `bundle outdated rake` to show
   which fresh patch versions got filtered.

`docs/demos/` is `.gitignored` per existing convention; these are
runnable walkthroughs, not part of the repo.

---

## 4. Implementation order

Mirroring the shipped Go pull-through PR's commit sequence so review
stays digestible:

1. **Commit 1 — handler + tests, no CI yet.**
   - `internal/upstream/defaults.go` flip
   - `internal/packages/rubygems/upstream.go` (new)
   - `internal/packages/rubygems/handler.go` (Upstream field, WithUpstream, miss-path wiring, `subjectFor` fix)
   - `internal/server/server.go` (one-line WithUpstream wiring)
   - `internal/packages/rubygems/upstream_test.go` (the full test list from §3.6)
   - `README.md` row flip
   - Commit message: `feat(rubygems): JIT pull-through against rubygems.org`
2. **Commit 2 — integration coverage.**
   - `tests/integration/rubygems/canary_test.go`
   - `.github/workflows/integration.yml` job + dispatch enum + notify
   - `docs/integration-testing.md` tree entry
   - Commit message: `test(integration): rubygems pull-through canary against real index.rubygems.org`

Both commits land before flipping the README from `planned` → `shipped`
counts as honest.

---

## 5. The non-negotiables, applied to RubyGems

From [adding-jit-pull-through.md "Non-negotiable behaviors"](../docs/adding-jit-pull-through.md#non-negotiable-behaviors):

| Rule | RubyGems realization |
| --- | --- |
| Policy runs BEFORE blob fetch | `servePullThroughGem` fetches upstream `/info/<n>` + `/api/v1/gems/<n>.json` (cheap; both metadata-cached) to get the SHA + publish time, then evaluates `ActionIngest`. Only on Allow do we touch `/gems/<filename>.gem`. |
| Policy runs AFTER ingest too | Post-`CreatePackageOrAddFileToExisting`, re-evaluate `ActionRead`. On `IsBlocked()` (quarantine action), persisted blob stays for admin promotion; inflight client gets 403. |
| Cold-path index filter | `servePullThroughInfo` MUST filter the parsed compact-index lines through `filterUpstreamVersions` using the `/api/v1/gems/<n>.json` publish-time map. Without this, a fresh version sneaks into the compact index, Bundler resolves it, then 403s on the gem download. |
| `CreatedVia` set on ingest | `pullThroughIngest` passes `CreatedVia: models.CreatedViaPullThrough` to `pkgsvc.CreationInfo`. Upload path's existing `gem push` handler (line 526-ish) must be updated to pass `models.CreatedViaUploaded` and to 409 when the existing package has `CreatedVia=pull_through`. This is the insider-shadow defense — see [implemented/created-via-package-ownership.md](implemented/created-via-package-ownership.md). |
| `mapUpstreamErr` | Copy verbatim from PyPI. RubyGems.org occasionally serves 451 for restricted gems; map that to 404 (we don't proxy restricted gems). |
| No silent egress | `h.Engine` is the audit-wrapped engine in production. Every `Engine.Evaluate` call already audits. No extra wiring needed. |
| Hash mismatch handling | The compact-index `/info/<n>` lines include a `checksum:<sha256>` extras field. After `fetchUpstreamGem` returns bytes, compute SHA256 and compare. On mismatch: abort, 502, log + audit, no DB row, no blob persisted. **Critical**: the SHA256 in the line is over the `.gem` file as-served by `index.rubygems.org/gems/<file>`, which is what we fetch — there's no "rebuild during transit" caveat here unlike PyPI wheels. |

---

## 6. What's intentionally out of scope

Documented here so a future reader doesn't try to add these "for
completeness" and accidentally invent a footgun:

1. **Full `.gz` specs index proxy.** Not viable; see §2. Local view stays correct.
2. **`/versions` cross-registry sync.** The compact-index `/versions`
   file is the global "what packages exist + their versions" map.
   Proxying it would mean re-publishing tens of MB on every request
   AND deciding how to merge it with local-uploaded packages. The
   only honest answer is a separate admin-driven "sync from upstream
   index" feature analogous to PyPI Bandersnatch; that's its own
   feature, not part of JIT pull-through.
3. **Platform gems (`-x86_64-linux.gem`).** As noted in §3.3,
   filename inversion for cold miss is ambiguous when platform is
   present. v1 ships ruby-platform only; native gems are a small
   follow-up that adds `parseGemFilename(filename) (name, version, platform)`
   reverse-parsing.
4. **`gem yank` via the API → upstream.** Yank is a write-side
   operation; we never propagate writes upstream by design. A yanked
   version locally stays yanked locally regardless of upstream state.
5. **`gem owner add/remove` proxy.** Ownership management is upstream-
   only. We expose no `/api/v1/owners/...` routes today, and won't
   start.
6. **Bundler's `/api/v1/dependencies` legacy endpoint.** Modern
   bundler uses the compact index. We don't implement it locally
   either; not worth proxying.

---

## 7. Effort estimate

Per [adding-jit-pull-through.md "Estimating effort"](../docs/adding-jit-pull-through.md#estimating-effort):

RubyGems falls in the **Medium** bucket (~5–7 hours):

- Compact index per-version parse + render is slightly more involved
  than Go's `/@v/list` (which is just newline-delimited versions).
- The two-hop (`/info` for shape + `/api/v1/gems/<n>.json` for publish
  times) is the same shape PyPI established; pattern is well-trodden.
- No URL rewriting needed in the index (the compact `/info` lines
  don't carry URLs — the client constructs `/gems/<filename>` itself).
- SHA256 verification adds ~30 LoC vs PyPI's wheel hash verify (same
  code path, different field name).
- The `subjectFor` bug fix + the regression test for it adds ~30min.

If the platform-gem reverse-parse turns out to be needed in the same
PR, add another hour.

---

## 8. Open questions

1. **Should the cold `/versions` endpoint do anything smarter than
   today?** Currently it returns only the local view. With pull-through
   on, a tenant whose `Gemfile` references many upstream-only packages
   would get a `/versions` response missing most of what their bundler
   later asks about — bundler then individually 404→pull-through-200s
   each one, which works but is chatty. Decision: defer until we see
   real-world bundler behavior; the chattiness might never matter.
2. **Should we honor the upstream `If-None-Match` / `Last-Modified`
   chain?** The metadata cache already TTL-dedupes; adding ETag
   round-trip avoidance is a possible v2 optimization. Not needed
   for correctness.
3. **The `/gems/<filename>` cold-miss filename ambiguity** —
   committing to ruby-platform-only in v1 is the right call but
   worth flagging that we'll see 404s for native gems until the
   follow-up. The error response should mention "platform gems
   not yet supported via pull-through; upload them manually with
   `gem push`" so users aren't confused.

---

## 9. Verification before flipping `planned` → `shipped`

Before the README claim is honest, all of these must be green:

- [ ] `go test ./...` passes (unit tests).
- [ ] `go test -tags integration ./tests/integration/rubygems/...`
      passes when upstream is reachable, `t.Skip`s when not.
- [ ] `make check-package-dupes` is clean.
- [ ] Manual smoke: `bundle install` against a fresh pkgmirror
      with a `Gemfile` requiring `rake`, `nokogiri` (sdist),
      `bundler`, `rails` — all install on first try, second try
      shows no upstream hits in pkgmirror logs.
- [ ] Manual smoke with cooldown rule installed: `bundle outdated`
      shows the filtered version set vs the unfiltered.
- [ ] Manual smoke: `gem push` to a name that pkgmirror already
      pulled through returns 409 with the expected provenance
      message. Admin flips via console, upload succeeds.
- [ ] CI workflow run on `main` after merge: rubygems job goes
      green (or `SKIP`s with upstream unavailable, in which case
      re-run on next cron).
