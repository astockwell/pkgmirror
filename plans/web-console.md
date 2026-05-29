# Plan: Web console for pkgmirror

**Status:** draft — pending user answers to §10 open questions before
implementation begins.

**Author:** assessment captured during a planning conversation; not
yet reviewed.

**Adjacent docs:** the supply-chain policy engine
([docs/supply-chain.md](../docs/supply-chain.md)) and the existing
`/admin` REST endpoints define most of what the console will need to
expose; this plan is about the *delivery shape*, not what data the
console shows.

---

## 1. What this plan covers

A browser-driven administrative console for pkgmirror operators:

- Overview / dashboard (per-tenant package counts, recent ingest,
  policy decisions, audit volume)
- Tenant management (CRUD, visibility, member list)
- Package browser (list, search, inspect a version, see policy
  decisions, quarantine / promote)
- Rules editor (cascading rules, dry-run a rule against past
  ingests)
- Audit log query (filter, paginate, export)
- Token management (mint, revoke, see last-used)
- User settings (own profile, own tokens)

Excludes: end-user package install instructions — those stay in
[README.md](../README.md). The console is for **operators** of a
pkgmirror deployment, not consumers of the registries.

---

## 2. Recommendation

**Single binary now, with a clean internal package boundary and a
feature flag (`PKGMIRROR_CONSOLE_ENABLED`, default `true`) to
disable the console.** Defer splitting into a second binary until
one of three concrete triggers fires (see §8).

Concretely:

- Console handlers live in `internal/console/`
- HTML templates in `templates/console/`
- Static assets (CSS/JS/favicon) in `assets/static/`
- Mounted at `/console/...`; `/static/...` for assets
- `cmd/pkgmirror/main.go` reads the flag and wires the console
  group only when enabled

---

## 3. Why a single binary today

### 3.1. SQLite is the load-bearing constraint

Two binaries writing to the same SQLite file is not viable. WAL
mode allows concurrent **reads** but writers serialize on a
single file lock, and the WAL-race patterns already documented
in [docs/long-term-maintenance.md](../docs/long-term-maintenance.md)
§7 (the macOS APFS one, the sibling-test SQLITE_BUSY one) would
compound across processes.

The realistic split-binary topologies all force a choice:

1. **Both binaries on the same SQLite file** → bad, will corrupt
   under load
2. **One binary owns the DB; the other calls it over HTTP** →
   you've already "split", but now both processes still need to
   coexist, with a network hop and serialization layer added to
   every operation that's currently an in-process function call
3. **Move to Postgres** → real lift; separate decision; not on
   the table today

Until (3) is on the table, the SQLite constraint says **one
process owns writes.**

### 3.2. The scale isn't there yet

A pkgmirror instance handling a corporate dev org's traffic
peaks at tens of req/s. HTML rendering for an admin console is
single-digit RPS at most — operators are humans, and there are
~N of them, not N×1000. One Go process with gin handles both
without sweating; there's no real "the console is starving the
registry" pressure to design for.

### 3.3. Forgejo / Gitea pattern as evidence

Both are single binaries serving HTTP UI + Git protocol + REST
API + webhook delivery in-process, with feature flags to disable
subsystems. Their only split (`forgejo runner`) happened when
the workload was **fundamentally different** — long-running CI
execution that needed sandboxing from the web server.

We're already cribbing their package format implementations; the
deployment shape is more of the same.

---

## 4. What "clean boundary" means concretely

### 4.1. Package layout

```
internal/
  console/                      # ← new
    handler.go                  # gin routes; reads via shared internal/* services
    pages/
      dashboard.go              # one file per page-cluster
      tenants.go
      packages.go
      audit.go
      rules.go
      tokens.go
    middleware.go               # session auth, CSRF
    session/                    # cookie session store (SQLite-backed)
  ui/                           # ← rename/keep existing — shared widgets, bootstrap helpers
  ...
templates/
  console/                      # ← page templates
    layout.html
    dashboard.html
    tenants/
      list.html
      detail.html
    ...
assets/
  static/                       # ← favicon, CSS, JS, images served at /static/
```

The console handlers use the same `internal/models/`,
`internal/policy/`, `internal/audit/`, `internal/tenants/` packages
the registry uses. **No HTTP between layers** — just function
calls.

### 4.2. Route layout

```
/api/packages/:tenant/...        # registry (existing, public-facing)
/api/admin/...                   # admin REST API (existing, PAT-authed)
/console/...                     # server-rendered HTML (new, session-authed)
/console/login                   # session start
/console/logout
/static/...                      # assets
/                                # redirect to /console
```

