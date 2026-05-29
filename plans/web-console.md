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

## 4. Structure and conventions

Informed by reviewing `copilot-api/cmd/admin` (excellent
page-package convention) and `go_gin_starter` (good DSO +
central-routes idea; dropped its flat `ctr_*.go` pattern and
2-level template-dir limit). What follows is the directory
layout, route shape, template loading, styling, and the
conventions that codify how the console package grows.

### 4.1. Package layout

```
internal/console/
  console.go              # Console struct (the DSO) + New() + Register(r)
  middleware/
    session.go            # cookie session via gin-contrib/sessions
    csrf.go               # cookie-and-form-field CSRF
    requireauth.go        # gates that read identity
    requiretenant.go      # data-plane scope + role check
    requireadmin.go       # control-plane system-admin check
  pages/                  # one Go package per page-area
    home/         { home.go ; home.tmpl ; home_test.go }
    auth/         { login.go ; login.tmpl ; logout.go }
    tenants/      { list.go list.tmpl ; detail.go detail.tmpl ; members.go members.tmpl }
    packages/     { list.go list.tmpl ; version.go version.tmpl ; delete.go }
    rules/        { list.go list.tmpl ; edit.go edit.tmpl ; dryrun.go }
    audit/        { query.go query.tmpl ; detail.go detail.tmpl }
    tokens/       { list.go ; mint.go ; revoke.go }
    profile/      { view.go ; password.go }
  layouts/
    base.tmpl             # html shell: head, nav, content slot, footer
    auth.tmpl             # login/logout chrome (no nav)
  partials/
    flash.tmpl
    pagination.tmpl
    breadcrumb.tmpl
    tenantswitcher.tmpl
    ui/                   # in-house component primitives (button, card, table, form-field, alert)
      button.tmpl
      card.tmpl
      table.tmpl
      formfield.tmpl
      alert.tmpl
  static/
    css/
      input.css           # Tailwind source (imports + @theme tokens lifted from copilot-api)
      console.css         # BUILD OUTPUT, committed, embedded
    js/
      console.js          # vanilla JS sprinkles
    favicon.ico
  tailwind.config.js      # points at pages/, partials/, layouts/ for class extraction
  embed.go                # //go:embed pages layouts partials static
```

The console handlers use the same `internal/models/`,
`internal/policy/`, `internal/audit/`, `internal/tenants/`
packages the registry uses. **No HTTP between layers** — just
function calls.

Key property: **one Go package per page-area, files flat
within.** This caps the "flatness explosion" at a single page's
worth of files (typically 3–8) rather than letting `ctr_*.go`
fill the package root indefinitely.

### 4.2. The `Console` struct is the DSO

Direct port of go_gin_starter's `DataSourceOrchestration`, with
typed-field access instead of `c.MustGet("dso").(*DSO)`. Each
handler closes over `*Console`:

```go
type Console struct {
    Service    *pkgsvc.Service
    Models     *models.Store
    Tenants    *tenants.Store
    Users      *users.Store
    Tokens     *tokens.Store
    Sessions   *session.Store
    Audit      audit.Logger
    Engine     policy.Engine
    Rules      *policy.RuleStore
    Templates  *template.Template
    Middleware *middleware.Middleware
    Logger     *slog.Logger
    Config     *config.ConsoleConfig
}
```

Wins over `MustGet`: compile-time check that deps exist;
refactor-safe in any IDE; the struct is the singular "what does
a console handler need?" answer (one-line change to add a new
dep).

### 4.3. Routes: hybrid (central groups, leaves near handlers)

Two-tier discoverability — keeps go_gin_starter's
routes-in-one-place clarity for the *tree shape*, but moves leaf
declarations next to the handlers they bind.

**Level 1 — central wiring (`internal/console/console.go`):**
the entire route tree, including middleware chains and which
page package owns each subtree.

