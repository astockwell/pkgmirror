# Multi-tenancy

pkgmirror is multi-tenant from the ground up: a single instance can host
many isolated namespaces (tenants), each with its own packages, tokens,
users, policy rules, and signing keys. This page describes the **URL
routing model** that makes that work for real package clients, the
per-format compatibility matrix, and the one place where the model
hits a wall (OCI).

For the tenant + user + token + permissions data model, see
[auth.md](auth.md). For the storage characteristics that fall out of
running many tenants in one process, see [storage.md](storage.md).

## The routing model

Every tenant-scoped request carries the tenant name as a path segment:

```
/api/packages/<tenant>/<format>/…
/t/<tenant>/p/<type>/<name>          (UI)
```

Routes are mounted accordingly in
[`internal/server/server.go`](../internal/server/server.go):

```go
// Format-specific API groups: /api/packages/:tenant/<format>/...
apiBase := r.Group("/api/packages/:tenant", tokenAuth)
```

One pkgmirror process can serve any number of tenants. There is no DNS
plumbing, no per-tenant subdomain, no wildcard TLS cert. A single
TLS cert for the pkgmirror hostname covers every tenant.

This is the same shape Artifactory, Nexus, Cloudsmith, and GitHub
Packages use to operate as multi-tenant SaaS on one host.

## Why this works for real client tools

Every mainstream package toolchain accepts an arbitrary URL as its
index / registry. The tenant segment in the URL is invisible to the
client — it's just part of "where the registry lives." When a Python
developer writes:

```toml
[tool.uv]
index-url = "https://pkgmirror.example.com/api/packages/acme/pypi/simple/"
```

…uv has no idea that `acme` is a tenant. It just sees an HTTP URL
that speaks PEP 691. The same is true for `pip`, `npm`, `go`,
`gem`, `mvn`, `dotnet`, `apk`, `apt`, `dnf`, `R`, etc.

## Per-format compatibility

| Format | Client config knob | Tenant lives in URL? | Example (tenant `acme`) |
| --- | --- | :---: | --- |
| `pypi` | `pip --index-url`, `[tool.uv] index-url`, `.pypirc` `repository` | yes | `https://host/api/packages/acme/pypi/simple/` |
| `npm` | `.npmrc` `registry=` (global or scoped) | yes | `https://host/api/packages/acme/npm/` |
| `go` | `GOPROXY=…,direct` | yes | `https://host/api/packages/acme/go/,direct` |
| `rubygems` | `gem sources --add`, `Gemfile` `source` | yes | `https://host/api/packages/acme/rubygems/` |
| `maven` | `settings.xml` `<url>` | yes | `https://host/api/packages/acme/maven/` |
| `nuget` | `dotnet nuget add source`, `nuget.config` `<add value=…>` | yes | `https://host/api/packages/acme/nuget/index.json` |
| `cran` | `repos = c(REPO = "…")` in `Rprofile` or `install.packages()` | yes | `https://host/api/packages/acme/cran/` |
| `alpine` | `/etc/apk/repositories` line | yes | `https://host/api/packages/acme/alpine/<branch>` |
| `debian` | `sources.list` `deb …` line | yes | `https://host/api/packages/acme/debian <suite> <component>` |
| `rpm` | `.repo` `baseurl=` | yes | `https://host/api/packages/acme/rpm/<branch>` |
| `generic` | our own contract | yes | `https://host/api/packages/acme/generic/<name>/<version>/<filename>` |
| `container` (OCI) | image ref `host/path:tag` | **see below** | `host/acme/myimage:v1` (image-prefix model) |

See [demos/python.md](demos/python.md) for a runnable end-to-end
example of the PyPI case.

## The OCI nuance

