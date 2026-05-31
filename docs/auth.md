# Auth & multi-tenancy

This document describes how `pkgmirror` models tenants, users, and tokens,
and the per-format mechanics for how each ecosystem client supplies
credentials. It is the spec for the current implementation and the design
contract that future identity sources (OIDC, LDAP, mTLS, …) must conform to.

For the URL routing model that makes multi-tenancy work end-to-end with
real package toolchains (and the OCI nuance), see
[multi-tenant.md](multi-tenant.md).

## Concepts

### Tenant

A **tenant** is a namespace for packages. Every package belongs to exactly
one tenant. Tenant names appear in URLs:

```
/api/packages/<tenant>/<format>/…
/t/<tenant>/p/<type>/<name>            (UI)
```

Tenants have a `visibility`:

| Visibility | Anonymous read | Auth required to write |
| --- | --- | --- |
| `private` (default) | No (401) | Yes |
| `public` | Yes | Yes |

Writes always require an authenticated identity with `write` permission on
the tenant.

### User

A **user** is a principal — either a `human` or a `service` account. Users
own tokens, and become members of tenants via `tenant_members`. Users are
deliberately decoupled from tenants so a single user can have different
roles in multiple tenants.

`users.is_admin = 1` grants system-wide admin authority (used today only by
the bootstrap admin).

The `external_provider` + `external_subject` columns are forward-compat
for OIDC/LDAP: when those land, the corresponding `Authenticator` looks up
or creates a user by `(provider, subject)`.

### Tenant member

A row in `tenant_members(tenant_id, user_id, role)` grants a user a role in
a tenant. Roles:

| Role | Read | Write | Manage tenant |
| --- | --- | --- | --- |
| `reader` | ✓ | | |
| `writer` | ✓ | ✓ | |
| `tenant_admin` | ✓ | ✓ | (reserved for future tenant-mgmt APIs) |

### Token

A **token** is an opaque, high-entropy bearer string of the form

```
pkm_<32 base32 chars>          (~160 bits of entropy)
```

The plaintext is shown to the operator exactly once at creation. The
database stores only `sha256(plaintext)` for O(1) lookup. SHA-256 (not
bcrypt) is the correct primitive here because tokens have ample entropy —
this is the same approach GitHub uses for `ghp_…` PATs.

Each token has:

- `user_id` — owner
- `scopes` — CSV of `read`, `write`, `admin`
- `tenant_scope` (optional) — when set, the token only acts on that
  one tenant regardless of the user's other memberships
- `expires_unix` (optional) — 0 means no expiry

## Authorization model

For a request to be authorized for a tenant `t` with action `a` ∈ {read, write}:

1. Resolve identity from `Authorization` header (see *Transport* below).
   No header → anonymous.
2. If `a = read` and `t.visibility = public`: allow.
3. Otherwise require a non-nil identity.
4. The token must include scope `a`.
5. The token's `tenant_scope`, if set, must equal `t.id`.
6. The user must be a member of `t` with role ≥ required:
   - read → `reader`+
   - write → `writer`+
7. System admin shortcut: `user.is_admin && token.scopes ∋ admin` bypasses
   2–6.

This is implemented in [`internal/auth/auth.go`](../internal/auth/auth.go)
(`Identity.CanRead`, `Identity.CanWrite`) and enforced by
`auth.RequireRead` / `auth.RequireWrite` at each handler entry point.

## Transport — how the token reaches us

Every authenticated request supplies one of:

| Mechanism | Header | Typical clients |
| --- | --- | --- |
| Bearer | `Authorization: Bearer <token>` | container registry, npm `_authToken`, cargo, direct API |
| HTTP Basic | `Authorization: Basic base64(<any>:<token>)` | `go` (HTTPS), `pip`, `mvn`, `gem`, `composer`, `nuget`, `dart pub`, `swift`, `helm` |

The username in Basic is ignored on the server — `<any>` works. Some clients
require a literal username; using your user's name is conventional.

### `WWW-Authenticate` challenge

On `401 Unauthorized` we respond with `WWW-Authenticate: Basic realm="pkgmirror"`
to prompt CLIs (curl, gem, etc.) to retry with credentials.

## First-boot bootstrap

On every start, `internal/bootstrap.Ensure` runs idempotently:

1. Ensures a `tenants.default` row exists. If `PKGMIRROR_DEFAULT_TENANT_VISIBILITY`
   = `public`, the existing row is updated to public; the default is `private`.
