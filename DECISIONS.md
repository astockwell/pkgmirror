# Decision Log

A running log of assumptions and architectural decisions made while building `pkgmirror`.
Each entry: date, decision, rationale, and (when relevant) what I'd revisit later.

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