Three distinct **middleware chains**, in declining order of
public-facing risk:

| Chain | Auth | CSRF | Used by |
| --- | --- | --- | --- |
| Registry | PAT (Bearer / Basic / `X-NuGet-ApiKey`) | ✗ (stateless) | `/api/packages/...` |
| Admin API | PAT (must be admin) | ✗ (stateless) | `/api/admin/...` |
| Console | Session cookie + admin user | ✓ | `/console/...` |

---

## 5. Auth is the genuinely-new piece

Today's [internal/auth/](../internal/auth/) is PAT-only (Bearer
/ Basic / `X-NuGet-ApiKey` fallthrough). Right model for CLI
clients, **wrong model for a browser console** — operators
shouldn't type `pkm_*` tokens into HTML forms.

### 5.1. What the console needs

- **Session cookies** — signed, `HttpOnly`, `Secure` (when TLS),
  `SameSite=Lax`, stored server-side (SQLite-backed table or
  in-memory map gated by config)
- **CSRF tokens** on every state-changing form (rule edit,
  quarantine, token revoke, etc.)
- **Login flow** — at minimum, username + password against tenant
  users; OIDC against an external IdP later
- **Session-from-PAT bridge** — a way for `curl`-driven console
  use to convert a PAT into a session cookie, with a different
  cookie name so the two paths don't tangle

### 5.2. Implementation shape

`internal/auth/` grows a second `Authenticator` impl:

```go
type SessionAuthenticator struct {
    Sessions *session.Store  // new
    Users    *users.Store
    Tenants  *tenants.Store
}

func (a *SessionAuthenticator) Authenticate(ctx, r) (*Identity, error) {
    cookie, err := r.Cookie("pkgmirror_session")
    if err != nil { return nil, nil }  // anonymous
    sess, err := a.Sessions.Get(ctx, cookie.Value)
    if err != nil { return nil, nil }
    if sess.ExpiresUnix < time.Now().Unix() { return nil, nil }
    u, _ := a.Users.GetByID(ctx, sess.UserID)
    mem, _ := a.Tenants.Memberships(ctx, u.ID)
    return &Identity{User: u, Memberships: mem}, nil  // Token field left nil
}
```

The shared `Identity` type and `RequireRead` / `RequireWrite`
helpers stay the same — they don't care which middleware
populated the context.

Console handlers wire `SessionAuthenticator` via a separate
middleware chain registered only inside the `/console` group;
registry routes keep using `TokenAuthenticator`. A request
arriving at the registry routes with a session cookie but no
Bearer/Basic header reads as anonymous — by design.

### 5.3. Login

The MVP login is **username + password against tenant users**.
We don't currently have password storage on the `users` table
(tokens are the only credential), so this is a real schema
addition:

```sql
ALTER TABLE users ADD COLUMN password_hash TEXT;     -- argon2id, nullable
ALTER TABLE users ADD COLUMN password_set_unix INT;  -- timestamp of last set
```

Nullable so existing token-only users aren't forced to pick a
password. The first console-login attempt for a tokenless user
gets a "set a password first" prompt.

OIDC is a **v2** item — see §11.

---

## 6. K8s topology

### 6.1. Day-1 (every deployment today)

```
Deployment: pkgmirror (1+ replicas)
  - serves /api/packages/* + /console/* + /api/admin/*
  - PKGMIRROR_CONSOLE_ENABLED=true
Service: pkgmirror :8080
Ingress: pkgmirror.example.com → Service
```

Single Service, single Ingress, console + registry on one
listener. Operators reach the console at `https://pkgmirror.example.com/console`;
package clients hit the registry routes.

### 6.2. Day-2 (when a §8 trigger fires AND Postgres migration is done)

```
Deployment: pkgmirror-registry (N replicas)
  - serves /api/packages/* + /api/admin/*
  - PKGMIRROR_CONSOLE_ENABLED=false
  - DATABASE_URL=postgres://...
Service: pkgmirror-registry :8080

Deployment: pkgmirror-console (1-2 replicas)
  - serves /console/* + /static/* only
  - PKGMIRROR_REGISTRY_ENABLED=false  (new flag to mirror)
  - DATABASE_URL=postgres://... (same database)
Service: pkgmirror-console :8080

Ingress:
  /console/* + /static/* → pkgmirror-console Service
  everything else        → pkgmirror-registry Service
```

