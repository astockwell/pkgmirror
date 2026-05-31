# Multi-tenancy: is it actually viable, and at what cost?

**Status:** decision note / brainstorm. Not a plan to *change* anything
yet — the analysis here may inform a future "scale back multi-tenancy"
plan, or a future "lean in to multi-tenancy as a SaaS" plan, but
nothing in `internal/` should change as a result of this file alone.

**Authored:** 2026-05-31, in response to a "do we still want
multi-tenancy at all?" gut-check.

---

## The question

The original pkgmirror design was multi-tenant from day one, motivated
by a hypothetical SaaS shape: host one instance ourselves, sell
"private mirror as a service" to many customers, each isolated as a
tenant.

The gut-check question: *does that even work?* The intuition was that
package toolchains (pip, npm, go, etc.) wouldn't accept a
path-prefixed URL like `pkgmirror.com/tenant-2/...`, and that real
SaaS multi-tenancy would require either subdomains per tenant (with
all the DNS + wildcard-cert + provisioning overhead that implies) or
abandoning the model entirely.

## The short answer

The premise is wrong. **Path-prefixed multi-tenancy works for every
format we ship except OCI.** Our own PyPI demo
([docs/demos/python.md](../docs/demos/python.md)) proves it:

```toml
[tool.uv]
index-url = "http://admin:pkm_demotoken@localhost:8080/api/packages/default/pypi/simple/"
```

The `default` segment in that URL **is** the tenant name. The route is
mounted in
[`internal/server/server.go`](../internal/server/server.go) as

```go
// Format-specific API groups: /api/packages/:tenant/<format>/...
apiBase := r.Group("/api/packages/:tenant", tokenAuth)
```

and `uv sync` accepts it without complaint. The same is true for
`pip --index-url`, `pip --extra-index-url`, `.pypirc`, etc.

This is the standard pattern for SaaS package mirrors — Artifactory,
Nexus, Cloudsmith, and GitHub Packages all do path-as-tenant on a
single shared host with one wildcard-free TLS cert. No DNS plumbing,
no per-tenant subdomain provisioning.

## Per-format compatibility matrix

| Format | Client config knob | Accepts path-prefixed tenant? |
| --- | --- | :---: |
| `pypi` | `pip --index-url`, `[tool.uv] index-url`, `.pypirc` `repository` | yes |
| `npm` | `.npmrc` `registry=` (global or scoped) | yes |
| `go` | `GOPROXY=https://host/path,direct` | yes |
| `rubygems` | `gem sources --add`, `Gemfile` `source` | yes |
| `maven` | `settings.xml` `<url>` | yes |
| `nuget` | `dotnet nuget add source`, `nuget.config` `<add value=...>` | yes |
| `cran` | `repos = c(REPO = "https://host/path/")` | yes |
| `alpine` | `/etc/apk/repositories` line | yes |
| `debian` | `sources.list` `deb https://host/path …` | yes |
| `rpm` | `.repo` `baseurl=` | yes |
| `generic` | our own contract | yes |
| `container` (OCI) | image ref `host/path:tag`, registry must answer `/v2/` at the **host root** | **no** (see below) |

OCI is the only genuine exception. The
[Distribution Spec](https://github.com/opencontainers/distribution-spec)
hard-codes `/v2/` at the registry root; a client that's told
`docker pull pkgmirror.com/tenant-2/myimage:v1` will issue
`GET pkgmirror.com/v2/tenant-2/myimage/manifests/v1`, not
`GET pkgmirror.com/tenant-2/v2/...`. Path-prefix-as-tenant doesn't
exist for OCI.

### What OCI multi-tenancy actually looks like in practice

The big public OCI registries (Quay, Docker Hub, GHCR) solve this by
treating the *image-name prefix* as the tenant:

- `ghcr.io/<owner>/<image>:tag`
- `docker.io/<library_or_user>/<image>:tag`
- `quay.io/<organization>/<image>:tag`

There's one shared `/v2/` endpoint; "tenant" is a namespace inside the
image name. That works fine and would let pkgmirror host OCI for
many tenants on one host. The cost is that the tenant identifier
isn't structurally separated from the other URL segments the way it
is for every other format we ship — it's just "the first path
component after `/v2/`" by convention. We'd need to teach the OCI
handler that "first path component → tenant lookup" instead of
"`:tenant` path param from the router."

This is implementable; it's a small adapter inside the OCI handler,
not a re-think of the multi-tenant model.

## So path-prefixed multi-tenancy is viable. What's the actual cost?

The original question conflated two things: "does the URL shape
work?" (yes) and "is multi-tenancy worth the operational cost?" (a
real, separate question worth thinking about). The operational costs
are where the real friction lives.

### 1. Shared SQLite, shared blob store, single failure domain

The whole stack today lives in one SQLite DB and one filesystem blob
root under `DATA_DIR`. Two consequences:

- **Noisy-neighbor risk.** One tenant doing a huge sync can starve
  every other tenant of write locks (SQLite is single-writer at the
  DB-file level). This is a real scale ceiling, not a theoretical
  one.
- **Single backup blast radius.** A corrupted DB, a bad migration,
  or an `rm -rf $DATA_DIR/blobs` affects every tenant at once. No
  per-tenant isolation at the storage layer.

