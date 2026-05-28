# Decision Log

A running log of assumptions and architectural decisions made while building `pkgmirror`.
Each entry: date, decision, rationale, and (when relevant) what I'd revisit later.

---

## 2026-05-27 — PyPI format (second format landed)

**Decision:** Implement PyPI as the second package format. Three endpoints:

- `POST /api/packages/:tenant/pypi/` — multipart "legacy upload" API
  (what `twine` and `pip upload` speak)
- `GET  /api/packages/:tenant/pypi/simple[/]` — PEP 503 root index
  and `GET .../simple/:name/` per-package index (HTML or PEP 691 JSON
  via Accept negotiation)
- `GET  /api/packages/:tenant/pypi/files/:name/:version/:filename` — download

Modeled on `forgejo/routers/api/packages/pypi/pypi.go` (MIT). No parser is
needed: package metadata arrives as multipart form fields (author, summary,
requires_python, etc.), not embedded in the wheel/sdist file itself. We
store per-version metadata in `package_properties` under `pypi.*` keys.

**Name canonicalization:** full PEP 503 normalization
(`re.sub(r"[-_.]+", "-", name).lower()`), stricter than Forgejo's partial
form (Forgejo only replaces individual `_` and `.` and does not lowercase).
To preserve the user-supplied display case (e.g. `Foo_Bar` shows as
`Foo_Bar` in the UI/JSON while being addressable as `foo-bar` in URLs)
we introduced `models.GetOrCreatePackageWithLookup` /
`GetPackageByLookup` and a `PackageLookupName` field on `pkgsvc.CreationInfo`.
The lookup key is separate from the display name and persists in the
existing `lower_name` column — no schema change.

**Multi-file versions:** PyPI's model is "one version → many files" (an
sdist + one or more wheel variants). Added
`pkgsvc.Service.CreatePackageOrAddFileToExisting` that, unlike
`CreatePackageAndAddFile`, tolerates an already-existing version and
attaches the new file to it. The existing
`UNIQUE(version_id, lower_name)` on `package_files` still surfaces a real
duplicate as `models.ErrDuplicatePackageFile` → 409.

**Black-box conformance:** uploads a **wheel** (not an sdist) and installs
it via `pip install --target=/work/site` in `python:3.12-slim`. Wheels are
pre-built so they install cleanly in stripped-down Python images that lack
`setuptools`. We construct a minimal PEP 427 universal wheel in-process
(`buildWheel` in `tests/blackbox/pypi/wheel_test.go`) so there's no
external fixture dependency. PEP 691 JSON conformance covered by a
separate host-side assertion.

**HTTP-vs-HTTPS:** unlike Go, pip honors in-URL Basic-auth credentials and
`.netrc` over plain HTTP, but it does require `--trusted-host` (or
`PIP_TRUSTED_HOST`) for non-HTTPS index URLs. The blackbox test sets
`PIP_TRUSTED_HOST=pkgmirror` and uses the (public, by harness convention)
default tenant, consistent with the goproxy strategy. Private-tenant +
authenticated `pip` flows work today over plain HTTP via in-URL creds; in
production we'd still terminate TLS in front of pkgmirror.

**Validation:** unit tests cover upload (auth gate, validation, sha256
mismatch, dup filename, multi-file version), HTML + JSON simple index,
download roundtrip, anonymous reads on public tenants. Black-box drives
real `pip install` + `python -c 'import foo; print(foo.greet())'` against
`python:3.12-slim`.

---

## 2026-05-27 — Multi-tenancy, users, and token auth

**Decision:** Introduce three new concepts and require an authenticated
identity for non-public reads and all writes:

- **`tenants`** — flat namespaces, primary key on `(tenant_id, type, name)`.
  Tenant name appears in URLs: `/api/packages/<tenant>/<format>/…`. Each
  tenant has a `visibility` (`private` / `public`).
- **`users`** — principals, either `human` or `service`. Decoupled from
  tenants so one user can belong to multiple tenants with different roles.
  `external_provider` + `external_subject` columns are forward-compat for
  OIDC/LDAP without forcing those today.
- **`tenant_members`** — many-to-many between users and tenants with a role
  (`reader` / `writer` / `tenant_admin`).
- **`tokens`** — opaque `pkm_<32 base32>` bearer strings, stored as
  `sha256(plaintext)` hex. Carry CSV scopes (`read`, `write`, `admin`) and
  optional `tenant_scope` + `expires_unix`.

Auth flows through the new `auth.Authenticator` interface; today's only
implementation is `TokenAuthenticator`. Future identity sources (OIDC,
LDAP, reverse-proxy header, mTLS) slot in alongside without handler
changes. See `docs/auth.md` for the full spec.

**Rationale:** the user confirmed multi-tenant isolation is a v1
requirement. Separating users from tenants (rather than collapsing both
into a Forgejo-style polymorphic `User` row) keeps the model crisp and
makes the OIDC/audit story straightforward.