```go
func (c *Console) Register(r *gin.Engine) {
    g := r.Group("/console", c.middleware.RequireAuth())
    home.Register(c, g)
    profile.Register(c, g.Group("/profile"))
    tokens.Register(c, g.Group("/tokens"))

    admin := g.Group("/admin", c.middleware.RequireSystemAdmin())
    tenants.Register(c, admin.Group("/tenants"))
    rules.RegisterSystem(c, admin.Group("/rules"))
    audit.RegisterSystem(c, admin.Group("/audit"))

    t := g.Group("/t/:tenant", c.middleware.RequireTenantMember())
    packages.Register(c, t.Group("/packages"))
    rules.RegisterTenant(c, t.Group("/rules"))
    audit.RegisterTenant(c, t.Group("/audit"))
}
```

Reading this answers every "what's at `/console/foo`?"
question at the group + middleware level.

**Level 2 — leaf routes near their handlers (`pages/<area>/<area>.go`):**

```go
// internal/console/pages/tenants/tenants.go
func Register(c *console.Console, g *gin.RouterGroup) {
    g.GET("",           list(c))
    g.GET("/:id",       detail(c))
    g.GET("/:id/edit",  editForm(c))
    g.POST("/:id",      update(c))
    g.POST("",          create(c))
}
```

5–10 routes per page area, sitting next to the handlers they
bind. Renaming, splitting, or adding a route happens in one
file alongside the related logic; the central wiring file
stays stable as page-areas grow.

### 4.4. Per-handler typed `Data` structs

Define a small `Data` struct next to each handler. No
anonymous structs (fixes go_gin_starter's rename-refactor
pain, where a renamed handler field silently breaks the
template):

```go
// internal/console/pages/tenants/list.go
type listData struct {
    Tenants []*tenants.Tenant
    Filter  string
    Total   int
}

func list(c *console.Console) gin.HandlerFunc {
    return func(gc *gin.Context) {
        ts, _ := c.Tenants.List(gc.Request.Context())
        c.Render(gc, "pages/tenants/list", listData{Tenants: ts})
    }
}
```

Less boilerplate than templ; renames follow through to
templates via the IDE's "find symbol references" rather than
silently breaking at runtime.

### 4.5. Template loading: recursive `embed.FS` walk

Sidesteps go_gin_starter's 2-level limit (which was an artifact
of its `ParseGlob` wiring, not a constraint of `html/template`).
One-time wiring in `Console.New`:

```go
//go:embed pages layouts partials
var embeddedTemplates embed.FS

func loadTemplates(funcs template.FuncMap, overrideDir string) (*template.Template, error) {
    var fsys fs.FS = embeddedTemplates
    if overrideDir != "" {
        fsys = os.DirFS(overrideDir)  // hot-reload during dev
    }
    root := template.New("").Funcs(funcs)
    return root, fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, _ error) error {
        if d.IsDir() || !strings.HasSuffix(p, ".tmpl") {
            return nil
        }
        b, err := fs.ReadFile(fsys, p)
        if err != nil { return err }
        name := strings.TrimSuffix(p, ".tmpl")  // e.g. "pages/tenants/list"
        _, err = root.New(name).Parse(string(b))
        return err
    })
}
```

Templates are named by path: the layout is `layouts/base`,
partials are `partials/flash` and `partials/ui/button`, page
templates are `pages/tenants/list`. Handlers call
`c.Render(gc, "pages/tenants/list", data)`, which wraps the
named page in the base layout.

### 4.6. Control plane vs data plane in URLs + middleware

The architectural answer to "siloed, multi-tenant, control/data
plane":

| Plane | URL prefix | Middleware | What's there |
| --- | --- | --- | --- |
| **Cross-cutting** | `/console/` | `RequireAuth` | Home, profile, own tokens, login, logout |
| **Control plane** | `/console/admin/...` | `RequireAuth + RequireSystemAdmin` | Tenant CRUD, user CRUD, system rules, system audit, global settings |
| **Data plane** | `/console/t/:tenant/...` | `RequireAuth + RequireTenantMember(:tenant)` (read) or `RequireTenantAdmin(:tenant)` (write) | Per-tenant packages, members, rules, audit, tokens |

