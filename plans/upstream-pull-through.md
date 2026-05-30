# Upstream pull-through caching

**Status:** ready for review and incremental implementation.

**Scope:** add JIT (just-in-time) pull-through caching to every package
format pkgmirror serves today, so a `pip install`, `npm install`,
`go get`, `docker pull`, etc. against a fresh pkgmirror tenant
**works without anyone having pre-pushed the package** — pkgmirror
fetches from the canonical public upstream on miss, runs it through
the supply-chain policy engine, persists, and serves.

**Out of scope (deferred):** federated multi-upstream mirroring
(`upstream A → upstream B → ...`), replication, deduplication across
tenants (existing blob dedup is unchanged), pull-through for the
Generic format, and the OCI/Container format which is significant
enough to deserve its own plan.

## Read first

The cooldown evaluator already names the gap this plan closes — see
[internal/policy/cooldown/cooldown.go:9-13](../internal/policy/cooldown/cooldown.go#L9-L13)
referring to a future `upstream_published_unix` source. This plan
ships that. The web-console policy/quarantine UI already exists and
needs only minor extensions (per-tenant upstream config screen).

---

## 1. Recommendation

**Pull-through ON by default**, per-tenant + per-format configurable,
fetching from a curated **default allowlist** of canonical public
registries. Three modes per (tenant, format):

| Mode | Behavior |
| --- | --- |
| `off` | Never reach upstream. Misses 404 immediately. (Current behavior.) |
| `cache_and_serve` | **Default.** On miss: fetch upstream, run policy engine, persist, serve. Subsequent requests hit the cache. |
| `cache_only` | On miss: fetch upstream, run policy engine, persist as **quarantined**, 404 the client. Operator promotes via the existing quarantine UI before serving. Most defensive. |

A tight default allowlist (`pypi.org`, `registry.npmjs.org`,
`proxy.golang.org`, `rubygems.org`, `repo.maven.apache.org`,
`api.nuget.org`, `cran.r-project.org`, `dl-cdn.alpinelinux.org`,
`deb.debian.org`, `dl.fedoraproject.org`) is shipped in code. Operators
can extend via env (`PKGMIRROR_UPSTREAM_ALLOWED_HOSTS=...`) or override
the canonical URL per-tenant in the web console.

Out of the box on a fresh install, `pip install requests` against the
default tenant Just Works. Operators turn it off (or switch to
`cache_only`) tenant-by-tenant as they need more control.

---

## 2. Why this matters (the actual product win)

Three things become true once pull-through ships:

1. **The cooldown rule actually defends against fresh upstream releases.**
   Today the cooldown clock ticks from local push time, which only helps
   in workflows where someone proactively pushed. With pull-through +
   `upstream_published_unix`, the rule fires on first-sight of a
   brand-new PyPI release and matches the semantics of
   `uv --exclude-newer` / Renovate's `minimumReleaseAge`.

2. **The blocklist rule can quarantine a malicious version before any
   user in the org installs it.** Today an attacker compromise of a
   well-known package only gets blocked if a human has been watching
   advisories and pushed a deny rule before someone runs `pip install`
   — and the install would 404 anyway because the version isn't on
   pkgmirror yet. With pull-through, the deny rule fires on first
   fetch attempt, audit log captures the attempted ingest, no bytes
   reach the client.

3. **The end-user demo story (`uv pip install` against pkgmirror)
   becomes one command instead of an operator-side push dance.** This
   is what the upcoming `uv` demo needs.

These are the three things `devpi` and Verdaccio give npm/Python users
today. pkgmirror's differentiator is the policy gate on the fetch path
— neither devpi nor Verdaccio enforce per-version policy as the bytes
flow through.

---

## 3. Threat model

Adding outbound HTTP from pkgmirror to arbitrary upstreams is a real
change. The threats and mitigations are non-negotiable parts of this
plan.

### 3.1. SSRF (server-side request forgery)

The threat: an admin (or an attacker who compromises an admin token)
configures a tenant upstream like `http://10.0.0.5:6379/` or
`http://169.254.169.254/latest/meta-data/` and uses pkgmirror as a
proxy to reach internal services.

Mitigations (defense-in-depth, all default-on):

1. **Hostname allowlist** — default to a fixed list of canonical public
   registry hostnames per format. Configuration to extend the list
   lives in `PKGMIRROR_UPSTREAM_ALLOWED_HOSTS=registry.internal.corp`
   at server-launch time, NOT in tenant config. An admin cannot expand
   the allowlist via the web console.
2. **RFC1918 / loopback / link-local block** — even if a host is on
   the allowlist, the resolved IP must not be in `10.0.0.0/8`,
   `172.16.0.0/12`, `192.168.0.0/16`, `127.0.0.0/8`, `169.254.0.0/16`,
   `::1/128`, `fc00::/7`. Operator override:
   `PKGMIRROR_UPSTREAM_ALLOW_PRIVATE_IPS=true` (for self-hosted
   intermediate-mirror deployments).
3. **No redirects to non-allowlisted hosts** — HTTP client follows
   3xx redirects only to hosts also on the allowlist. Counts against
   a max-hop budget (default 5).
4. **HTTPS-only by default** — `http://` upstream URLs rejected unless
   `PKGMIRROR_UPSTREAM_ALLOW_PLAINTEXT=true`.

### 3.2. Cache poisoning

The threat: an attacker races a request, pushes a malicious version
with the same name+version before the legitimate fetch completes.

Mitigations:

1. **Single-flight per (tenant, format, canonical-key)** — only one
   upstream fetch is in flight at a time for a given identity tuple.
   Concurrent clients wait for the single result. (Implementation:
   `golang.org/x/sync/singleflight`.)
2. **Hash verification where the format provides it** — PyPI ships
   `#sha256=...` in PEP 503 index lines; npm packuments ship
   `dist.shasum` + `dist.integrity`; Go modules ship `.info`/`.mod`
   hashes via the sumdb; Maven ships `.sha1`/`.sha256`/`.md5`
   companion files. Per-format adapters compute the hash on the fetched
   bytes and abort the fetch if it doesn't match what the upstream
   metadata claimed.
3. **No write path to package_versions from anywhere except the
   single-flight fetcher** — uploads still go through the existing
   handlers; pull-through and upload coexist but each owns a non-
   overlapping (name, version) namespace per request. If both arrive
   at the same time, the second loses to a unique-constraint error;
   no merge.

### 3.3. Resource exhaustion

The threat: an attacker triggers fetches of huge artifacts to fill
disk, exhaust memory, or DoS upstream.

Mitigations:

1. **Per-fetch size cap** —
   `PKGMIRROR_UPSTREAM_MAX_BYTES_PER_FETCH=1073741824` (default 1 GiB).
   Stream-with-cap; abort + clean up the partial blob if exceeded.
2. **Per-fetch timeout** — `PKGMIRROR_UPSTREAM_FETCH_TIMEOUT=300s`
   (5 min) for the whole transfer; `30s` for connect + TLS handshake.
3. **Per-tenant rate limit on upstream fetches** —
   `PKGMIRROR_UPSTREAM_FETCH_RPM_PER_TENANT=120` (2 per second avg,
   bursty). Trips a 429 on the *client* request; the tenant just
   sees their installs slow down briefly.
4. **Storage quota per tenant** — out of scope for this plan
   (tracked elsewhere), but the pull-through code MUST check the
   existing quota check before persisting (it does already).

### 3.4. Information leak via metadata

The threat: pkgmirror's outbound requests reveal what packages the org
is interested in to the upstream registry and any network observer.

Mitigations: nothing technical here — operators who care should run
pkgmirror behind their corporate egress proxy (the Go HTTP client
honors `HTTPS_PROXY` by default; document this prominently). For
operators who really need it, `PKGMIRROR_UPSTREAM_USER_AGENT=...` lets
them set a generic UA instead of `pkgmirror/<version>` to reduce
attribution.

### 3.5. Egress policy push-back from security teams

Some shops will not allow pkgmirror outbound at all. They get the
default-off-per-tenant escape hatch and full audit trail:
`PKGMIRROR_UPSTREAM_DEFAULT_MODE=off` flips the global default to
`off`, requiring per-tenant opt-in.

---

## 4. Architecture overview

```
┌─────────────────────────────────────────────────────────────┐
│  client (uv, pip, npm, docker, ...)                         │
└─────────────────────────────────────────────────────────────┘
                          │ GET
                          ▼
┌─────────────────────────────────────────────────────────────┐
│  /api/packages/:tenant/<format>/...    (existing handlers) │
│                                                             │
│  1. resolve tenant                                          │
│  2. local lookup → HIT? serve from blob store               │
│  3. MISS? → ask internal/upstream/Fetcher.Fetch(...)        │
└─────────────────────────────────────────────────────────────┘
                          │
                          ▼
┌─────────────────────────────────────────────────────────────┐
│  internal/upstream/Fetcher                                  │
│                                                             │
│  - resolve (tenant, format) → upstream config               │
│  - check mode: off → return ErrUpstreamOff                  │
│  - check rate limit                                         │
│  - single-flight key: (tenant, format, canonical_key)       │
│  - build per-format upstream URL via Adapter                │
│  - allowlist check on the URL                               │
│  - HTTP client with limits/timeout/redirect-allowlist       │
│  - return io.ReadCloser + ContentMeta                       │
└─────────────────────────────────────────────────────────────┘
                          │
                          ▼
┌─────────────────────────────────────────────────────────────┐
│  back in the format handler:                                │
│                                                             │
│  - stream-and-tee: bytes go to client AND blob store        │
│  - on close: invoke policy engine on the persisted blob     │
│    (deny ⇒ delete blob + 404; quarantine ⇒ mark; allow ⇒ ok)│
│  - write package_versions row with upstream_published_unix  │
│    (when metadata provides it) and the resolved blob_id     │
│  - audit row: action=pull_through, decision, upstream_url   │
└─────────────────────────────────────────────────────────────┘
```

### Why this shape

- **Fetcher is format-agnostic**; per-format adapters only know how to
  build the upstream URL and how to extract `upstream_published_unix`
  from the response. Everything else (HTTP, single-flight, rate limit,
  allowlist, persist) is shared. This keeps per-format PRs small.
- **Policy runs on persisted bytes**, not on the streamed-through bytes.
  Two reasons:
  - The current policy engine takes a `Subject` populated from DB rows,
    not a stream. Reshaping it to be stream-y is much bigger work.
  - Persist-then-evaluate-then-decide-serve matches the quarantine
    forensics requirement: even denied bytes get persisted (with a
    `REJECTED:` marker per the existing flow) so an operator can audit
    what the attacker tried to push through.
- **Stream-and-tee** (the client gets bytes as they arrive at our
  server) is what every other pull-through cache does and is the only
  thing that makes "first install" feel reasonable. We use Go's
  `io.TeeReader` + `pipe` to fan one read into one client-bound write
  and one blob-store write.

---

## 5. The shared `internal/upstream/` package

### 5.1. Fetcher surface

```go
package upstream

import (
    "context"
    "io"
    "net/url"
)

// Fetcher resolves an upstream fetch for a (tenant, format,
// canonical_key) and returns a streaming reader plus content meta.
// On hit-from-cache it returns a Result with FromCache=true so the
// caller doesn't double-persist.
type Fetcher interface {
    Fetch(ctx context.Context, req Request) (*Result, error)
}

type Request struct {
    TenantID   int64
    Format     string  // "pypi", "npm", etc.
    Kind       Kind    // Metadata or Blob
    UpstreamPath string // adapter-built path relative to upstream root
    // canonical_key uniquely identifies this resource for single-flight.
    // e.g. "pypi:default:requests:2.32.4:.whl"
    CanonicalKey string
}

type Kind int
const (
    KindMetadata Kind = iota // index pages, packuments, repomd.xml, etc.
    KindBlob                  // .whl, .tgz, .gem, .rpm, etc.
)

type Result struct {
    Body            io.ReadCloser  // streamed bytes; caller MUST Close
    ContentType     string
    ContentLength   int64          // -1 if unknown
    ETag            string         // for metadata cache headers passthrough
    LastModified    time.Time      // for metadata revalidation
    UpstreamURL     *url.URL       // for audit log
    UpstreamPublishedUnix int64    // 0 if not available from upstream
    FromCache       bool           // metadata only; true if served from
                                   // in-memory metadata cache
}

// Sentinel errors:
var (
    ErrUpstreamOff       = errors.New("upstream pull-through disabled")
    ErrUpstreamNotFound  = errors.New("upstream returned 404")
    ErrUpstreamRateLimit = errors.New("upstream rate-limit tripped (local)")
    ErrUpstreamTooLarge  = errors.New("upstream response exceeded max size")
    ErrUpstreamForbidden = errors.New("upstream host not on allowlist")
)
```

### 5.2. Single-flight

`golang.org/x/sync/singleflight.Group` keyed on `CanonicalKey`. The
returned `*Result` is shared across waiters. The fetcher's `Body` is
read once into a `bytes.Buffer` for blobs under a low-water mark
(default 32 MiB) and shared via independent readers; for larger blobs
the single-flight key serializes the fetch and waiters retry from the
now-populated cache. (Avoids the alternative of multi-reader piping
which has subtle backpressure bugs.)

### 5.3. HTTP client policy

One shared `*http.Client` per pkgmirror process, configured with:

- `Timeout: 0` — per-request deadline via context (see size+time caps)
- `Transport` with explicit limits:
  - `MaxIdleConns: 100`, `MaxIdleConnsPerHost: 10`
  - `DialContext` wrapped to reject private IPs (see §3.1)
  - `TLSClientConfig.MinVersion: tls.VersionTLS12`
- `CheckRedirect` rejects hops to non-allowlisted hosts and trips
  after `MaxRedirects` (default 5).
- `User-Agent` set to `pkgmirror/<version> (+https://pkgmirror.example)`
  unless overridden by env.

### 5.4. Hostname allowlist

Compiled into the binary as a starting set; extensible via env. Per
format, with the canonical upstream URL also baked in:

```go
// internal/upstream/defaults.go
var defaultUpstreams = map[string]UpstreamDefault{
    "pypi": {
        URL:   "https://pypi.org",
        Hosts: []string{"pypi.org", "files.pythonhosted.org"},
    },
    "npm": {
        URL:   "https://registry.npmjs.org",
        Hosts: []string{"registry.npmjs.org"},
    },
    "go": {
        URL:   "https://proxy.golang.org",
        Hosts: []string{"proxy.golang.org", "sum.golang.org"},
    },
    "rubygems": {
        URL:   "https://rubygems.org",
        Hosts: []string{"rubygems.org", "index.rubygems.org"},
    },
    "maven": {
        URL:   "https://repo.maven.apache.org/maven2",
        Hosts: []string{"repo.maven.apache.org", "repo1.maven.org"},
    },
    "nuget": {
        URL:   "https://api.nuget.org/v3/index.json",
        Hosts: []string{"api.nuget.org"},
    },
    "cran": {
        URL:   "https://cran.r-project.org",
        Hosts: []string{"cran.r-project.org"},
    },
    "alpine": {
        URL:   "https://dl-cdn.alpinelinux.org/alpine",
        Hosts: []string{"dl-cdn.alpinelinux.org"},
    },
    "debian": {
        URL:   "https://deb.debian.org/debian",
        Hosts: []string{"deb.debian.org", "security.debian.org"},
    },
    "rpm": {
        URL:   "https://dl.fedoraproject.org",
        Hosts: []string{"dl.fedoraproject.org",
                       "mirrors.fedoraproject.org"},
    },
    // generic + container excluded; see scope.
}
```

Note: some formats (PyPI especially) split index hostname
(`pypi.org`) from blob hostname (`files.pythonhosted.org`) — both
need to be allowlisted.

### 5.5. Auth handling

Per-tenant, per-format auth is supported but **not** in the first
wave (no consumer of the canonical public registries needs it).
Schema reserves columns for `auth_kind` (none / bearer / basic) and
`auth_credential` (encrypted with the existing session-encryption
key). Per-tenant private upstreams (JFrog/Nexus/Artifactory) arrive
in a later wave alongside the web-console UI for entering credentials.

### 5.6. Audit + observability

Every fetch attempt produces one `audit_log` row:

| Column | Value |
| --- | --- |
| `action` | `pull_through` |
| `decision` | `allow` / `quarantine` / `deny` / `error` |
| `format` | the format |
| `tenant_id` | the tenant |
| `package` | canonical name |
| `version` | the version (or `""` for metadata-only fetches) |
| `extra_json` | `{"upstream_url": "...", "bytes": N, "from_cache": bool, "duration_ms": N}` |

Plus a `slog` Info line at the application logger with the same fields
for ops streaming.

A small Prometheus-style counter set is in scope as a follow-up; not
in this plan.

---

## 6. Per-tenant, per-format configuration

### 6.1. Schema v5

```sql
-- migration v5
CREATE TABLE tenant_upstreams (
    tenant_id        INTEGER NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    format           TEXT    NOT NULL,
    mode             TEXT    NOT NULL DEFAULT 'cache_and_serve',
                     -- 'off' | 'cache_and_serve' | 'cache_only'
    upstream_url     TEXT,   -- NULL = use compiled default for format
    metadata_ttl_sec INTEGER NOT NULL DEFAULT 300,
    auth_kind        TEXT,   -- NULL | 'bearer' | 'basic'
    auth_credential  TEXT,   -- encrypted via session enc key
    updated_unix     INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, format)
);

-- versions now capture the upstream publish time when pull-through
-- ingests them. Existing rows stay NULL.
ALTER TABLE package_versions
    ADD COLUMN upstream_published_unix INTEGER;
```

When no row exists for (tenant_id, format) → use the compiled defaults
(mode = global default = `cache_and_serve`, URL = the canonical, etc.).

### 6.2. Global config defaults (env vars)

| Var | Default | Effect |
| --- | --- | --- |
| `PKGMIRROR_UPSTREAM_DEFAULT_MODE` | `cache_and_serve` | Per-tenant override wins |
| `PKGMIRROR_UPSTREAM_ALLOWED_HOSTS` | _(empty)_ | Comma-separated; appended to compiled allowlist |
| `PKGMIRROR_UPSTREAM_ALLOW_PRIVATE_IPS` | `false` | If true, RFC1918 + loopback OK |
| `PKGMIRROR_UPSTREAM_ALLOW_PLAINTEXT` | `false` | If true, `http://` upstreams accepted |
| `PKGMIRROR_UPSTREAM_FETCH_TIMEOUT` | `300s` | Whole-transfer deadline |
| `PKGMIRROR_UPSTREAM_CONNECT_TIMEOUT` | `30s` | TCP + TLS handshake |
| `PKGMIRROR_UPSTREAM_MAX_BYTES_PER_FETCH` | `1073741824` | 1 GiB |
| `PKGMIRROR_UPSTREAM_FETCH_RPM_PER_TENANT` | `120` | Per-tenant rate limit |
| `PKGMIRROR_UPSTREAM_USER_AGENT` | _(default UA)_ | Override |
| `PKGMIRROR_UPSTREAM_METADATA_CACHE_MAX_BYTES` | `268435456` | 256 MiB in-memory metadata cache (LRU) |

### 6.3. Web console UI

New page `/console/tenants/:name/upstreams` shows a table:

| Format | Mode | Upstream URL | Metadata TTL | Auth | Status |
| --- | --- | --- | --- | --- | --- |
| pypi | cache_and_serve | https://pypi.org | 5 min | none | active |
| npm | cache_only | https://registry.npmjs.org | 5 min | none | active (quarantines all) |
| go | off | — | — | — | disabled |
| ... | ... | ... | ... | ... | ... |

Per-row edit form: mode (radio), upstream URL (text, validated against
allowlist + URL parser), metadata TTL (number, 0–86400), auth fields.
"Reset to default" button per row.

Mounted under the existing `RequireSystemAdmin` group (per-tenant
admins later if we add the role).

---

## 7. Per-format adapter pattern

Each format's pull-through code is a small file in
`internal/packages/<format>/upstream.go` that implements:

```go
// Adapter knows how to translate a per-format protocol path to an
// upstream URL, and how to extract upstream-published-time and hash
// info from the upstream response.
type Adapter interface {
    // Kind decides cache strategy.
    Kind(path string) Kind

    // BuildURL returns the upstream URL for a path our handler
    // received. Empty string means "this path has no upstream
    // representation" (e.g. our own per-tenant indexes that we
    // generate locally).
    BuildURL(base *url.URL, path string) string

    // CanonicalKey for single-flight de-dup. e.g. "pypi:requests:2.32.4:wheel".
    CanonicalKey(path string) string

    // VerifyAndExtract is called AFTER fetch, BEFORE persist. Returns
    // the upstream publish time (0 if unknown) and any hash for
    // verification. Returning a non-nil error aborts the fetch and
    // logs.
    VerifyAndExtract(body []byte, expected ExpectedFromMetadata) (publishedUnix int64, err error)
}
```

The handler-side wiring (in each format's `Handler` struct) becomes a
small `tryUpstream(c *gin.Context, path string)` method called from
the existing `download` / `proxy` / etc. miss paths. ~30 lines per
format on top of the existing code.

### 7.1. Metadata cache (in-memory LRU)

Lives in `internal/upstream/metadata_cache.go`. Keyed on
`(tenant_id, format, path)`. Stores `(bytes, ETag, last_modified,
fetched_at)`. Default TTL from tenant config; respects
`Cache-Control: max-age=` from upstream when present (capping at the
configured max).

On revalidation (TTL expired): conditional GET with `If-None-Match` /
`If-Modified-Since`. 304 → bump fetched_at, return cached bytes. 200
→ replace cache entry. Other → log and return cached bytes (degraded
mode; better than going dark when upstream hiccups).

### 7.2. Blob cache (persistent, via existing blob store)

Blobs are persisted via the existing `internal/storage/` blob writer
on first fetch and looked up on subsequent requests by their normal
local row (the local row now exists because pull-through wrote it).
**Blobs are never expired by the pull-through subsystem** — the
existing cleanup-rule machinery is the right place to garbage collect.

---

## 8. Policy engine integration

### 8.1. When the engine runs

The policy engine runs **after** the upstream fetch completes and the
bytes are persisted to the blob store, but **before** the `package_versions`
row is committed and **before** the bytes are served to the client.

Sequence for a miss-path fetch:

1. Single-flight wins → upstream HTTP GET → stream to temp blob in
   `BLOB_DIR/tmp/`
2. After the body is fully received: compute hash, verify against
   any upstream-provided checksum (per-format adapter)
3. Build `policy.Subject` with the just-fetched metadata, set
   `Attrs["ingest_age_seconds"] = 0` and (when known)
   `Attrs["upstream_published_unix"]`
4. `engine.Evaluate(ctx, subject, ActionRead)` → Decision
5. Switch on decision:
   - `allow` → move blob to permanent storage, insert `package_versions`
     row (with `upstream_published_unix`), insert blob row, audit
     `decision=allow`, return body to client
   - `quarantine` → same as allow but mark `quarantine_reason = "policy:..."`,
     audit `decision=quarantine`, 404 the client (or serve if mode is
     `cache_and_serve` per existing serve-while-quarantined rules —
     ops decision; default 404)
   - `deny` → discard blob, do NOT insert version row, audit
     `decision=deny`, 404 with reason in the response body (helps
     `pip` show a useful error)
6. In `cache_only` mode: same as above but quarantine the row even on
   `allow` decision (the operator still needs to promote)

### 8.2. Cooldown evaluator update

Add the `time_source` knob from the existing cooldown.go comment:

```go
type Config struct {
    MinAgeDays int    `json:"min_age_days"`
    TimeSource string `json:"time_source,omitempty"`
    // "ingest" (default; current behavior)
    // "upstream_publish" (requires upstream_published_unix populated)
}
```

When `time_source: upstream_publish` and `upstream_published_unix == 0`
(can't determine), fall back to `ingest` and log a warning. Don't
silently break.

### 8.3. Existing quarantine flow

Pull-through-fetched-then-quarantined versions appear in the existing
`/console/quarantine` page as normal. The console's promote button
clears `quarantine_reason` exactly as it does today.

---

## 9. Format-by-format protocol notes

Per-format details that affect the adapter. Read these before
implementing each format's PR.

### 9.1. PyPI (PEP 503)

- **Upstream**: `https://pypi.org/simple/<name>/` for index,
  `https://files.pythonhosted.org/...` for blobs (different host;
  both on allowlist)
- **Index format**: HTML (PEP 503) or JSON (PEP 691); accept both.
  Each line/entry has `href="<blob-url>#sha256=<hash>"`.
- **Cache**: per-package index = metadata + short TTL; .whl/.tar.gz
  = blob + permanent.
- **Hash**: PEP 503 ships SHA-256 in URL fragment. Verify on fetch.
- **Upstream publish time**: not in the simple index. Use the
  Warehouse JSON API as a second hop: `GET https://pypi.org/pypi/<name>/<version>/json`,
  read `urls[].upload_time_iso_8601`. Cache the result.
- **URL rewriting**: blob URLs in the index served to clients are
  REWRITTEN from `https://files.pythonhosted.org/...` to
  `http://<our-host>/api/packages/<tenant>/pypi/files/<name>/<version>/<filename>`
  so the client downloads through us (and we cache + audit).

### 9.2. RubyGems

- **Upstream**: `https://rubygems.org/` (with `https://index.rubygems.org/`
  for the compact index, which newer Bundler prefers)
- **Index format**: `Marshal.4.8.gz` (binary) for old clients, compact
  index (`/info/<name>` text format) for new ones. Both supported.
- **Cache**: specs / compact-info = metadata + short TTL; .gem = blob
  permanent.
- **Hash**: SHA-256 in the compact index. Verify.
- **Upstream publish time**: gemspec includes `date`; also available
  via the JSON API `/api/v1/versions/<name>.json`.

### 9.3. Go modules

- **Upstream**: `https://proxy.golang.org/` (and `https://sum.golang.org/`
  for sumdb, proxy as-is)
- **Index format**: `/<module>/@v/list`, `/<module>/@v/<version>.info`,
  `/<module>/@v/<version>.mod`, `/<module>/@v/<version>.zip`
- **Cache**: `@latest`, `@v/list` = metadata short TTL;
  `.info`/`.mod`/`.zip` = immutable blob.
- **Hash**: not inline; sumdb is a separate trust mechanism. Pass-
  through sumdb requests as-is.
- **Upstream publish time**: `.info` JSON has `Time` field.

### 9.4. Maven

- **Upstream**: `https://repo.maven.apache.org/maven2/` (Central);
  some shops want Spring/Google/JBoss — handled via additional rows
  in `tenant_upstreams` (NOT v1; v1 only supports a single upstream
  per format-per-tenant).
- **Index format**: `maven-metadata.xml` per groupId/artifactId,
  artifact GAV path layout.
- **Cache**: `maven-metadata.xml` short TTL; jar/pom/sources immutable.
- **Hash**: `.sha1`/`.sha256`/`.md5` companion files. Fetch + verify
  the primary, also fetch and store the hash files.

### 9.5. npm

- **Upstream**: `https://registry.npmjs.org/`
- **Index format**: packument JSON at `/<name>` or
  `/<scope>/<name>`. Contains `versions` map with `dist.tarball` URLs.
- **Cache**: packument short TTL; tarball immutable.
- **Hash**: `dist.shasum` (SHA-1) + `dist.integrity` (SRI) in
  packument. Verify SRI on fetch.
- **URL rewriting**: critical. The `dist.tarball` URLs in the packument
  we serve to the client MUST point at our pkgmirror, not at
  `registry.npmjs.org`. Adapter rewrites them.
- **Upstream publish time**: packument's `time` map keyed by version.

### 9.6. NuGet (V3)

- **Upstream**: `https://api.nuget.org/v3/index.json` (the service
  index; contains URLs for registration / search / package endpoints)
- **Index format**: service index JSON, then per-resource endpoints
- **Cache**: service index + registration short TTL; .nupkg immutable.
- **Hash**: V3 has SHA-512 in the registration; verify.
- **URL rewriting**: the service index returned to clients must point
  at our pkgmirror endpoints, not at api.nuget.org. Same as npm.

### 9.7. CRAN

- **Upstream**: `https://cran.r-project.org/`
- **Index format**: `PACKAGES` / `PACKAGES.gz` / `PACKAGES.rds` flat
  files per `src/contrib/` and `bin/<platform>/contrib/<rversion>/`
- **Cache**: PACKAGES short TTL; source / binary archives immutable.
- **Hash**: MD5 in PACKAGES file. Verify.
- **Upstream publish time**: DESCRIPTION file inside the archive has
  `Packaged:` — expensive to extract; v1 leaves
  `upstream_published_unix` NULL for CRAN.

### 9.8. Alpine (apk)

- **Upstream**: `https://dl-cdn.alpinelinux.org/alpine/`
- **Index format**: `APKINDEX.tar.gz` per branch/repo/architecture.
  Signed (`.SIGN.RSA.*` inside).
- **Cache**: APKINDEX short TTL; .apk immutable.
- **Signature**: we pass through APKINDEX as-is (signed by Alpine).
  Local indexes for first-party uploads remain a separate workflow.
- **MERGE problem**: if a tenant has BOTH pull-through Alpine packages
  AND first-party uploads, the served APKINDEX needs to merge both
  sets while keeping the upstream signature valid for the upstream
  subset. **v1 punts**: a tenant with first-party Alpine uploads
  cannot enable pull-through (boot-time validation). v2 may add
  index merging with our own re-signing.

### 9.9. Debian (apt)

- **Upstream**: `https://deb.debian.org/debian/`
- **Index format**: `Release` / `Release.gpg` / `InRelease` /
  `Packages` / `Packages.gz` / `Packages.xz` per distribution +
  component + arch.
- **Cache**: Release+Packages short TTL; .deb immutable.
- **Signature**: pass-through (signed by Debian).
- **MERGE problem**: same as Alpine; v1 punts same way.

### 9.10. RPM

- **Upstream**: distribution-specific. For demo, `https://dl.fedoraproject.org/...`
- **Index format**: `repodata/repomd.xml` then `primary.xml.gz`,
  `filelists.xml.gz`, `other.xml.gz`
- **Cache**: repodata short TTL; .rpm immutable.
- **Signature**: pass-through.
- **MERGE problem**: same as Alpine/Debian; v1 punts.

### 9.11. Container / OCI

**Not in scope for this plan.** Container pull-through is
significantly more complex than the other formats combined:

- Multi-arch manifest indexes (need to translate per-arch digests)
- Blob mounts (`POST /v2/.../blobs/uploads/?mount=<digest>&from=<repo>`)
- Range requests for blob resume
- Foreign layers (`urls` in image manifests pointing to non-registry
  hosts — security implications)
- Sigstore signatures + Cosign attestations (separate refs that may
  or may not exist)
- Per-arch dedup across upstreams

Container pull-through deserves its own plan doc when there's
specific user demand.

### 9.12. Generic

**Not in scope.** Generic format is by definition "we don't know
what this is"; operators using it manage their own files. No
canonical upstream exists.

---

## 10. PR sequence

Following the web-console pattern: small, independently shippable PRs,
each green on `go test ./...` and the existing blackbox conformance
suites.

| # | PR | Estimate |
| --- | --- | --- |
| **A** | **Plan doc** (this file), `DECISIONS.md` entry stub | 0 (this PR) |
| **B** | Schema v5: `tenant_upstreams` + `package_versions.upstream_published_unix` + migration test | 0.5 day |
| **C** | `internal/upstream/` foundation: Fetcher interface, HTTP client, allowlist+IP guard, single-flight, metadata cache, sentinel errors, ~200 lines of unit tests with `httptest.NewTLSServer` | 1.5 days |
| **D** | `internal/upstream/config.go` — env loading, tenant override resolver, defaults table | 0.5 day |
| **E** | Web console UI: `/console/tenants/:name/upstreams` page (list + edit), `tenant_upstreams_test.go` | 1 day |
| **F** | **PyPI pull-through** (the demo deliverable): per-format adapter, handler miss-path wiring, URL rewriting in the index, Warehouse JSON two-hop for `upstream_published_unix`, end-to-end test against a stubbed pypi.org + a real-network smoke test marked `-short=false` | 1.5 days |
| **G** | Cooldown rule `time_source` knob + tests | 0.5 day |
| **H** | RubyGems pull-through | 1 day |
| **I** | CRAN pull-through | 0.5 day |
| **J** | Go modules pull-through (incl. sumdb proxy-as-is) | 1 day |
| **K** | Maven pull-through (incl. hash companion files) | 1 day |
| **L** | npm pull-through (incl. packument URL rewriting) | 1.5 days |
| **M** | NuGet pull-through (incl. service-index URL rewriting) | 1.5 days |
| **N** | Alpine pull-through (pass-through signed indexes; boot-validate no first-party uploads coexist) | 1 day |
| **O** | Debian pull-through | 1 day |
| **P** | RPM pull-through | 1 day |
| **Q** | Per-tenant private upstreams (auth_kind / auth_credential) + web console form fields | 1 day |
| **R** | Container/OCI plan doc (separate file) | TBD |

**~14 days of focused work** for full coverage minus container.
**~4 days** gets you PRs A–G — the foundation + PyPI + cooldown
upgrade — which is the demo deliverable.

PRs A–F can land in a single sprint. PRs H–P can land in parallel
once F is merged (each format is independent).

---

## 11. Decisions log (the 5 questions, resolved)

| # | Decision | Resolution |
| --- | --- | --- |
| 1 | Default mode | **`cache_and_serve` on by default**, per-tenant + per-format configurable. Global override via `PKGMIRROR_UPSTREAM_DEFAULT_MODE=off` for shops that won't allow egress. |
| 2 | Hostname allowlist | **Curated allowlist compiled in**; extensible via `PKGMIRROR_UPSTREAM_ALLOWED_HOSTS` at server-launch time only (not via web console). Private-IP block default on. |
| 3 | Per-tenant modes | **Three modes**: `off`, `cache_and_serve`, `cache_only`. `cache_only` is the most defensive (auto-quarantine on fetch). |
| 4 | Caching semantics | **Short TTL + ETag/304** for metadata; immutable blobs cached permanently via existing blob store. Per-tenant TTL override; respect upstream `Cache-Control: max-age=` capped at configured max. |
| 5 | Cooldown interaction | **Capture `upstream_published_unix` on ingest** (where the format provides it; NULL otherwise). Cooldown rule gets a `time_source: ingest \| upstream_publish` knob (defaults to `ingest` for backwards compat). |

Additional clarifications:

- Egress proxy: support via `HTTPS_PROXY` env (Go HTTP client honors
  it by default). Document.
- Auth to upstream: schema reserves columns from v1, web console form
  fields land in PR Q (later).

---

## 12. Risks and unknowns

- **Upstream rate limits / IP bans**: pypi.org has aggressive
  rate-limiting on `pip` user-agent patterns. Our User-Agent will
  identify pkgmirror clearly; if we exceed limits, individual
  pkgmirror deployments could get blocked. Mitigation: built-in
  per-host rate-limit (separate from the per-tenant fetch limit) +
  exponential backoff on 429.
- **Hash mismatch handling**: when upstream's claimed hash doesn't
  match the fetched bytes, we currently abort + log + audit
  `decision=error`. Is this a `deny` audit instead? Probably yes;
  decision in PR F.
- **Metadata cache growth**: a malicious actor could request many
  non-existent packages to pollute the metadata cache. Default
  cache size is 256 MiB LRU, but combined with the 1 GiB max
  blob fetch, a tenant under attack could pin memory. Per-tenant
  metadata cache quota is a follow-up.
- **Cache poisoning via concurrent push**: covered by §3.2 but
  there's a subtle window where someone pushes `requests==2.32.4`
  while pkgmirror is mid-fetching the same from PyPI. UNIQUE
  constraint on `(package_id, lower_version)` makes one of them
  fail; the failed one's bytes go to forensic blob storage. This
  is acceptable but worth testing.
- **Format-specific URL-rewriting bugs**: npm packuments and NuGet
  service indexes have nested URL fields; missing one means the
  client bypasses pkgmirror for some sub-resource. Per-PR test
  coverage must include "all served URLs point at pkgmirror" as
  an explicit assertion.
- **The MERGE problem for Alpine/Debian/RPM**: boot-time
  validation (a tenant with first-party uploads can't enable
  pull-through for the same format) is a real UX regression for
  operators who want both. v2 plan: index-merging with our own
  re-signing. Track separately.
- **Storage cost**: pull-through fills the blob store with
  upstream packages on first access. Existing cleanup-rule
  machinery (Forgejo-style) handles retention. Operators should
  understand: pull-through = bigger disk. Documentation duty.

---

## 13. What this plan deliberately does NOT specify

These are decided during implementation by the engineer doing the
work. Listed here so reviewers don't ask:

- Exact `User-Agent` string format (pick something reasonable)
- Exact `slog` field names (use the existing convention from the
  console package's S3.2)
- Exact 404/429 body wording (succinct + actionable; match the
  per-format error idioms — `pip` parses certain things)
- Whether the metadata cache lives in memory or on disk (start
  in-memory LRU; revisit if cache size becomes a real concern)
- Exact retry backoff curve for upstream errors (start with
  exponential 1s/2s/4s, max 3 retries; tune as needed)
- Whether to expose a manual `/console/tenants/:name/upstreams/<format>/refresh`
  button (useful for ops; not a v1 must-have)

### 13.1. Rules this plan DOES enforce

- **No silent egress.** Every upstream fetch produces an audit row.
- **No unverified bytes served.** Where the format provides a hash,
  we verify before serving.
- **No SSRF surface widening via tenant config.** Allowlist is global
  + binary-compiled or env-set; not editable from the web console.
- **No bypass of the policy engine.** Policy runs on every fetched
  version before serve, same as on uploaded versions today.
- **No corruption of existing handlers.** Pull-through wires into the
  miss-path only; the happy path (locally-stored package) is
  byte-for-byte unchanged.
- **No double-fetch under concurrency.** Single-flight per
  `(tenant, format, canonical_key)`.
- **No private-IP egress without explicit operator opt-in.**

---

## See also

- [plans/web-console.md](web-console.md) — the architecture plan format this
  document mirrors.
- [plans/supply-chain-security-brainstorm.md](supply-chain-security-brainstorm.md)
  — the original motivating doc for the policy engine that pull-through
  hooks into.
- [internal/policy/cooldown/cooldown.go](../internal/policy/cooldown/cooldown.go) —
  the existing comment naming `upstream_published_unix` as a future
  addition that this plan ships.