**Crypto note:** SHA-256 (not bcrypt) for token hashing. Tokens have ~160
bits of entropy from `crypto/rand` — rainbow tables and brute force are
irrelevant; bcrypt would only add cost without security. This matches how
GitHub stores `ghp_…` PATs.

**Bootstrap:** on every start, `internal/bootstrap.Ensure` idempotently
creates the `admin` service user, the `default` tenant (with visibility
from `PKGMIRROR_DEFAULT_TENANT_VISIBILITY`), the admin → default
membership, and an initial admin token (from `PKGMIRROR_ADMIN_TOKEN` or
freshly minted and printed once to stderr).

**Migrations:** new `migrations` slice in `internal/db/db.go` driven by
`PRAGMA user_version`. v1 is the original schema; v2 adds tenants/users/
members/tokens and rewrites `packages` with the new `UNIQUE(tenant_id,
type, lower_name)` constraint. SQLite WAL was dropped from the DSN because
modernc.org/sqlite leaves `-wal`/`-shm` files past `Close()`, breaking
test cleanup; default rollback journaling is fine for our single-writer
workload.

**Known limitation:** in black-box tests we default the tenant to
`public` because the Go toolchain refuses to send Basic-auth credentials
over plain HTTP (a hardcoded rule, not configurable via GOAUTH/netrc/URL).
Production deploys must terminate TLS in front of pkgmirror for `go` (and
several other clients) to send credentials. Auth-gate behavior is
exhaustively covered by the in-process unit tests, which have no transport
restriction. See `docs/auth.md` "Per-format credential supply" for the
detail.

**Validation:** unit + black-box conformance suites pass. The blackbox
suite covers: anonymous read on public tenant succeeds; upload without
token returns 401; `go mod download`/`go build`/run flow works against
the real `golang:1.22-bookworm` client.

**Revisit when:** we add OIDC, web sessions, a token CRUD API, or per-
package visibility (currently only per-tenant). Also when we decide on
TLS termination — built-in vs always-via-proxy.

---

## 2026-05-27 — Black-box conformance via dockerized native clients