Mirrors the existing API surface for visual + cognitive parity:

- `/api/admin/...` → `/console/admin/...`
- `/api/packages/:tenant/...` → `/console/t/:tenant/packages/...`

System admins see both planes; tenant-only users see only their
tenants under `/console/t/...`. The home page lists tenants the
user has access to (the siloed view). The tenant switcher in the
layout is a partial that renders `Identity.Memberships`.

### 4.7. Three distinct middleware chains

In declining order of public-facing risk:

| Chain | Auth | CSRF | Used by |
| --- | --- | --- | --- |
| Registry | PAT (Bearer / Basic / `X-NuGet-ApiKey`) | ✗ (stateless) | `/api/packages/...` |
| Admin API | PAT (must be admin) | ✗ (stateless) | `/api/admin/...` |
| Console | Session cookie + role gate | ✓ | `/console/...` |

A request arriving at the registry routes with a session cookie
but no Bearer/Basic header reads as anonymous — by design. The
chains don't blur.

### 4.8. UI partials: extract on repeat-three, not upfront

Following copilot-api's `components/ui/<widget>/` directory
shape but not their full inventory:

- Start with ~5 partials in `partials/ui/`: button, card,
  table, form-field, alert
- Add a partial only when a usage pattern repeats 3+ times
- Resist building the full templui-equivalent inventory (sheet,
  popover, tooltip, collapsible, …) until those patterns
  actually appear

Each partial is a small `html/template`:

```html
{{/* partials/ui/button.tmpl */}}
{{ define "ui/button" }}
<button type="{{ .Type | default "button" }}"
        class="px-4 py-2 rounded-md {{ if eq .Variant "primary" }}bg-primary text-primary-foreground{{ else }}bg-secondary{{ end }} hover:opacity-90"
        {{ with .Disabled }}disabled{{ end }}>
  {{ .Label }}
</button>
{{ end }}
```

Used from page templates via
`{{ template "ui/button" (dict "variant" "primary" "label" "Save" "type" "submit") }}`.

### 4.9. Zero JS by default; per-page slotted scripts

Layout has a `{{ block "page-js" . }}{{ end }}` near `</body>`.
Page templates that need interactivity define it inline:

```html
{{ define "page-js" }}
<script>
  // filter-as-you-type for the audit log
  const filter = document.getElementById('audit-filter');
  // ...
</script>
{{ end }}
```

When a page needs something nontrivial (e.g. the rules dry-run
preview), ship a small `.js` file under `static/js/` and
`<script src="...">` it from the page template. No global
bundle, no JS dependency tree.

### 4.10. Tests: per-package, against a real `Console` + `httptest.NewServer`

Same shape as the existing format grey-box tests. One fixture
builder in `internal/console/testutil/` for the common
"fresh Console + fresh DB + logged-in admin session" setup.
Each page package's `_test.go` covers: 200 happy path, 401
anon, 403 wrong tenant / wrong role, 404 missing, CSRF reject
on POST without token.

### 4.11. Embed everything; env-var override for dev

```go
//go:embed pages layouts partials static
var embedded embed.FS
```

Single binary in prod, no on-disk template tree needed. Dev
override:

- `PKGMIRROR_CONSOLE_DEV_DIR=internal/console` reads templates
  and static assets from disk so edits show on refresh without
  rebuild

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

## 7. Templating + styling

### 7.1. Templating: `html/template` (no templ, no HTMX)

- Server-rendered via the existing gin `html/template`
  integration
- **No templ.** Would force a codegen step; we author templates
  as files for IDE-native edit + reload
- **No HTMX for now.** Full page reloads per action. Revisit in
  §11 if any specific form gets tedious

Templates are loaded recursively from `embed.FS` (see §4.5);
named by relative path (`pages/tenants/list`, `layouts/base`,
`partials/flash`, `partials/ui/button`).