The `PKGMIRROR_REGISTRY_ENABLED` flag is the mirror image of the
console flag and lands at the same time as the split: when set
to `false`, the registry route groups don't register. Same
binary, opposite behavior.

**This topology requires Postgres.** If you haven't migrated off
SQLite by the time you want to split, the topology is forced
back to single-binary anyway. So: don't optimize for the split
until the Postgres migration is on the table.

---

## 7. Templating choice

### 7.1. Recommendation: `html/template` + HTMX

- Server-rendered, gin's existing templating
- HTMX adds progressive interactivity (sort tables, inline edit,
  modal forms, partial updates) without a JS toolchain
- All the planned admin actions are CRUD — HTMX fits like a glove
- Single `<script src="/static/htmx.min.js">` in the layout; no
  npm, no webpack, no JS build

### 7.2. Alternatives weighed

- **`html/template` only.** Zero JS dependency, full page
  reloads for every action. Smaller toolchain footprint but
  meaningfully worse UX for the rules editor and the audit-log
  filter. Reasonable if HTMX is considered overhead.
- **SPA (React, Svelte, etc.).** Overkill. Admin consoles for
  CRUD apps don't justify the toolchain; we'd be reinventing
  what HTMX gives us in 14 KB.
- **Templ / a-h/templ (typed Go templates).** Worth considering
  for type safety, but adds a code-gen step. `html/template` is
  fine for the surface area we expect.

### 7.3. Static asset story

- `assets/static/` for everything served at `/static/...`
- Bundled via `embed.FS` into the binary so deployments are still
  single-artifact
- Cache-bust via a build-time hash in the asset URL

---

## 8. When to actually split into two binaries

Three concrete triggers, **any** of which justifies the work:

1. **The console grows a heavy operation** — async report
   generation, full audit-log scrubbing, OCI image scanning,
   anything that runs for >30s. You don't want it sharing a
   process with package downloads. Split.
2. **You need different ingress / WAF / rate-limit policies** for
   the registry vs the console. Easier to enforce at the
   deployment boundary than in middleware.
3. **You move to Postgres and have a real reason to scale them
   independently** (registry hot, console cold). Even with
   Postgres, one binary still works *unless* (1) or (2) is also
   true.

Until one of those fires, "single binary with clean boundary"
gives ~all the benefits of split (clean package, disable flag,
predictable scope) and none of the cost (no IPC, no double
config, no auth-between-services, no SQLite contention).

---

## 9. Effort estimate

The auth and session work is the biggest swing of the bat; the
templates are mechanical once auth is solid.

| # | Step | Estimate |
| --- | --- | --- |
| 1 | `internal/console/session/` — cookie store, SQLite-backed table + migration | 3 hours |
| 2 | `SessionAuthenticator` + login route + logout + password set/reset + argon2id schema | 4 hours |
| 3 | CSRF middleware | 1 hour |
| 4 | `internal/console/handler.go` + layout template + base CSS | 2 hours |
| 5 | Dashboard page (read-only) | 1 hour |
| 6 | Tenant browser (list + detail) | 2 hours |
| 7 | Package browser (list + version + file detail) | 3 hours |
| 8 | Audit query page | 2 hours |
| 9 | Rules editor (with the dry-run hook from the existing rules engine) | 4 hours |
| 10 | Token management page (mint + revoke + last-used) | 2 hours |
| 11 | Tests: grey-box for session + CSRF + at least one page; e2e via `chromedp` is a stretch | 4 hours |
| 12 | Docs: README usage, DECISIONS entry capturing the §10 answers | 1 hour |

Total: ~4 days for a usable MVP. The schema migration and
session bits are load-bearing — don't skimp on them.

---

## 10. Open questions (need user answers before §9 starts)

### Q1. Templating: `html/template + HTMX`, or `html/template` only, or `templ`?

- **Recommendation:** `html/template` + HTMX. Cheapest reasonable
  UX; no JS toolchain.

### Q2. Login mechanism: username/password only, or OIDC from day one?

- **Recommendation:** username/password for MVP (with argon2id
  storage). OIDC as a v2 item — needs a per-tenant or
  per-deployment IdP config dance that's its own discussion.

### Q3. CSRF: cookie-and-form-field pattern, or origin-header check?

- **Recommendation:** cookie-and-form-field (gorilla/csrf style
  but home-grown to avoid the dep). Origin-header check is
  cheaper but less battle-tested.

### Q4. Session store: SQLite-backed (durable across restarts), or in-memory map (lost on restart)?

- **Recommendation:** SQLite-backed by default with an opt-in
  in-memory mode for single-process dev. Durable sessions match
  user expectations and don't force re-login on every binary
  restart.