**Decision:** Conformance for each package format is validated by running the
real ecosystem client (`go`, `npm`, `pip`, `mvn`, …) in an official docker
container against a `pkgmirror` container, both on a per-test private docker
network. Orchestration uses
[`testcontainers-go`](https://golang.testcontainers.org/). All black-box
files are gated behind `//go:build blackbox`, so the default
`go test ./...` stays sub-second and CI-friendly even without docker.

**Rationale:** the only reliable way to prove wire-format compatibility
with an ecosystem is to drive its actual client. Containerizing both sides
eliminates "works on my mac because brew installed npm 10 but CI has 18"
drift and makes the test identical locally and on any CI runner with a
docker daemon. testcontainers' Ryuk reaper guarantees cleanup even on
crashed test processes.

**Implementation notes:**

- Dockerfile is intentionally BuildKit-free (no `--mount=type=cache`) so the
  legacy docker daemon builder used by testcontainers-go v0.42 can build it.
  Layer caching across runs is still good enough.
- `Client.Exec` wraps each command in `sh -c '… 2>&1'` and uses
  `tcexec.Multiplexed()` so callers receive a clean text stream instead of
  docker's framed stdout/stderr multiplex.
- See [`docs/blackbox-testing.md`](docs/blackbox-testing.md) for the full
  contract and "how to add a new format" guide.

**Validation:** `make test-blackbox` builds the pkgmirror image (~1 min cold,
seconds warm), brings up a `golang:1.22-bookworm` client container, drives
`go mod download` / `go list -m -versions` / `go build` against the mirror,
and runs the resulting binary. Two tests cover single-version and
multi-version flows.

**Revisit when:** we want a multi-version client matrix (e.g. test against
go 1.21 *and* 1.22), or want to test directly through the official
`GOPROXY` redirection behavior.

---

## 2026-05-27 — Build a fresh service, do not vendor Forgejo

**Decision:** Treat the spec's "Fork the Forgejo project and extract the package
registry code" as a *learning* exercise rather than a literal extraction. Build a
new, small Go service in `pkgmirror/` that reimplements only what is needed,
using Forgejo's code as a reference design.

**Rationale:** Forgejo's package registry is deeply coupled to:

- the Forgejo ORM (`forgejo.org/models/db`) on top of XORM
- the org/user "owner" concept (every package belongs to a `User` row)
- the Forgejo `context.Context` request abstraction, auth, sessions, CSRF, ACL
- the Forgejo storage abstraction (local/minio/azure)
- a giant web of `services/` packages

Verbatim extraction would drag in tens of thousands of lines and leave us with
a Forgejo-shaped service we couldn't actually run. The spec explicitly allows
"a whole new web server/framework if it makes sense" and prefers Gin + `html/template`.
So: borrow the *protocol* code (e.g. zip parsing for Go modules) and the *schema*
shape, write fresh handlers.

**Revisit when:** scaling to many formats — we may want to vendor specific
parser modules from `modules/packages/<fmt>/` rather than rewrite each one.

---

## 2026-05-27 — Web framework: Gin

**Decision:** Use `github.com/gin-gonic/gin` for HTTP routing.

**Rationale:** Spec calls it out by name. Mature, popular, simple middleware
model, good for both JSON APIs and `html/template` rendering.

---

## 2026-05-27 — Metadata DB: SQLite via `modernc.org/sqlite`

**Decision:** Single embedded SQLite database for all metadata. Use the
pure-Go driver `modernc.org/sqlite` (no CGO required).

**Rationale:** Simplest possible deployment story for an early-stage service.
A package mirror is overwhelmingly read-heavy; SQLite handles this fine for a
single-node deployment. Pure-Go driver keeps `go build` trivial on any platform.

**Revisit when:** we need multi-node / HA, or when write contention from
concurrent uploads becomes a real problem. Schema is plain SQL so a
Postgres swap is straightforward later.

---

## 2026-05-27 — Blob storage: filesystem, content-addressed by SHA-256

**Decision:** Store package file bytes on the local filesystem at paths
derived from the SHA-256 of the file content (`<root>/aa/bb/aabbcc…`).
Metadata DB references blobs by their hash. Identical blobs are stored once.

**Rationale:** Mirrors Forgejo's `package_blobs` approach. Trivial to
implement, dedupes naturally, easy to swap behind an interface (`storage.Backend`)
for S3/MinIO/Azure later.

---

## 2026-05-27 — No multi-tenant "owners" yet

**Decision:** Drop Forgejo's per-user/per-org package ownership. A package
is uniquely identified by `(type, name)` globally in this service.

**Rationale:** The mirror has no users — it's a single shared cache/mirror.
Adding owners is straightforward (add `owner_id` column + scope routes) when
we need ACLs.

**Revisit when:** authentication is added or different teams need isolated
namespaces.

---

## 2026-05-27 — No authentication on v1

**Decision:** All endpoints are open. Uploads via `PUT /api/packages/go/upload`
are unauthenticated.

**Rationale:** Spec explicitly flags "questions about how to handle authentication"
as something to be decided as we go. Keeping it open lets us validate the
end-to-end flow without infrastructure dependencies.

**Revisit when:** before any deploy beyond localhost. Likely add either a
shared bearer token (`PKGMIRROR_UPLOAD_TOKEN`) or OIDC.

**Open question for the user:** which auth model do you want — shared
secret, OIDC, mTLS, or integrated with an existing IdP?

---

## 2026-05-27 — UI: server-rendered `html/template` + Bootstrap 5 from CDN

**Decision:** Minimal server-rendered pages (index, per-package, per-version),
Bootstrap loaded from jsDelivr CDN. No build step, no JS framework.

**Rationale:** Spec says "We do NOT want to replicate the Forgejo UI" and
"we can simply start with Bootstrap" + "simplicity (such as go http templates)".

---

## 2026-05-27 — Go module proxy is the first format

**Decision:** Implement `go` first, matching the protocol described at
<https://go.dev/ref/mod#goproxy-protocol>.

**Rationale:** Spec lists it as the starting example. The protocol is small
and well-specified (5 endpoints), and the zip format is documented.
Forgejo's `modules/packages/goproxy/metadata.go` parser is ~90 lines we can
reimplement cleanly.

**Endpoints implemented:**

- `GET /api/packages/go/<module>/@v/list`
- `GET /api/packages/go/<module>/@v/<version>.info`
- `GET /api/packages/go/<module>/@v/<version>.mod`
- `GET /api/packages/go/<module>/@v/<version>.zip`
- `GET /api/packages/go/<module>/@latest`
- `PUT /api/packages/go/upload` (mirror-population endpoint — not part of
  the standard `GOPROXY` protocol, modeled on Forgejo's upload route)

**Validation:** Verified end-to-end against the real `go` toolchain:

```sh
GOPROXY=http://127.0.0.1:18080/api/packages/go GOSUMDB=off \
  go mod download -x example.com/foo
# 3× 200 OK on .info, .mod, .zip
```

Integration tests in `internal/packages/goproxy/handler_test.go` cover upload,
list, info, mod, zip, @latest, duplicate-upload (409), missing version (404),
and multi-version ordering.

---

## 2026-05-27 — Module path parsing

**Decision:** A Go module path can contain `/`. The proxy URL embeds it
literally (e.g. `example.com/foo/bar/@v/v1.0.0.info`). I route on
`/api/packages/go/*path` and parse `<module>/@v/<file>` from `path` in code,
rather than try to express it as a Gin route pattern.

**Rationale:** Gin's tree router can't express "any number of segments,
then a literal `@v` segment". A single catch-all + manual split is
straightforward and matches how the spec / Go toolchain emit these URLs.