### 7.2. Styling: Tailwind via standalone CLI + copilot-api's theme tokens

To reach copilot-api's look and feel **without templui** (which
would require templ):

- **Tailwind via the standalone CLI** — single Go-friendly
  binary (~25 MB), no Node toolchain. Watch mode in dev,
  one-shot for prod builds.
- **CSS theme tokens lifted from copilot-api** (the `--color-*`
  custom properties, the `--radius-*` scale, the dark-mode
  setup). These are a port of [shadcn/ui](https://ui.shadcn.com)'s
  tokens; MIT-compatible.
- **In-house partials** for repeated patterns (see §4.8),
  styled with Tailwind classes inline.

The output CSS lives at
`internal/console/static/css/console.css`, embedded via
`embed.FS`, and **committed to the repo** so:

- Prod builds don't need Tailwind installed (just `go build`)
- The unit-test CI job doesn't need Tailwind (only the dedicated
  `make build-css` step does)
- Reviewers see CSS diffs in PRs

**Trade-off accepted:** the CSS file in git churns when
templates change. Bounded surface area = bounded churn;
acceptable for an admin console.

### 7.3. Build pipeline

Makefile additions (~4 lines):

```makefile
TAILWIND ?= $(BIN_DIR)/tailwindcss

build-css: ## Compile Tailwind into internal/console/static/css/console.css
	$(TAILWIND) -i internal/console/static/css/input.css \
	            -o internal/console/static/css/console.css \
	            --minify

watch-css: ## Like build-css but watches for template changes
	$(TAILWIND) -i internal/console/static/css/input.css \
	            -o internal/console/static/css/console.css \
	            --watch
```

`tools/tailwindcss/install.sh` fetches the standalone binary
one-time per contributor; vendored to `$(BIN_DIR)/tailwindcss`
so it lives alongside our existing `dedup-package` tool.

### 7.4. The visual match — what we get vs what we skip

Lifting copilot-api's CSS theme variables + Tailwind defaults
buys us:

- Same color palette + dark mode
- Same spacing rhythm (Tailwind's default scale)
- Same typography (`font-sans` system stack)
- Same border-radius vocabulary
- Same sidebar+content app-shell shape (we rebuild their
  `AppSidebar` as a layout partial, not as a templ component)

What we **don't** get for free (defer to v2):

- Their search command palette (`SearchScript` + `search.js`)
- Their lazy-loaded HTMX sections (full pages instead)
- The full inventory of interactive components (sheet,
  popover, tooltip, collapsible) — we use native HTML
  `<details>` + minimal JS for what's needed, add a partial
  when a pattern repeats

The trade is **~80% of the visual polish for ~5% of the
toolchain commitment.**

### 7.5. Alternatives weighed

- **html/template + HTMX.** Was the prior recommendation;
  flipped because user prefers no JS framework dependency for
  v1. Tracked as a possible v2 item in §11 if a form gets
  tedious.
- **html/template only, no Tailwind.** Hand-rolled CSS would
  work but matching copilot-api's look would mean
  reimplementing their token system manually. Tailwind costs
  the same effort and lets us copy the tokens verbatim.
- **SPA (React, Svelte).** Still overkill. CRUD admin UIs
  don't justify the toolchain.
- **templ + templui (copilot-api's stack).** Would give us the
  look most quickly but requires codegen; user said no.

### 7.6. Static asset story

- `internal/console/static/` for everything served at `/static/...`
- Bundled via `embed.FS` so deployments are single-artifact
- Cache-bust via a build-time content hash baked into the URL
  (e.g. `/static/css/console.css?v=<sha256>`)

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

Two of the original seven questions have since been answered by
the user and folded into §4/§7:

- **Templating decided:** `html/template`, no templ, no HTMX (§7.1)
- **Styling decided:** Tailwind via standalone CLI + copilot-api
  theme tokens (§7.2)

Five remain.

### Q1. Login mechanism: username/password only, or OIDC from day one?

- **Recommendation:** username/password for MVP (with argon2id
  storage). OIDC as a v2 item — needs a per-tenant or
  per-deployment IdP config dance that's its own discussion.

### Q2. CSRF: cookie-and-form-field pattern, or origin-header check?

- **Recommendation:** cookie-and-form-field (gorilla/csrf style
  but home-grown to avoid the dep). Origin-header check is
  cheaper but less battle-tested.

### Q3. Session store: SQLite-backed (durable across restarts), or in-memory map (lost on restart)?

- **Recommendation:** SQLite-backed by default with an opt-in
  in-memory mode for single-process dev. Durable sessions match
  user expectations and don't force re-login on every binary
  restart.

### Q4. Console + registry on the same listener, or separate ports from day 1?

- **Recommendation:** same listener with route-group separation.
  Splitting listeners means two `http.Server`s, two TLS configs,
  two metrics paths — overhead for no clear benefit at our
  current scale. Day-2 split into two binaries covers the
  "different rate-limit policies" case better than two listeners
  in one process would.

### Q5. Routes for end-user-facing pages (per-tenant package browse without admin login), or admin-only?

- **Recommendation:** admin-only for MVP. End-user-facing
  "browse the contents of a tenant" is a separate UI concern
  that can borrow templates later. Limits the scope and the
  threat model for v1.

### Q6. Embedded assets vs filesystem-served assets?

- **Recommendation:** `embed.FS` so the binary is still
  single-artifact. Override path via env var
  (`PKGMIRROR_CONSOLE_DEV_DIR`) for live-reload during
  development. See §4.11.

### Default to all recommendations?

If the user says "your call on all six", proceed with:

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
8. **HTMX (re-introduce, scoped)** — if any specific form is
   meaningfully worse with full-page reloads (rules editor with
   live dry-run preview is the most likely candidate), add HTMX
   for that page only. Don't blanket-introduce.
9. **Full templui-equivalent component inventory** — sheet,
   popover, tooltip, collapsible, etc. Grow `partials/ui/` to
   match copilot-api's depth only if usage justifies it.
10. **Search command palette** — copilot-api's `SearchScript` +
    `search.js`. Cmd-K "go to anything" navigation. Nice-to-have
    once we have >20 page types.

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
- **Tailwind CSS file churns in git.** Every template change
  that adds or removes a class produces a CSS diff. Bounded by
  the admin console's modest surface area but worth naming.
  Mitigation: a CI job (or commit hook) that rebuilds CSS and
  fails the PR if the committed file is stale.
- **Visual match to copilot-api is approximate, not pixel-for-
  pixel.** Their app uses templ-driven components (which we
  can't reuse) and lazy-loaded HTMX sections (which we chose
  not to). The CSS tokens transfer cleanly; component-level
  behavior may diverge in small ways. Don't promise users
  byte-identical visuals.
- **embed.FS doesn't update on file save during development.**
  The env-var override (§4.11, Q6) is what makes hot iteration
  possible; needs to be wired before the first page lands.

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
- `make build-css` produces `internal/console/static/css/console.css`
  byte-identically on a clean checkout; the committed file
  matches
- `PKGMIRROR_CONSOLE_DEV_DIR=internal/console` overrides the
  embed and serves edits-without-rebuild for both `.tmpl` and
  static assets
- Visual match to copilot-api: same color palette, sidebar+content
  app shell, dark mode toggle. Pixel-fidelity not required
- Stress loop (`go test ./... -count=1` × 10) green
- README has a "Using the console" section with screenshots,
  the `PKGMIRROR_CONSOLE_ENABLED` flag, and the
  `make build-css` / `make watch-css` workflow documented
- DECISIONS.md entry capturing all six §10 answers + the
  schema migration + the Tailwind + the page-package convention
- [docs/auth.md](../docs/auth.md) updated to document the
  Session vs Token middleware chains and where each is mounted