For self-hosted "one team, one tenant" deployments this doesn't
matter. For SaaS-style "many customers, one instance" it's a
material constraint that has to be either accepted, papered over
(read replicas, per-tenant sharding), or replaced (Postgres + object
storage). See
[`DECISIONS.md`](../DECISIONS.md) §"Revisit when: we need multi-node
/ HA" — already flagged.

### 2. Content-addressed blob store deduplicates *across* tenants

Blobs are stored by their SHA-256 under `<root>/aa/bb/aabbcc…`. Two
tenants that both pull `requests-2.32.5.whl` share one blob on disk.

This is:

- **Good** for cost (a few-MB wheel stored once, not N times).
- **Surprising** when an operator quarantines a version in tenant A
  expecting "the bytes are now hidden" — they're not, tenant B still
  serves them via *its own* `package_versions` row pointing at the
  same blob. (The hiding is correct per-tenant; the *bytes* aren't
  blast-radius-isolated.) This is semantically the right model — a
  policy decision in one tenant shouldn't drag another tenant's
  workflow — but it's worth being explicit about in docs.
- **A privacy consideration** if "tenant" is meant to model
  competitors-on-the-same-host. Knowing that the SHA of a private
  wheel is `abc123…` and timing a request to test whether it's
  already cached is a real (small, but real) side channel.

For a SaaS shape we'd want either:

- Per-tenant blob namespaces (lose the dedupe; gain the isolation).
- Or accept the dedupe + add per-tenant ACLs at the blob-open layer
  (already true — `checkRead` runs before `OpenFile`).

The status quo is fine for trusted "different teams in the same
company" tenancy. For "competitors share a host" tenancy it needs
more thought.

### 3. Per-tenant signing keys for alpine / debian / rpm

These formats ship signed indices (APKINDEX.tar.gz with an RSA
signature, Release/InRelease with an OpenPGP signature, repomd.xml
with `repomd.xml.asc`). Each tenant gets its own key, and the public
key has to be installed on every client machine that consumes that
tenant's repo.

For self-hosted "one tenant" use this is a one-time pain. For SaaS
it's a recurring per-tenant onboarding step that has to be
documented + tooled. Doable, but not free.

### 4. Cross-tenant policy reuse doesn't exist

A `policy_rules` row has `tenant_id`. A rule with `tenant_id IS NULL`
applies globally; with `tenant_id = N` applies only to that tenant.
There's no "this rule applies to tenants A, B, and C." That's the
right primitive for most cases, but a SaaS operator who wants to
ship a "baseline" policy bundle to all paying customers (and not
non-paying ones) has to denormalize: insert N rows, one per tenant.
Not a blocker; worth a future "rule groups" or "policy templates"
concept if SaaS becomes the real shape.

### 5. The OCI quirk above

If OCI matters and "first path component as tenant" is unacceptable,
we'd need subdomain-per-tenant for OCI specifically. That *does*
push DNS + cert plumbing into the picture, but only for the OCI
host, not the others. Hybrid is fine: `pkgmirror.com/api/packages/<tenant>/<format>/`
for everything except OCI, and `registry-<tenant>.pkgmirror.com/v2/`
for OCI.

## Where this leaves us

The technical viability of multi-tenant SaaS via path-prefix is
solid for everything except OCI. The decision of whether to keep
investing in multi-tenancy as a *product shape* is not blocked by
URL routing — it's blocked by:

- Do we actually want to operate a SaaS? (operational ongoing cost,
  on-call, billing, support).
- Are we comfortable shipping the shared-SQLite + shared-blob-store
  constraints to paying customers, knowing the scale ceiling?
- Is the OCI subdomain-or-namespace-prefix split acceptable?

If we *don't* want to operate SaaS, multi-tenancy still has value
for self-hosted shops with multiple teams who want isolated
namespaces inside one pkgmirror instance — that's a strictly
smaller, lower-risk version of the SaaS shape and the current
implementation is already a good fit.

If we want to *step away* from multi-tenancy and go single-tenant,
the cost is real but bounded:

- Strip the `tenant_id` foreign keys from every table (or default
  them to a hardcoded `1` and stop exposing tenant management).
- Strip `:tenant` from every route group.
- Update every doc / demo / `pyproject.toml` example to drop the
  tenant segment.
- The `tenants` table itself can stay as a stub if we ever want to
  bring it back.

Reversibility cost: medium. Not a one-line change, but not a
ground-up rewrite either. The schema, routing, and tests would all
need touching.

## Open questions

1. Do we keep multi-tenancy as a first-class feature for the
   self-hosted-many-teams use case?
2. If yes, do we ever want to operate it as SaaS? If yes,
   what's the answer to the shared-DB scale ceiling — Postgres
   port? Per-tenant SQLite shards? Read replicas?
3. For OCI specifically, is "first-path-component-as-tenant" (Quay
   / GHCR pattern) acceptable, or do we want to do the
   subdomain-per-tenant work?

## What this file is NOT

- A plan to rip out multi-tenancy.
- A plan to launch a SaaS.
- A commitment to ship OCI subdomain routing.

It's a decision note that the path-prefix shape works, and that the
real costs of multi-tenancy live in the storage / operations /
policy-tooling layers — not the URL routing layer.