OCI is the one format that *cannot* express the tenant as a path
segment under a common API root. The
[Distribution Spec](https://github.com/opencontainers/distribution-spec)
hard-codes `/v2/` at the **host root**. A client told to pull
`pkgmirror.example.com/acme/myimage:v1` issues:

```
GET pkgmirror.example.com/v2/acme/myimage/manifests/v1
```

…not `pkgmirror.example.com/acme/v2/myimage/...`. There is no place
in the request shape to put `acme` as a path-prefix the way every
other format allows.

### How public OCI registries solve this

The three large public OCI registries all treat the **first path
component after `/v2/`** as the tenant / namespace:

| Registry | Tenant identifier |
| --- | --- |
| Docker Hub | `docker.io/<library_or_user>/<image>:tag` |
| GitHub Container Registry | `ghcr.io/<owner>/<image>:tag` |
| Quay | `quay.io/<organization>/<image>:tag` |

There's one `/v2/` endpoint; "tenant" is a namespace inside the
image name. pkgmirror's OCI handler follows the same convention: an
image pull at `/v2/<tenant>/<image>/manifests/<ref>` resolves the
tenant from the first path segment under `/v2/`. From the OCI
client's perspective, "the tenant" is just part of the image name.

The trade-off: an OCI tenant identifier isn't structurally
separated from the other URL segments the way it is for every other
format. The tenant lookup happens inside the OCI handler rather
than via a router-level `:tenant` parameter. The behavior is the
same; the parsing is one layer deeper.

### When subdomain-per-tenant would be the right call

If an operator needs each OCI tenant to "own" its own host root —
e.g. because they want each tenant to have its own
authentication realm, or because the OCI registry has to look like
a private corporate registry under that tenant's brand — then
subdomain-per-tenant (`registry-acme.pkgmirror.example.com/v2/…`)
is the cleanest answer. That does require DNS + wildcard TLS, and
it can be hybrid: subdomains for OCI only, path-prefix for
everything else.

This is not how pkgmirror is configured out of the box. The
default is the path-prefix / image-name-prefix model described
above.

## What multi-tenancy buys you

The two intended deployment shapes are:

1. **Self-hosted, many teams in one company.** One pkgmirror
   process; each team gets a tenant; tokens, packages, and policy
   rules don't bleed across teams. Cheaper than running N
   pkgmirror processes, and centralized for ops.
2. **SaaS-style, many customers on one host.** One pkgmirror
   instance hosts isolated mirrors for unrelated organizations.
   The path-prefix routing means no DNS or cert provisioning per
   customer.

Both shapes use the same mechanics. The differences are in how you
operate the host (backups, on-call, capacity planning) and in how
much isolation each tenant requires.

## Things to know about how tenants share the underlying system

These aren't "bugs in multi-tenancy" — they're consequences of the
single-process, single-storage architecture. They're called out
here so operators know what shape of isolation they're getting.

### Storage is shared, content-addressed blobs deduplicate across tenants

All tenants live in one SQLite DB and one filesystem blob root
under `DATA_DIR`. Blobs are stored by their SHA-256 (see
[storage.md](storage.md)), so two tenants that both pull
`requests-2.32.5.whl` share a single on-disk blob. Each tenant has
its own `package_versions` row pointing at that blob, and the
policy engine gates access per-tenant — so a quarantine in tenant
A doesn't affect tenant B's view. But the *bytes* themselves
aren't per-tenant.

For the "many teams in one company" shape this is desirable: every
fresh ingest only pays the bandwidth + storage cost once. For
"unrelated organizations" tenancy where blob existence might be a
side channel, it's worth being aware of.

### SQLite is single-writer at the database-file level

A tenant doing a very large write burst can briefly serialize
writes for everyone else on the same instance. For typical workloads
this is invisible; for SaaS-scale "many customers, sustained load"
it becomes a real ceiling. The remediation path — Postgres,
per-tenant sharding, or read replicas — is documented in
[`DECISIONS.md`](../DECISIONS.md) ("Revisit when we need multi-node
/ HA").

### Per-tenant signing keys for signed-index formats

Alpine, Debian, and RPM ship signed indices. Each tenant gets its
own signing key, and the public key has to be installed on every
client machine that consumes that tenant's repo. This is a
per-tenant onboarding step (documented in the per-format demos and
runbooks); it's not a bug, but it is recurring work proportional
to the number of tenants.

### Policy rules are tenant-scoped or global, with nothing in between

A `policy_rules` row has `tenant_id`. A rule with `tenant_id IS
NULL` applies globally; a rule with `tenant_id = N` applies only
to that tenant. There is no "this rule applies to tenants A and
B." If you want a baseline policy shared across a subset of
tenants today, you insert one row per tenant.

## See also

- [auth.md](auth.md) — tenant + user + token data model, permissions,
  visibility (public / private)
- [storage.md](storage.md) — content-addressed blob store, SQLite
  schema, per-tenant data layout
- [supply-chain.md](supply-chain.md) — policy rules and how the
  `tenant_id` scope works
- [demos/python.md](demos/python.md) — runnable example using the
  `default` tenant