2. Ensures the `admin` service-account user exists with `is_admin=1`.
3. Ensures `admin` is a tenant_admin of `default`.
4. If `admin` has zero tokens:
   - If `PKGMIRROR_ADMIN_TOKEN` is set, that plaintext is installed.
   - Otherwise a fresh `pkm_…` token is minted, its hash stored, and the
     plaintext printed to stderr **once** (the operator must save it).

The admin token has scopes `read,write,admin` and no tenant_scope, so it
can act against any tenant.

## Per-format credential supply

How does each ecosystem's CLI pass our token?

### Go (`go`)

The Go toolchain has a strong, **non-overridable** rule: it refuses to send
Basic-auth credentials over plain HTTP — regardless of `GOAUTH`, `.netrc`,
or URL embedding. Credentials only flow over HTTPS.

Implications:

- For **production**: terminate TLS in front of pkgmirror (reverse proxy,
  ingress, or a future built-in TLS listener). Then `GOAUTH=netrc` with
  `~/.netrc` containing `machine pkgmirror login x password <token>` works.
- For **public tenants**: no client-side credentials needed — anonymous
  reads succeed over HTTP. Uploads still require a token (via direct API).
- For our **black-box conformance test**: we configure the default tenant
  as public so the test can drive the real `go` toolchain over plain HTTP.
  Auth-gate behavior on private tenants is exercised by the in-process
  tests in `internal/packages/goproxy`, which have no transport restriction.

### npm

```sh
npm config set //pkgmirror.example.com/api/packages/<tenant>/npm/:_authToken=<token>
```

### Container registry (OCI)

`docker login` performs a token-exchange dance: it hits `/v2/`, receives a
`WWW-Authenticate: Bearer realm=…` challenge, exchanges its Basic creds at
the named realm endpoint for a short-lived bearer scoped to the requested
repo. We'll implement this when the container format lands.

### Maven, PyPI, RubyGems, NuGet, Composer, Cargo, Dart, Swift, Helm

All speak HTTP Basic. Each ecosystem has a CLI command to register the
credential at the package-server URL. Pattern is uniform: token goes in the
password slot of HTTP Basic, with any username.

## Future identity sources

The `auth.Authenticator` interface

```go
type Authenticator interface {
    Authenticate(ctx context.Context, r *http.Request) (*Identity, error)
}
```

is the only seam handlers know about. Today we ship `TokenAuthenticator`.
Future authenticators will:

| Source | What it does |
| --- | --- |
| `OIDCAuthenticator` | Verifies a JWT, looks up `(provider, subject)` in `users`, creates row on first sight, syncs `tenant_members` from group claims if configured. |
| `ReverseProxyHeaderAuthenticator` | Trusts an upstream proxy that already authenticated the user (e.g. `X-Forwarded-User`). Useful when fronting pkgmirror with oauth2-proxy / Pomerium. |
| `MTLSAuthenticator` | Maps a client cert's subject DN to a user. |

When multiple authenticators are configured, a chain authenticator tries
each until one returns a non-nil `Identity`. No handler changes needed.

## Threat-model notes

- **Cross-tenant blob dedup.** Blobs are content-addressed by SHA-256 and
  shared across tenants for storage efficiency. An attacker with write
  access in tenant A who can guess a blob's bytes could detect its
  presence in tenant B via response timing (currently). This is acceptable
  for a v1 internal mirror; if it isn't, partition blob paths by tenant.
- **Token rotation.** No HTTP API yet — operators rotate by deleting the
  row from the `tokens` table and minting a new one via SQL or by
  restarting with a new `PKGMIRROR_ADMIN_TOKEN`. A CRUD API is on the
  roadmap.
- **Token leakage in logs.** Tokens never appear in pkgmirror logs;
  `Authorization` headers are dropped by `gin.Logger`. Operators bridging
  request logs upstream should ensure the same.
- **Brute-forcing tokens.** With 160 bits of entropy and SHA-256 lookup,
  online brute force is computationally infeasible. No rate-limiting on
  failed auth is implemented yet; it's worth adding before exposing the
  service to the public internet.

## See also

- [DECISIONS.md](../DECISIONS.md) — running architecture decisions log.
- [blackbox-testing.md](blackbox-testing.md) — how each format's
  conformance suite drives its real client.
- [supply-chain.md](supply-chain.md) — supply-chain controls
  (cooldown, license allowlist, audit, quarantine) layered on top
  of this auth model. Admin endpoints there require `is_admin = 1`
  on the user *and* the `admin` scope on the token.