### Q5. Console + registry on the same listener, or separate ports from day 1?

- **Recommendation:** same listener with route-group separation.
  Splitting listeners means two `http.Server`s, two TLS configs,
  two metrics paths — overhead for no clear benefit at our
  current scale. Day-2 split into two binaries covers the
  "different rate-limit policies" case better than two listeners
  in one process would.

### Q6. Routes for end-user-facing pages (per-tenant package browse without admin login), or admin-only?

- **Recommendation:** admin-only for MVP. End-user-facing
  "browse the contents of a tenant" is a separate UI concern
  that can borrow templates later. Limits the scope and the
  threat model for v1.

### Q7. Embedded assets vs filesystem-served assets?

- **Recommendation:** `embed.FS` so the binary is still
  single-artifact. Override path via env var
  (`PKGMIRROR_STATIC_DIR`) for live-reload during development.

### Default to all recommendations?

If the user says "your call on all seven", proceed with:

- `html/template` + HTMX
- Username/password (argon2id), OIDC later
- Cookie-and-form-field CSRF
- SQLite-backed session store
- Same listener, route-group separation
- Admin-only console
- Embedded assets with env-var override

---

## 11. v2 / follow-up roadmap

Items intentionally deferred from the MVP:

1. **OIDC login** — per-deployment IdP config; group → role
   mapping from claims. The most-requested missing thing for any
   internal-corporate deployment.
2. **End-user / read-only tenant browse** — non-admin users
   browsing what's in a tenant they have read access to. Limits
   scope of v1.
3. **WebAuthn / passkeys** — once OIDC is in, native passkey
   support is a small additional layer.
4. **Audit log export** (CSV / JSON streaming, with date-range
   filter).
5. **Bulk operations** — quarantine all versions of a package
   matching a glob, promote all-versions, etc. (rules engine
   covers this via patterns today; UI surfacing is the gap.)
6. **Postgres migration** — pre-requisite for the §6.2 split
   topology. Tracked separately as its own plan.
7. **Splitting into two binaries** per §8 triggers.

---

## 12. Risks and unknowns

- **Password storage is a new responsibility.** argon2id with
  proper parameters is straightforward, but it adds a real
  attack surface (login brute-force, credential stuffing). Need
  to add rate limiting on `/console/login` from day 1.
- **CSRF is easy to get wrong.** Home-growing is fine but the
  test surface needs to actually probe the failure modes
  (missing token, stale token, cross-tenant token reuse).
- **Session-store-in-SQLite** competes with the same write
  serialization as the rest of the schema. For a console with
  N=tens of operators, this is fine; for thousands it'd want a
  separate store. Not on the horizon.
- **The "console disabled, registry only" mode** must continue
  to serve the existing `/api/admin/...` REST routes — those are
  the PAT-authenticated programmatic interface and they're not
  going away when we add a UI. Keep them as a parallel surface,
  not a console-internal API.
- **HTMX-driven pages need to handle being hit by a non-HTMX
  request.** Direct URL access to a fragment endpoint should
  redirect to the parent page, not render the fragment standalone.
- **embed.FS doesn't update on file save during development.**
  The env-var override (Q7) is what makes hot iteration possible;
  needs to be wired before page 4.

---

## 13. Acceptance criteria for "MVP shipped"

- `PKGMIRROR_CONSOLE_ENABLED=true` (default) wires the
  `/console` and `/static` route groups; `false` strips them
  cleanly with no leftover routes
- `/console/login` accepts username + password; argon2id
  hashing; on success sets an HttpOnly session cookie
- `/console/dashboard` renders without errors for a freshly-
  logged-in admin against a fresh `make run`-style instance
- Tenant browser, package browser, audit page, rules editor,
  and token management page all read-render-and-edit per their
  scope
- CSRF token enforced on every state-changing form; missing
  token returns 403 with a friendly page
- Session expires after the configured TTL
  (`PKGMIRROR_SESSION_TTL`, default 24h); a request with an
  expired cookie redirects to login
- Schema migration adds `password_hash` + `password_set_unix`
  columns; existing users without a password get a "set password"
  redirect on first login attempt
- Stress loop (`go test ./... -count=1` × 10) green
- README has a "Using the console" section with screenshots and
  the `PKGMIRROR_CONSOLE_ENABLED` flag documented
- DECISIONS.md entry capturing all seven §10 answers + the
  schema migration
- `docs/auth.md` updated to document the
  Session vs Token middleware chains and where each is mounted
