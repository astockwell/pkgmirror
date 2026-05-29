# Web console — implementation plan

**Status:** ready — companion to
[plans/web-console.md](web-console.md) (the architecture plan).
Read that first.

**Scope:** this document is the implementer's runbook. It walks
file by file through the console package, codifies the
conventions for logging, error handling, flash messaging, form
handling, and templates, and proposes a phased PR sequence so
the work lands in shippable slices rather than one giant change.

Where the architecture plan says **what** and **why**, this
plan says **how**.

**Read §0 first.** It enumerates the cross-cutting contract
changes (auth Identity model, route-scoped server middleware,
schema v4, existing UI coexistence, audit semantics for console
actors) that touch supply-chain-spine code and must be sequenced
before the page-level work begins.

---

## 0. Cross-cutting contracts (must precede the PR sequence)

The page conventions in §2–§13 assume five cross-cutting
things are already true. They're not, in the current codebase.
Each one touches code the
[copilot-instructions.md](../.github/copilot-instructions.md)
calls out as supply-chain-spine and deserving of human review.

### 0.1. `auth.Identity` authorization model needs an auth-source field

Verified by reading `internal/auth/auth.go`. Today, every
authorization method (`CanRead`, `CanWrite`, `IsSystemAdmin`)
calls `HasTokenScope`, which returns `false` when `Token == nil`.
A session-authed or proxy-authed console identity (token-less by
design) would fail every authorization check.

**The change:** add a credential-kind field to `Identity` and
gate token-scope requirements on `kind == token`:

```go
type CredentialKind int
const (
    CredentialToken CredentialKind = iota  // existing PAT auth
    CredentialSession                       // console session
    CredentialProxy                         // console proxy-header
)

type Identity struct {
    User         *users.User
    Token        *tokens.Token  // nil for non-token kinds
    Memberships  map[int64]tenants.Role
    Kind         CredentialKind
}

func (id *Identity) IsSystemAdmin() bool {
    if id == nil || id.User == nil || !id.User.IsAdmin {
        return false
    }
    switch id.Kind {
    case CredentialToken:
        return id.HasTokenScope(tokens.ScopeAdmin)
    case CredentialSession, CredentialProxy:
        return true  // browser auth carries no separate scope
    }
    return false
}

func (id *Identity) CanRead(tenantID int64) bool { /* same pattern */ }
func (id *Identity) CanWrite(tenantID int64) bool { /* same pattern */ }
```

Lands as **PR 0a** (see §1). Hits `internal/auth/auth.go` plus
any call sites in registry handlers; covered by exhaustive
unit tests for each (kind × method) combination.

Why not collapse Kind into a method on Token-pointer-nil-ness?
Because we also want to be able to *deny* state-changing actions
to sessions backed by a PAT bridge in v2; a real field keeps
that door open.

### 0.2. Server middleware must move from global to route-scoped

Verified by reading `internal/server/server.go:59`. Today
`r.Use(auth.Middleware(d.Authenticator))` is mounted on the
whole engine, so every request is processed by
`TokenAuthenticator`. The plan's three-chains design needs
per-group mounting.

**The change:** lift the global `auth.Middleware` call; mount
it on the registry + admin REST groups individually; mount the
console authenticator (chosen at boot per
`PKGMIRROR_CONSOLE_AUTH_MODE`) only on the `/console` group.

```go
// internal/server/server.go (sketch after restructure)
func New(d Deps) (*gin.Engine, error) {
    r := gin.New()
    r.Use(gin.Logger(), gin.Recovery())
    r.Use(policyActorMiddleware())

    tokenAuth := auth.Middleware(d.TokenAuthenticator)

    // Registry routes: token auth only
    api := r.Group("/api/packages/:tenant", tokenAuth)
    goproxy.NewHandler(...).Register(api.Group("/go"))
    // ... all the other formats

    // Admin REST: token auth + admin gate (existing behavior)
    admin := r.Group("/admin", tokenAuth)
    admin.GET("/audit", ...)
    // ...

    // Container at root /v2 stays with tokenAuth too
    container.NewHandler(...).Register(r)  // adds tokenAuth itself

    // Console: separate authenticator chosen per config
    if d.Console != nil {
        d.Console.Register(r)  // mounts session/proxy auth + CSRF inside /console
    }
    return r, nil
}
```

Lands as **PR 0b**. Touches `internal/server/server.go` and
every registry handler that calls `auth.RequireRead` /
`RequireWrite` (the calls themselves don't change; only the
middleware wiring). Stress loop must be green before any
console work merges on top.

### 0.3. Schema v4 migration: `users.password_hash` + `users.password_set_unix`

Current schema is at `PRAGMA user_version = 3`
(`internal/db/db.go`). The console adds:

```sql
-- migration v4
ALTER TABLE users ADD COLUMN password_hash TEXT;     -- argon2id encoded
ALTER TABLE users ADD COLUMN password_set_unix INT;  -- last set timestamp
```

Both nullable. proxy-header-only deployments never populate
them. Idempotent.

**Lands as PR 2a** (see §1). The PR scope:

- the migration step in `internal/db/db.go`
- new `users.Store` methods (§0.4)
- tests that explicitly upgrade a v3 DB and open a fresh v4 DB
- a `DECISIONS.md` entry naming the schema bump
- a `docs/auth.md` update distinguishing console credentials
  from package PATs

### 0.4. `users.Store` API additions

Verified the current surface: `Create / GetByID / GetByName /
GetByExternal / TouchLastSeen`. The console needs:

```go
// New in PR 2a:
func (s *Store) GetOrCreate(ctx context.Context, name, email string) (*User, error)
func (s *Store) SetPasswordHash(ctx context.Context, id int64, hashed string) error
func (s *Store) VerifyPassword(ctx context.Context, name, plaintext string) (*User, error)
func (s *Store) ClearPassword(ctx context.Context, id int64) error
```

Proxy-header users are keyed on the external
`(provider="proxy-header", subject=<header value>)` tuple via
the existing `GetByExternal` lookup; `GetOrCreate` is the new
upsert that wraps it.

All four methods get standard table-driven tests in PR 2a.

### 0.5. Existing `internal/ui` coexists with the console

Verified: the existing public UI registers `/`, `/-/healthz`,
`/t/:tenant`, and `/t/:tenant/p/:type/*name` (Bootstrap-based,
anonymous-readable for public tenants).

**The contract:**

- `internal/ui` (existing) stays at root + `/t/...`. Public-
  facing, anonymous-readable on public tenants, no console
  auth applied.
- `internal/console` (new) lives at `/console/...` exclusively.
  Authenticated, operator-facing.
- The two share zero templates and zero static assets in v1.
  No `/static` at root — the console serves
  `/console/static/...` so route ownership is unambiguous.
- v2 may fold the public UI into the console (or rebrand it),
  but that's a separate plan.

### 0.6. Audit vs logging semantics for console actors

Logging (§3) and audit are different things.
[docs/supply-chain.md](../docs/supply-chain.md) requires every
state-changing admin action to produce an audit row, not just a
log line.

**The console adds these state changes; each emits one audit
row:**

| Action | Audit `action` | Notes |
| --- | --- | --- |
| login success | `console.login` | severity=info |
| login failure | `console.login.deny` | severity=warn; rate-limit-able |
| logout | `console.logout` | info |
| password set/reset | `console.password.set` | info; never logs the password |
| token mint | `tokens.mint` (existing) | already audited; ensure console actor is captured |
| token revoke | `tokens.revoke` (existing) | same |
| tenant create | `tenants.create` | info |
| tenant update | `tenants.update` | info; capture changed fields |
| tenant member add/remove | `tenants.member.add` / `remove` | info |
| rule create/update/delete | `rules.{create,update,delete}` | info |
| rule dry-run | (no audit; logging only) | dry-runs don't mutate |
| quarantine promote | `quarantine.promote` | info |
| quarantine reject | `quarantine.reject` | info |

**Actor population:** existing audit rows expect both
`actor_user_id` and `actor_token_id`. Console identities have no
token, so:

- `actor_user_id` = `Identity.User.ID`
- `actor_token_id` = NULL
- new column `actor_kind` (TEXT: `token` / `session` / `proxy`)
  added in the v4 migration so audit queries can filter

The `actor_kind` column addition is part of §0.3 (schema v4).

---

## 1. Phased PR sequence

The §9 effort table in web-console.md lists 12 steps totaling
~4 days. They're best landed as **ten independently shippable
PRs**, in this order. Each PR ends with the stress loop green
and is reviewable on its own.

**Two restructure PRs land BEFORE any console code** to address
the §0 cross-cutting contracts. They have nothing to do with the
console specifically; they're prerequisite cleanup to
`internal/auth` and `internal/server`.

| # | PR | What lands | Estimate |
| --- | --- | --- | --- |
| 0a | **auth Identity model** | `CredentialKind` field on `Identity`; `IsSystemAdmin` / `CanRead` / `CanWrite` switch on Kind; exhaustive unit tests; `DECISIONS.md` entry | 0.5 day |
| 0b | **server route-scoped auth** | Lift `auth.Middleware` from global to per-group in `internal/server/server.go`; registry + admin REST keep `TokenAuthenticator`; no console code yet | 0.5 day |
| 1 | **foundation** | `Console` struct, embed.FS loader, layout + base CSS, `/console/_ping`, dev-dir override, security headers middleware, Tailwind binary pinning | 1.5 days |
| 2a | **schema v4 + users** | Migration for `users.password_hash` + `password_set_unix` + `audit.actor_kind`; new `users.Store` methods (`GetOrCreate`, `SetPasswordHash`, `VerifyPassword`, `ClearPassword`); migration test (v3 → v4 + fresh v4) | 0.5 day |
| 2b | **password auth** | `SessionAuthenticator`, login/logout, gorilla/sessions CookieStore, CSRF, rate-limited login route, token-verified set-password flow for bootstrap | 0.75 day |
| 2c | **proxy-header auth** | `ProxyHeaderAuthenticator`, trusted-proxies IP allowlist + tests for spoofed `X-Forwarded-For`, `PKGMIRROR_CONSOLE_BOOTSTRAP_ADMIN` one-shot promotion, audit row on promotion | 0.5 day |
| 3 | **dashboard** | `pages/home/` — first real page, read-only, exercises the layout + partials. **Prerequisite helpers:** `Tenants.Count`, `Models.CountPackages`, `Models.RecentIngests`, `Audit.DecisionRollup` — list each as a PR deliverable | 0.5 day |
| 4 | **tenants** | `pages/tenants/` — list, detail, members. First page with state-changing forms + flash messages. All destructive actions use `POST .../delete` not `DELETE ...` (HTML form constraint) | 0.5 day |
| 5 | **packages (browse + quarantine only)** | `pages/packages/` — per-tenant list + version detail + quarantine promote/reject. **Raw delete is deferred to v2** (overlaps with quarantine forensics; needs a separate flow with confirmation + audit + retention policy) | 0.5 day |
| 6 | **audit** | `pages/audit/` — query + detail. Read-only but exercises pagination + filtering | 0.5 day |
| 7 | **rules** | `pages/rules/` — list, edit, dry-run preview. Most complex page; biggest form. **Prerequisite:** rules dry-run query helper to reconstruct past `Subject` objects; carved out as its own sub-slice | 1 day |
| 8 | **tokens + profile** | `pages/tokens/` + `pages/profile/` — mint/revoke + own-profile management | 0.5 day |

PR 0a and 0b unblock everything else. PR 1 is the console
foundation; PR 2a/b/c unblock all subsequent pages. PRs 3–8
can ship in any order after PR 2c.

For each PR after #1: copy an existing page-package as the
template, replace the handler body, add the
`{{define "pages/<area>/<name>"}}` template, drop in a test that
follows the convention in §11. The repeated steps are
intentionally mechanical.

---

## 2. File-by-file walkthrough

Files marked **★** are worked examples — the implementer should
read them carefully before writing the others. Files marked **○**
are one-liners or boilerplate; copy-from-template once and forget.

### 2.1. The core (PR 1)

#### ★ `internal/console/console.go`

The single file the rest of the package centers on. ~150 lines.
Contains:

- `Console` struct (the DSO) with all the deps named in
  web-console.md §4.2
- `Config` struct + `LoadConfig()` from env
- `New(deps Deps) (*Console, error)` constructor that:
  - Loads templates (calls `loadTemplates` in `templates.go`)
  - Builds the chosen `Authenticator` based on `Config.AuthMode`
  - Builds the session store
  - Wires the CSRF middleware
- `Register(r *gin.Engine)` that mounts all route groups
- `Render(c *gin.Context, page string, data any)` helper that:
  - Wraps `data` in a `pageWrapper{Base BaseData, Body any}`
  - Populates `BaseData` (current identity, current tenant, flash
    messages, CSRF token, nav state, app version)
  - Executes the `layouts/base` template

```go
// Sketch — full implementation in PR 1.
func (c *Console) Render(gc *gin.Context, page string, body any) {
    if !c.Config.Enabled {
        gc.String(http.StatusNotFound, "console disabled")
        return
    }
    base, err := c.baseData(gc)
    if err != nil {
        c.RenderError(gc, "build page chrome", err)
        return
    }
    wrapper := struct {
        Page string
        Base BaseData
        Body any
    }{
        Page: page,
        Base: base,
        Body: body,
    }
    gc.HTML(http.StatusOK, "layouts/base", wrapper)
}
```

The layout template uses the `tmpl` template func
(§2.1.templates) to dispatch on `.Page` — a tiny helper that
looks up the named template, executes it into a buffer, and
returns the result as `template.HTML` (errors surfaced as
logged-and-rendered server errors). Far cleaner than a giant
`if/else` and the only thing that scales as the page count
grows past five.

```html
{{ define "layouts/base" }}<!doctype html>
<html><head>...</head><body>
  <nav>...</nav>
  <main>
    {{ template "partials/flash" .Base.Flash }}
    {{ tmpl .Page .Body }}  {{/* dispatches to e.g. pages/tenants/list */}}
  </main>
</body></html>{{ end }}
```

Per-page external JS scripts come in via
`.Base.ScriptURLs []string` (populated by the page handler
before calling `c.Render`). Per-page inline scripts go directly
inside the page template's body. We **deliberately do not** use
`{{ define "page-js" }}` blocks because `html/template` has a
flat namespace and the last-parsed wins — every page after the
first would silently lose its scripts. See §2.3 base layout.

#### ★ `internal/console/templates.go`

The recursive `embed.FS` walker described in web-console.md §4.5.
~50 lines. Plus the template func map:

```go
var funcs = template.FuncMap{
    // CSRF: NOT a func that magically receives the request — the
    // pre-rendered hidden input is populated into BaseData.CSRFField
    // by baseData(); templates emit it as {{ .Base.CSRFField }}.
    "default":     defaultFunc,      // {{ .X | default "fallback" }}
    "dict":        dictFunc,         // {{ template "x" (dict "k" "v") }}
    "humanBytes":  humanBytesFunc,
    "humanTime":   humanTimeFunc,    // relative time ago
    "timefmt":     timefmtFunc,
    "lower":       strings.ToLower,
    "upper":       strings.ToUpper,
    "title":       titleFunc,
    "join":        strings.Join,
    "tmpl":        nil,              // bound per-request — see below
}
```

The `tmpl` template func dispatches the layout's body slot to a
named page template. It can't be a global function value because
it needs the template set it was parsed into; it's bound at
parse time in `loadTemplates`:

```go
func loadTemplates(funcs template.FuncMap, overrideDir string) (*template.Template, error) {
    // ... parse all .tmpl files as in web-console.md S4.5 ...
    root.Funcs(template.FuncMap{
        "tmpl": func(name string, data any) (template.HTML, error) {
            var buf bytes.Buffer
            if err := root.ExecuteTemplate(&buf, name, data); err != nil {
                return "", fmt.Errorf("tmpl %q: %w", name, err)
            }
            return template.HTML(buf.String()), nil  //nolint:gosec // output is template-rendered, not user input
        },
    })
    return root, nil
}
```

Errors from `tmpl` surface as Go template execution errors
(caught by `gin.Recover` middleware — renders a friendly 500).

The `dict` func is the only other non-obvious one and it's the
one that makes UI partials usable from page templates:

```go
func dictFunc(args ...any) (map[string]any, error) {
    if len(args)%2 != 0 {
        return nil, fmt.Errorf("dict: odd number of args")
    }
    m := make(map[string]any, len(args)/2)
    for i := 0; i < len(args); i += 2 {
        key, ok := args[i].(string)
        if !ok { return nil, fmt.Errorf("dict: non-string key at %d", i) }
        m[key] = args[i+1]
    }
    return m, nil
}
```

#### ○ `internal/console/embed.go`

One file, one purpose:

```go
package console

import "embed"

//go:embed pages layouts partials static
var embeddedFS embed.FS
```

#### `internal/console/types.go`

The shared structs:

```go
type BaseData struct {
    Identity     *auth.Identity     // nil for anonymous
    Memberships  []*tenants.Tenant  // tenants the user can access
    ActiveTenant *tenants.Tenant    // nil unless we're under /console/t/:tenant
    Flash        []FlashMessage     // see S6
    CSRFField    template.HTML      // pre-rendered <input type="hidden" ...>, populated by baseData()
    ScriptURLs   []string           // per-page external JS to <script src="..."> in the layout
    AppVersion   string
    ConsolePath  string             // e.g. "/console" — for href-building
    DarkMode     bool               // resolved from cookie / config default
    PageTitle    string
    Now          time.Time
}

type FlashMessage struct {
    Severity string  // "info" | "success" | "warning" | "danger"
    Text     string
}
```

`baseData()` populates `CSRFField` via `csrf.TemplateField(c.Request)`
(gorilla/csrf does need the request) so templates only ever
need to emit `{{ .Base.CSRFField }}`. No magic template func
that can't see the request.

### 2.2. Middleware (PR 1 foundation + PR 2 auth)

#### ★ `internal/console/middleware/auth.go`

Two `Authenticator` implementations + the gate helpers. Both
modes here, mode chosen at boot via `Console.Config.AuthMode`.
~200 lines.

Auth code is supply-chain spine; these sketches model real error
handling (DB failure ⇒ fail closed; missing credentials ⇒
anonymous).

```go
// Mode selector: returned by Console.New() based on config.
func NewAuthenticator(cfg Config, users *users.Store, tenants *tenants.Store) (auth.Authenticator, error) {
    switch cfg.AuthMode {
    case "proxy-header":
        if len(cfg.TrustedProxies) == 0 {
            return nil, fmt.Errorf("PKGMIRROR_CONSOLE_TRUSTED_PROXIES required in proxy-header mode")
        }
        return newProxyHeader(cfg, users, tenants), nil
    case "password":
        return newSession(cfg, users, tenants), nil
    default:
        return nil, fmt.Errorf("unknown PKGMIRROR_CONSOLE_AUTH_MODE=%q", cfg.AuthMode)
    }
}

// Mode A: trust an upstream-set header from a trusted source.
func (a *ProxyHeaderAuthenticator) Authenticate(ctx context.Context, r *http.Request) (*auth.Identity, error) {
    if !a.isTrustedSource(r) {
        return nil, nil  // anonymous; headers ignored unconditionally
    }
    user := strings.TrimSpace(r.Header.Get(a.UserHeader))
    if user == "" {
        return nil, nil  // anonymous
    }
    u, err := a.Users.GetOrCreate(ctx, user, r.Header.Get(a.EmailHeader))
    if err != nil {
        return nil, fmt.Errorf("proxy-header auth: get/create user %q: %w", user, err)
    }
    mem, err := a.Tenants.Memberships(ctx, u.ID)
    if err != nil {
        return nil, fmt.Errorf("proxy-header auth: memberships for user %d: %w", u.ID, err)
    }
    return &auth.Identity{User: u, Memberships: mem, Kind: auth.CredentialProxy}, nil
}

// Mode B: session cookie set by /console/login.
func (a *SessionAuthenticator) Authenticate(ctx context.Context, r *http.Request) (*auth.Identity, error) {
    sess, err := a.Sessions.Get(r, "pkgmirror_session")
    if err != nil {
        // gorilla/sessions returns a fresh session + err on decode failure;
        // treat as anonymous + log so we notice persistent corruption.
        slog.Warn("session decode failed; treating as anonymous", "err", err)
        return nil, nil
    }
    uid, ok := sess.Values["user_id"].(int64)
    if !ok {
        return nil, nil  // not logged in
    }
    u, err := a.Users.GetByID(ctx, uid)
    if err != nil {
        if errors.Is(err, users.ErrNotExist) {
            return nil, nil  // session for a deleted user → anonymous
        }
        return nil, fmt.Errorf("session auth: get user %d: %w", uid, err)
    }
    mem, err := a.Tenants.Memberships(ctx, u.ID)
    if err != nil {
        return nil, fmt.Errorf("session auth: memberships for user %d: %w", u.ID, err)
    }
    return &auth.Identity{User: u, Memberships: mem, Kind: auth.CredentialSession}, nil
}
```

The authenticator middleware in `internal/auth` already returns
any non-nil `error` from `Authenticate` to the recovery
middleware (which renders a 500). Returning `nil, nil` is the
"anonymous; let route gates decide" path. **The rule:** missing
credentials are anonymous; DB/session corruption is an error;
console auth fails closed.

Gates registered as gin middleware (called from console.go's
Register):

```go
func (m *Middleware) RequireAuth() gin.HandlerFunc {
    return func(c *gin.Context) {
        id := auth.FromContext(c)
        if id == nil {
            if m.AuthMode == "password" {
                // PRG pattern: stash return-to, redirect to login
                sess := sessions.Default(c)
                sess.Set("return_to", c.Request.URL.RequestURI())
                if err := sess.Save(); err != nil {
                    slog.Warn("return-to save failed", "err", err)
                }
                c.Redirect(http.StatusSeeOther, "/console/login")
            } else {
                c.String(http.StatusUnauthorized, "unauthorized: check proxy auth headers")
            }
            c.Abort()
            return
        }
        c.Next()
    }
}

func (m *Middleware) RequireSystemAdmin() gin.HandlerFunc { /* checks id.IsSystemAdmin() */ }
func (m *Middleware) RequireTenantMember() gin.HandlerFunc { /* checks :tenant param against id.Memberships */ }
func (m *Middleware) RequireTenantAdmin() gin.HandlerFunc { /* same but role=admin */ }
```

#### ★ `internal/console/middleware/csrf.go`

Wraps gorilla/csrf as a gin middleware. ~40 lines. The hidden
input is exposed via `BaseData.CSRFField` (populated in
`baseData()`), not a template func — template funcs have no
way to receive the request, and gorilla/csrf's token nonce
lives there.

```go
func CSRF(secretKey []byte, secure bool) gin.HandlerFunc {
    inner := csrf.Protect(secretKey,
        csrf.Secure(secure),
        csrf.HttpOnly(true),
        csrf.SameSite(csrf.SameSiteLaxMode),
        csrf.Path("/console"),
        csrf.ErrorHandler(http.HandlerFunc(csrfErrorPage)),
    )
    return func(c *gin.Context) {
        inner(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            c.Request = r  // gorilla/csrf adds the token to request context
            c.Next()
        })).ServeHTTP(c.Writer, c.Request)
    }
}

func csrfErrorPage(w http.ResponseWriter, r *http.Request) {
    http.Error(w, "Form expired or tampered. Please go back and try again.", http.StatusForbidden)
    // PR 4 upgrade: render a proper friendly HTML page via the Console.
}
```

`baseData()` populates `BaseData.CSRFField` once per request:

```go
// internal/console/console.go (part of baseData)
base.CSRFField = csrf.TemplateField(gc.Request)
```

Templates use `{{ .Base.CSRFField }}` (see §5.2).

#### ★ `internal/console/middleware/securityheaders.go` (new for PR 1)

Baseline browser security headers on every `/console` response.
This is non-negotiable for an admin UI.

```go
func SecurityHeaders(usingTLS bool) gin.HandlerFunc {
    return func(c *gin.Context) {
        h := c.Writer.Header()
        // CSP: no inline scripts (we use external scripts + ScriptURLs);
        // no inline styles either; allow our own font + asset origin only.
        h.Set("Content-Security-Policy",
            "default-src 'self'; script-src 'self'; style-src 'self'; "+
            "img-src 'self' data:; font-src 'self'; "+
            "frame-ancestors 'none'; base-uri 'self'")
        h.Set("X-Content-Type-Options", "nosniff")
        h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
        h.Set("X-Frame-Options", "DENY")   // redundant with CSP frame-ancestors but defensive
        if usingTLS {
            h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
        }
        c.Next()
    }
}
```

The CSP choice of `script-src 'self'` is exactly why we removed
`{{ define "page-js" }}` blocks — inline scripts would violate
the policy. All console JS lives in `/console/static/js/*.js`
files.

#### ○ `internal/console/middleware/session.go`

Direct gin-contrib/sessions wiring. ~30 lines. Cookie-only store
(no SQLite session table per web-console.md §5.4).

**Key encoding:** env vars are **hex-encoded** strings. The
config loader decodes once and validates the decoded byte
length. Mixing "64 hex chars" with "64 bytes" is a real bug —
the v0.1 of this plan had that mismatch.

```go
func NewSessionMiddleware(cfg Config) (gin.HandlerFunc, error) {
    // cfg.SessionAuthKey and cfg.SessionEncKey are []byte values
    // already decoded from hex by config.Load() (see S8.1).
    // Auth key: 64 raw bytes. Enc key: 32 raw bytes.
    if len(cfg.SessionAuthKey) != 64 {
        return nil, fmt.Errorf("session auth key: need 64 bytes after hex decode, got %d", len(cfg.SessionAuthKey))
    }
    if len(cfg.SessionEncKey) != 32 {
        return nil, fmt.Errorf("session enc key: need 32 bytes after hex decode, got %d", len(cfg.SessionEncKey))
    }
    store := cookie.NewStore(cfg.SessionAuthKey, cfg.SessionEncKey)
    store.Options(sessions.Options{
        Path:     "/console",
        MaxAge:   int(cfg.SessionTTL.Seconds()),
        Secure:   cfg.UsingTLS,
        HttpOnly: true,
        SameSite: http.SameSiteLaxMode,
    })
    gob.Register(FlashMessage{})  // so gorilla can serialize our flash type
    return sessions.Sessions("pkgmirror_session", store), nil
}
```

#### ★ `internal/console/middleware/flash.go`

The flash convention is the user-facing equivalent of a log
line. ~50 lines.

```go
type FlashMessage struct {
    Severity string  // see constants
    Text     string
}

const (
    FlashInfo    = "info"
    FlashSuccess = "success"
    FlashWarning = "warning"
    FlashDanger  = "danger"
)

// AddFlash queues a message to display on the next page render.
// Called from handlers; consumed by Console.baseData on the next request.
func AddFlash(c *gin.Context, severity, text string) {
    sess := sessions.Default(c)
    sess.AddFlash(FlashMessage{Severity: severity, Text: text})
    if err := sess.Save(); err != nil {
        slog.Error("flash save", "err", err)  // non-fatal; log + continue
    }
}

// ReadFlashes returns and clears all queued flash messages.
func ReadFlashes(c *gin.Context) []FlashMessage {
    sess := sessions.Default(c)
    raw := sess.Flashes()
    if len(raw) == 0 { return nil }
    out := make([]FlashMessage, 0, len(raw))
    for _, f := range raw {
        if m, ok := f.(FlashMessage); ok {
            out = append(out, m)
        }
    }
    _ = sess.Save()  // clear consumed flashes
    return out
}

// Convenience wrappers; pick from these instead of calling AddFlash directly.
func FlashSavedFlash(c *gin.Context, what string) { AddFlash(c, FlashSuccess, fmt.Sprintf("%s saved.", what)) }
func FlashDeletedFlash(c *gin.Context, what string) { AddFlash(c, FlashSuccess, fmt.Sprintf("%s deleted.", what)) }
func FlashErrorFlash(c *gin.Context, what string, err error) { AddFlash(c, FlashDanger, fmt.Sprintf("Couldn't %s: %s", what, err)) }
```

Modeled on go_gin_starter's `addFlash` / `getFlashes`, but
typed (a struct with severity) rather than bare strings.

#### ○ `internal/console/middleware/recover.go`

Custom panic recovery that renders a friendly 500 page rather
than gin's default text dump. ~30 lines. Logs the panic via
slog with a request-id field.

#### ○ `internal/console/middleware/logger.go`

Per-request slog line. Fields:
`method`, `path`, `status`, `latency_ms`, `bytes`,
`user_id` (if authed), `ip` (if proxy-header mode with trusted source),
`request_id`.

### 2.3. Layouts + partials (PR 1)

#### ★ `internal/console/layouts/base.tmpl`

The HTML shell. ~80 lines. Sections:

```html
{{ define "layouts/base" }}<!doctype html>
<html lang="en" class="{{ if .Base.DarkMode }}dark{{ end }}">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>{{ .Base.PageTitle | default "pkgmirror" }}</title>
  <link rel="stylesheet" href="/console/static/css/console.css?v={{ .Base.AppVersion }}">
  <link rel="icon" href="/console/static/favicon.ico">
</head>
<body class="bg-background text-foreground antialiased">
  <div class="flex min-h-screen">
    {{ template "partials/sidebar" .Base }}
    <main class="flex-1 p-6">
      {{ template "partials/flash" .Base.Flash }}
      {{ template "partials/breadcrumb" .Base }}
      {{ tmpl .Page .Body }}
    </main>
  </div>
  <script src="/console/static/js/console.js?v={{ .Base.AppVersion }}" defer></script>
  {{/* Per-page external JS via Base.ScriptURLs; no inline scripts — see S2.1 and S2.2 security headers (CSP forbids inline). */}}
  {{ range .Base.ScriptURLs }}
  <script src="{{ . }}" defer></script>
  {{ end }}
</body>
</html>{{ end }}
```

**No `{{ define "page-js" }}` block** — `html/template` has a
flat namespace; multiple page templates defining the same block
name would silently overwrite each other. Per-page scripts come
in via `BaseData.ScriptURLs []string`, populated by the handler
before calling `c.Render`. Inline scripts are forbidden by the
CSP set in `middleware/securityheaders.go`.

#### `internal/console/layouts/auth.tmpl`

Minimal chrome for the login page. No sidebar, no auth-required
navigation. ~30 lines.

#### ★ `internal/console/partials/flash.tmpl`

```html
{{ define "partials/flash" }}
{{ range . }}
<div class="rounded-md p-3 mb-3 border
            {{ if eq .Severity "success" }}bg-green-50 border-green-200 text-green-900 dark:bg-green-950 dark:text-green-100
            {{ else if eq .Severity "warning" }}bg-yellow-50 border-yellow-200 text-yellow-900 dark:bg-yellow-950 dark:text-yellow-100
            {{ else if eq .Severity "danger"  }}bg-red-50   border-red-200   text-red-900   dark:bg-red-950   dark:text-red-100
            {{ else }}                            bg-blue-50  border-blue-200  text-blue-900  dark:bg-blue-950  dark:text-blue-100
            {{ end }}">
  {{ .Text }}
</div>
{{ end }}
{{ end }}
```

#### `internal/console/partials/sidebar.tmpl`, `breadcrumb.tmpl`, `pagination.tmpl`, `tenantswitcher.tmpl`

Standard partials. Each ~20–40 lines. Modeled on copilot-api's
`AppSidebar` but in `html/template`.

#### `internal/console/partials/ui/{button,card,table,formfield,alert}.tmpl`

The starter UI primitive partials. Each ~10–30 lines. Examples
in web-console.md §4.8.

### 2.4. First worked page (PR 3 dashboard)

#### ★ `internal/console/pages/home/home.go`

```go
package home

import (
    "github.com/astockwell/pkgmirror/internal/console"
    "github.com/gin-gonic/gin"
)

type dashboardData struct {
    TotalTenants   int
    TotalPackages  int
    RecentIngests  []ingestSummary
    PolicyDecisions []decisionSummary
}

type ingestSummary struct {
    TenantName string
    Format     string
    Package    string
    Version    string
    WhenUnix   int64
}

type decisionSummary struct {
    Decision string
    Count    int64
}

func Register(c *console.Console, g *gin.RouterGroup) {
    g.GET("", dashboard(c))
}

func dashboard(c *console.Console) gin.HandlerFunc {
    return func(gc *gin.Context) {
        ctx := gc.Request.Context()

        totalT, err := c.Tenants.Count(ctx)
        if err != nil {
            c.RenderError(gc, "count tenants", err)
            return
        }
        totalP, _ := c.Models.CountPackages(ctx)
        recent, _ := c.Models.RecentIngests(ctx, 10)
        decisions, _ := c.Audit.DecisionRollup(ctx, time.Now().Add(-24*time.Hour), time.Now())

        c.Render(gc, "pages/home/dashboard", dashboardData{
            TotalTenants:    totalT,
            TotalPackages:   totalP,
            RecentIngests:   mapRecent(recent),
            PolicyDecisions: mapDecisions(decisions),
        })
    }
}
```

**Prerequisite helpers** for PR 3 — these methods do not exist
in the current codebase and must land as part of PR 3:

- `tenants.Store.Count(ctx) (int, error)`
- `models.Store.CountPackages(ctx) (int, error)`
- `models.Store.RecentIngests(ctx, limit) ([]IngestRow, error)`
- `audit.Logger.DecisionRollup(ctx, since, until) (map[string]int64, error)`

List them in the PR 3 description so reviewers know about the
cross-package work. Same pattern for PR 7 (rules dry-run
needs a `Subject`-reconstructor query helper, carved out as
its own sub-slice within the PR).

The pattern is dictated: parse params, call services, build the
typed Data struct, call `c.Render`. No anonymous structs. No
direct `gc.HTML` calls. Errors go through `c.RenderError` (§4).

#### `internal/console/pages/home/home.tmpl`

```html
{{ define "pages/home/dashboard" }}
<h1 class="text-2xl font-semibold mb-4">Dashboard</h1>

<div class="grid grid-cols-1 md:grid-cols-3 gap-4 mb-6">
  {{ template "ui/card" (dict "title" "Tenants" "value" .TotalTenants) }}
  {{ template "ui/card" (dict "title" "Packages" "value" .TotalPackages) }}
  {{ template "ui/card" (dict "title" "Recent" "value" (len .RecentIngests)) }}
</div>

<h2 class="text-xl font-semibold mt-6 mb-2">Recent ingests</h2>
{{ template "ui/table" (dict "headers" (slice "Tenant" "Format" "Package" "When") "rows" .RecentIngests) }}
{{ end }}
```

### 2.5. First worked page with a form (PR 4 tenants)

#### ★ `internal/console/pages/tenants/create.go`

The canonical write-handler pattern: parse → validate →
service-call → redirect-with-flash, or re-render-with-errors.

```go
type createForm struct {
    Name        string  // POST form field
    Visibility  string
    Description string

    // Computed on re-render after a validation failure:
    Errors map[string]string
}

func create(c *console.Console) gin.HandlerFunc {
    return func(gc *gin.Context) {
        f := createForm{
            Name:        strings.TrimSpace(gc.PostForm("name")),
            Visibility:  gc.PostForm("visibility"),
            Description: strings.TrimSpace(gc.PostForm("description")),
            Errors:      map[string]string{},
        }

        // Validation — hand-rolled per web-console.md §7 "no validator/v10 for v1"
        if f.Name == "" {
            f.Errors["name"] = "Required."
        } else if !tenantNamePattern.MatchString(f.Name) {
            f.Errors["name"] = "Letters, digits, hyphens only."
        }
        if f.Visibility != "public" && f.Visibility != "private" {
            f.Errors["visibility"] = "Must be public or private."
        }
        if len(f.Errors) > 0 {
            c.Render(gc, "pages/tenants/new", f)  // re-render the form with errors
            return
        }

        ctx := gc.Request.Context()
        t, err := c.Tenants.Create(ctx, f.Name, f.Visibility, f.Description)
        if err != nil {
            if errors.Is(err, tenants.ErrDuplicate) {
                f.Errors["name"] = "A tenant with that name already exists."
                c.Render(gc, "pages/tenants/new", f)
                return
            }
            c.RenderError(gc, "create tenant", err)
            return
        }

        middleware.AddFlash(gc, middleware.FlashSuccess, fmt.Sprintf("Tenant %q created.", t.Name))
        gc.Redirect(http.StatusSeeOther, "/console/admin/tenants/"+strconv.FormatInt(t.ID, 10))
    }
}
```

This pattern is **the** load-bearing convention. Every
state-changing handler follows it.

### 2.6. Static assets (PR 1)

#### `internal/console/static/css/input.css`

```css
@import "tailwindcss";

/* Theme tokens lifted from copilot-api/cmd/admin/static/css/.
 * Those are in turn a port of shadcn/ui's design tokens
 * (https://ui.shadcn.com, MIT). Both upstreams are MIT-licensed
 * and reuse is explicit; this comment is the source-of-record
 * attribution per the project's no-silent-derived-files rule.
 */
@custom-variant dark (&:where(.dark, .dark *));

@theme inline {
  --color-background: var(--background);
  --color-foreground: var(--foreground);
  --color-card: var(--card);
  --color-primary: var(--primary);
  /* ... full set from copilot-api ... */
}

:root {
  --background: oklch(1 0 0);
  --foreground: oklch(0.145 0 0);
  /* ... light-mode token values ... */
}

.dark {
  --background: oklch(0.145 0 0);
  --foreground: oklch(0.985 0 0);
  /* ... dark-mode token values ... */
}
```

#### `internal/console/static/js/console.js`

Vanilla JS for two global behaviors: dark-mode toggle
persistence + dismissible flash messages. ~50 lines. Per-page
JS is added by appending URLs to `BaseData.ScriptURLs` in the
handler before calling `c.Render`; the layout iterates them and
emits `<script src="..." defer>` tags. No inline scripts — the
CSP set in `middleware/securityheaders.go` forbids them.

#### `internal/console/tailwind.config.js`

```js
module.exports = {
  content: [
    "./internal/console/pages/**/*.tmpl",
    "./internal/console/partials/**/*.tmpl",
    "./internal/console/layouts/**/*.tmpl",
  ],
  darkMode: 'class',
  theme: { extend: {} },
}
```

### 2.7. Tests (every PR)

#### ★ `internal/console/testutil/fixture.go`

One helper, one job: spin up a `*Console` against a fresh
SQLite + an empty `users`/`tenants` store, return a struct that
makes assertions easy.

```go
type Fixture struct {
    T        *testing.T
    Console  *console.Console
    Server   *httptest.Server
    Client   *http.Client          // cookie jar; preserves session
    Tenant   *tenants.Tenant       // pre-created "default"
    Admin    *users.User           // pre-created admin
    Password string                // for mode=password tests
}

func New(t *testing.T) *Fixture { /* ~80 lines */ }

func (f *Fixture) Login(t *testing.T) {
    // POST /console/login with f.Admin.Username + f.Password
    // Client's cookie jar now holds the session
}

func (f *Fixture) Get(t *testing.T, path string) (*http.Response, []byte) { /* ... */ }
func (f *Fixture) PostForm(t *testing.T, path string, vals url.Values) (*http.Response, []byte) {
    // Auto-fetches the CSRF token first via a GET to the form's page,
    // includes it in the POST. See §5.
}
```

Each page package's `<page>_test.go` follows this shape (PR 3+
onward):

```go
func TestDashboard_RendersForAdmin(t *testing.T) {
    f := testutil.New(t)
    f.Login(t)
    resp, body := f.Get(t, "/console/")
    if resp.StatusCode != 200 { t.Fatalf("status=%d body=%s", resp.StatusCode, body) }
    if !bytes.Contains(body, []byte("Dashboard")) { t.Fatalf("missing heading") }
}

func TestDashboard_Anon401(t *testing.T) {
    f := testutil.New(t)
    resp, _ := f.Get(t, "/console/")
    // password mode → 303 redirect to /console/login
    if resp.StatusCode != http.StatusSeeOther { t.Fatalf("status=%d", resp.StatusCode) }
}
```

---

## 3. Logging conventions

We use **`log/slog`** (standard library since Go 1.21). No
logging dep beyond the stdlib. The console package gets a
named logger via `slog.Logger` injected on `Console.Logger`.

### 3.1. Levels

| Level | When | Example |
| --- | --- | --- |
| `Debug` | Per-request tracing, dev-only volume | "rendered page", "session loaded" |
| `Info` | User-meaningful events; one per state change | "tenant created", "rule edited", "user logged in" |
| `Warn` | Recovered errors; suspicious-but-handled | "csrf token mismatch", "rate limit triggered", "session save failed but request continued" |
| `Error` | Unrecovered system failures | DB connection lost, template parse error, panic recovered |

**Per-request access log lines are `Info`**, one per request,
emitted by `middleware/logger.go`. Higher-volume traces go to
`Debug` and are off in prod.

### 3.2. Structured fields

Every log line uses key=value structured args, never `fmt.Sprintf`:

```go
slog.Info("tenant created",
    "tenant_id", t.ID,
    "tenant_name", t.Name,
    "actor_user_id", id.User.ID,
    "request_id", requestID(c),
)
```

Standard fields, names baked into convention:

| Field | When | Source |
| --- | --- | --- |
| `request_id` | Every log line in handler/middleware | Generated by recover/logger middleware |
| `method`, `path`, `status`, `latency_ms` | Access log | logger middleware |
| `actor_user_id`, `actor_username` | Any line in a authed handler | `auth.FromContext(c)` |
| `tenant_id`, `tenant_name` | Any line referencing a tenant | The handler |
| `err` | Always when level=Error | The error |

Reusing the same key names across packages makes log search
trivial. No `"user"` in one place and `"username"` in another.

### 3.3. What NOT to log

- Passwords, hashes, session tokens, CSRF tokens, PATs
- Full request bodies (could contain secrets)
- The `Authorization` header (could be set in proxy-header mode
  with a JWT that includes claims)
- The session cookie contents

A small helper in `middleware/logger.go` redacts these from any
captured request headers before logging.

---

## 4. Error handling conventions

Two categories. The handler must decide which it has.

### 4.1. User errors (4xx — show to the user)

- Form validation failure → re-render the form with `Errors` map
  populated (worked example in §2.5). HTTP 200 + same template.
  No flash; the inline field errors are the feedback.
- Not found → `c.RenderNotFound(gc, "tenant %q not found", name)`.
  Renders a friendly 404 page with `Base` chrome intact.
- Forbidden → `c.RenderForbidden(gc, "you don't have access to this tenant")`.
  Renders a friendly 403 with same chrome.
- Unauthenticated (auth mode dependent):
  - mode=password → 303 redirect to login, with `return_to` stashed
  - mode=proxy-header → 401 page suggesting proxy misconfig
- Conflict (duplicate) → re-render the form with the conflicting
  field's error set, same as validation.

### 4.2. System errors (5xx — show to the user, log to ops)

- Anything else from a service call.
- Pattern: `c.RenderError(gc, "create tenant", err)` which:
  1. Logs at Error level with the full `err`, the `request_id`,
     the `actor_user_id`, and the verb ("create tenant")
  2. Renders a friendly 500 page that shows the user the verb
     and the `request_id` (so support can find the log line)
     but **never** the raw `err` text (might leak DB schema or
     internal paths)

### 4.3. Panics

The recover middleware catches them, logs with `level=Error
panic=true stack=<runtime>`, and renders the same 500 page as
above. The request never silently 200s and the user always sees
the same friendly message.

### 4.4. What goes to flash vs inline vs render-error

| Situation | Where the message goes |
| --- | --- |
| Field validation fail | Inline next to the field |
| Conflict (e.g. duplicate name) | Inline next to the field |
| Successful state change | **Flash** (success severity), then redirect (PRG) |
| Successful read-only navigation | Nothing — just the page |
| Authentication failed (login) | Inline on the login form, never flash (don't leak across users on shared computers) |
| System error (DB down, etc.) | RenderError page; no flash |
| User logged out | Flash (info severity), redirect to login |
| Backgrounded job kicked off | Flash (info, "Started rebuilding indexes; this may take a moment.") |
| Bulk action partial success | Flash (warning, "Quarantined 12 of 14 versions. 2 failed: …") |

### 4.5. Audit vs logging

Logging (§3) and audit are different. Logs are operational
breadcrumbs for ops; audit rows are compliance / forensic /
supply-chain artifacts queryable via `/admin/audit`.

Every state-changing console action produces exactly one
audit row, per the table in §0.6 (Cross-cutting contracts).
Logging continues independently — a state change emits both
an audit row AND an `Info` slog line, by convention.

Non-state-changing actions (page loads, search, dry-runs) emit
logs only; no audit rows.

Actor population on audit rows:

- `actor_user_id` = `Identity.User.ID` (always present for
  authenticated console requests)
- `actor_token_id` = NULL for session/proxy console identities
  (no token involved)
- `actor_kind` = `"session"` or `"proxy"` (the new column
  added in the v4 migration; see §0.6)

A dashboard widget on `/console/` can show "recent actions by
me" by filtering audit on `actor_user_id = me`.

---

## 5. Form handling conventions

### 5.1. POST-Redirect-GET (PRG) is mandatory

Every successful POST `Redirect(303 SeeOther)` to a fresh URL.
Never render directly from a POST handler on success — refresh
would re-submit the form.

```go
// good
c.Tenants.Create(...)
middleware.FlashSuccess(c, "Tenant created.")
gc.Redirect(http.StatusSeeOther, "/console/admin/tenants")

// bad
c.Tenants.Create(...)
c.Render(gc, "pages/tenants/list", listData{...})  // refresh = double-create
```

### 5.2. Validation re-render preserves user input

When validation fails, the re-render gets the **submitted**
values back so the user doesn't retype. Errors map populates
field-level error messages. The form template:

```html
{{ define "pages/tenants/new" }}
<form method="post" action="/console/admin/tenants">
  {{ .Base.CSRFField }}
  {{ template "ui/formfield" (dict
        "label" "Name"
        "name" "name"
        "value" .Body.Name
        "error" (index .Body.Errors "name")
        "required" true) }}
  {{ template "ui/formfield" (dict
        "label" "Visibility"
        "name" "visibility"
        "value" .Body.Visibility
        "error" (index .Body.Errors "visibility")
        "type" "select"
        "options" (slice "public" "private")) }}
  <button type="submit">Create</button>
</form>
{{ end }}
```

Note `.Base.CSRFField` (not `{{ csrfField }}`) and `.Body.Name`
(not `.Name`) — the page template receives the wrapper struct
`{Base, Body}` defined in §2.1. The `ui/formfield` partial
handles the error display + the input population uniformly.
Five lines per field is the bar.

### 5.3. CSRF tokens

Every form: `{{ .Base.CSRFField }}` at the top. The hidden
input is populated in `baseData()` via
`csrf.TemplateField(gc.Request)` (see §2.2 csrf middleware).
Forgetting it means the POST 403s and the user sees the
friendly CSRF error page — fail loud.

We deliberately do **not** ship a `{{ csrfField }}` template
func, because template funcs have no way to receive the request
and gorilla/csrf's token nonce lives there. The `.Base.CSRFField`
path is the one true way.

Test fixture (§2.7) handles tokens automatically by GET-ing the
form page first, scraping the token, and including it in the
POST. Manual test reproduction works the same way.

### 5.4. No validator/v10 for v1

Hand-rolled validation per handler. Reasons:

- Small forms (login, tenant CRUD, rule edit) — ~5 forms total
- The validation logic is right next to the handler, not in
  struct tags somewhere else
- Less magic = less debug pain
- Adding `validator/v10` later is mechanical if forms grow

The trade-off is acknowledged: when a form grows to ~10 fields
with cross-field validation rules, switch.

---

## 6. Flash messaging deep-dive

Flash messages are short, user-meaningful, **one-shot** UI
notifications that appear on the next page render. The pattern:

1. Handler completes a state change (success or recoverable
   failure)
2. Calls `middleware.AddFlash(c, severity, text)` — writes to
   the session
3. Redirects to a new page (PRG)
4. The next request's `Console.baseData(c)` calls
   `middleware.ReadFlashes(c)` which returns + clears them
5. `partials/flash.tmpl` renders them at the top of the page
6. Refresh shows no flash (already consumed)

### 6.1. Severity levels

| Severity | Use for | CSS color |
| --- | --- | --- |
| `success` | A state change completed | green |
| `info` | A neutral event (logged out, job queued) | blue |
| `warning` | A partial success or recoverable degradation | yellow |
| `danger` | A failure the user should retry | red |

### 6.2. Multiple flashes per request

Supported: each `AddFlash` appends. The partial loops over them.
Useful for bulk actions: "Quarantined 10. Failed: 2 (see audit
log)."

### 6.3. Cross-tab flash leakage

The flash lives in the user's session, which is shared across
tabs. Adding a flash in tab A and refreshing tab B will show
it in B (and clear it). This is *almost always desirable* —
users expect "I did the thing" to follow them around.

The exception: flashes from one user's session never leak to
another's (sessions are per-cookie-per-browser). The shared-
computer "logged out by my colleague" gotcha doesn't apply.

### 6.4. What goes in `Text`

Plain text. **Not HTML.** The partial does not call `safehtml`
or `template.HTML` on it; it's rendered as a literal string. If
we ever want a link inside a flash, add a `LinkURL` and
`LinkText` field to `FlashMessage` rather than allowing HTML —
keeps the threat surface tiny.

---

## 7. The bootstrap admin user story

Day-1 deployment needs a way for the first admin to log in.
Both scenarios are handled **inside the existing binary** with
no new CLI subcommands — the `cmd/pkgmirror/` server binary is
the only entry point today and adding a subcommand framework
is out of scope for the console work.

### 7.1. Mode=proxy-header

The first user to reach `/console/` with a trusted-source request
+ the configured user header set:

1. Triggers `Users.GetOrCreate` — a `users` row is auto-created
   (only when the source IP matches `TRUSTED_PROXIES`)
2. Has **no admin role** by default — sees only their own
   profile + tenants they're already members of

First-admin promotion via env var:

- `PKGMIRROR_CONSOLE_BOOTSTRAP_ADMIN=alice@example.com`
- On every login, if the env var is set AND the resolved user's
  name/email matches AND the user is not already an admin: set
  `users.is_admin = true`, write an `audit.actor.promote` row
  with `severity=warn` and `reason=bootstrap_env_var`, log
  loudly ("BOOTSTRAP ADMIN PROMOTION: alice@example.com…")
- After the first promotion, subsequent matching logins are
  no-ops (the env var is convergent, not repeated)
- Operators are expected to *unset* the env var after the first
  promotion succeeds; the docs say so but we don't enforce it,
  since the no-op behavior makes leaving it set harmless

Group-to-role mapping from `X-Forwarded-Groups` is **deferred to
v2** (web-console.md §11). The first admin is the only special
case; subsequent admin grants happen through the regular console
flow.

### 7.2. Mode=password

The bootstrap admin token (`PKGMIRROR_ADMIN_TOKEN`) already
creates a `users` row for the admin. That user has no password
set.

**Token-verified set-password flow** (entirely in the console,
no CLI):

1. Admin loads `/console/login` for the first time
2. Submits username (no password yet) + their PAT in a single
   form field labeled "Personal Access Token (one time)"
3. The handler:
   - Looks up the user by name
   - Verifies the supplied PAT belongs to that user via
     `tokens.Store.Lookup` (the same path the registry uses)
   - If both match: shows the set-password form
   - If anything fails: same rate-limited generic
     authentication-failed page (don't leak whether the user
     exists or the token is wrong)
4. Admin sets a password; `users.Store.SetPasswordHash` writes
   the argon2id-encoded value; session cookie issued; redirect
   to `/console/`
5. Next login is the normal username/password flow; the PAT is
   no longer required

Same flow works as a self-service password reset (forgot-my-
password): the user still has their PAT issued at account
creation, that's the recovery secret. If they lost both the
password and the PAT, a system admin clears the password via
the console's user-management page (PR 8) and the user starts
over with a fresh PAT from a system admin.

This means we never need a `pkgmirror admin set-password` CLI
subcommand. The token-as-bootstrap-secret pattern is enough.

### 7.3. The README documents both

`README.md` "Using the console" gets a one-paragraph block per
mode showing the exact first-login dance.

---

## 8. Configuration conventions

All env vars are `PKGMIRROR_*`-prefixed. Console-specific ones
are `PKGMIRROR_CONSOLE_*`. The full v1 list:

| Var | Default | Notes |
| --- | --- | --- |
| `PKGMIRROR_CONSOLE_ENABLED` | `true` | Wires/strips the `/console` route group |
| `PKGMIRROR_CONSOLE_AUTH_MODE` | `password` | `proxy-header` or `password` |
| `PKGMIRROR_CONSOLE_AUTH_USER_HEADER` | `X-Forwarded-User` | proxy-header mode only |
| `PKGMIRROR_CONSOLE_AUTH_EMAIL_HEADER` | `X-Forwarded-Email` | proxy-header mode only; optional |
| `PKGMIRROR_CONSOLE_AUTH_GROUPS_HEADER` | `X-Forwarded-Groups` | proxy-header mode only; optional; v2 honors |
| `PKGMIRROR_CONSOLE_TRUSTED_PROXIES` | _(empty)_ | proxy-header mode: comma-separated IP/CIDR allowlist. **Required** in proxy-header mode; boot fails loud if empty |
| `PKGMIRROR_CONSOLE_BOOTSTRAP_ADMIN` | _(empty)_ | proxy-header mode: username/email auto-promoted to admin on first matching login. Convergent: re-running after promotion is a no-op |
| `PKGMIRROR_SESSION_AUTH_KEY` | _(generated, dev only)_ | Hex-encoded 64-byte key (128 hex chars). Required in production password mode; see below |
| `PKGMIRROR_SESSION_ENC_KEY` | _(generated, dev only)_ | Hex-encoded 32-byte key (64 hex chars). Required in production password mode |
| `PKGMIRROR_CSRF_KEY` | _(generated, dev only)_ | Hex-encoded 32-byte key (64 hex chars). Required in production. CSRF applies in both auth modes |
| `PKGMIRROR_CONSOLE_ALLOW_EPHEMERAL_KEYS` | `false` | Production override allowing missing keys (generates on boot + logs). Dev fills this in automatically |
| `PKGMIRROR_SESSION_TTL` | `24h` | Go duration string |
| `PKGMIRROR_CONSOLE_DEV_DIR` | _(empty)_ | Dev only: path to load templates + static from disk instead of embed |
| `PKGMIRROR_CONSOLE_DARK_MODE_DEFAULT` | `auto` | `auto` (follow OS), `light`, `dark` |

**Keys are hex-encoded.** Config loader decodes once and
validates byte length (see §2.2 session middleware). Mixing
"64 hex chars" with "64 bytes" was a bug in an earlier draft.

**Boot behavior for missing keys:**

- **Dev** (gin in debug mode OR `PKGMIRROR_CONSOLE_DEV_DIR` set):
  generate ephemeral keys, print them, log a warning. Sessions
  + CSRF tokens invalidate on every restart — fine for dev.
- **Production** (gin release mode + no dev dir): require
  explicit keys. Boot fails with a clear error pointing at
  `make gen-keys`. Setting
  `PKGMIRROR_CONSOLE_ALLOW_EPHEMERAL_KEYS=true` overrides this
  for operators who genuinely want the dev behavior in prod.

A `make gen-keys` target prints fresh values:

```makefile
gen-keys:
	@printf 'PKGMIRROR_SESSION_AUTH_KEY=%s\n' "$$(openssl rand -hex 64)"
	@printf 'PKGMIRROR_SESSION_ENC_KEY=%s\n'  "$$(openssl rand -hex 32)"
	@printf 'PKGMIRROR_CSRF_KEY=%s\n'         "$$(openssl rand -hex 32)"
```

Values are 128 / 64 / 64 hex chars respectively, decoding to
64 / 32 / 32 raw bytes — matching the byte-length validators.

### 8.1. Config loading shape

`internal/console/config.go` follows the same pattern as
`internal/config/config.go` — one struct, one `LoadFromEnv()`,
one `Validate()` that returns clear errors on bad input
(`PKGMIRROR_CONSOLE_AUTH_MODE=floob` → "must be 'password' or
'proxy-header'").

---

## 9. Middleware order

The exact order matters; document it once and keep it stable.
Registered in `console.go`'s `Register`:

```go
r := gin.New()

// 1. Recovery — must be first so panics in any later middleware are caught
r.Use(middleware.Recover(c.Logger))

// 2. Request ID — generates and stashes; later middleware logs it
r.Use(middleware.RequestID())

// 3. Access logger — emits one Info line per request
r.Use(middleware.AccessLog(c.Logger))

// 4. Session middleware (cookie store) — must come before CSRF and auth
sess, _ := middleware.NewSessionMiddleware(c.Config)
r.Use(sess)

// 5. CSRF — must come after session (uses the session for nonce storage)
r.Use(middleware.CSRF(c.Config.CSRFKey, c.Config.UsingTLS))

// 6. Authentication — populates ctx with *auth.Identity
r.Use(auth.Middleware(c.Authenticator))

// 7. Per-route gates
g := r.Group("/console", c.Middleware.RequireAuth())
admin := g.Group("/admin", c.Middleware.RequireSystemAdmin())
t := g.Group("/t/:tenant", c.Middleware.RequireTenantMember())
```

Static assets mount before the console group (no auth, no CSRF
needed):

```go
r.StaticFS("/static", http.FS(staticFS))
```

---

## 10. The first PR's vertical slice

PR 1 (foundation, after PR 0a/0b land) ships **only**:

- `console.go` + `embed.go` + `types.go` + `templates.go`
- `middleware/recover.go` + `requestid.go` + `accesslog.go` + `session.go` + `csrf.go` + `securityheaders.go`
- `layouts/base.tmpl` + `layouts/auth.tmpl`
- `partials/flash.tmpl` + `sidebar.tmpl` + `breadcrumb.tmpl`
- `partials/ui/{button,card,alert}.tmpl` (start with three)
- `static/css/{input.css,console.css}` + `tailwind.config.js`
- `static/js/console.js` + `static/favicon.ico`
- `tools/tailwindcss/install.sh` — pins the standalone CLI
  version + verifies the binary's SHA-256 + maps Darwin/Linux
  amd64/arm64; vendors to `$(BIN_DIR)/tailwindcss`
- Makefile `build-css` / `watch-css` / `gen-keys` targets
- `cmd/pkgmirror/main.go` wiring (read console config, construct
  Console, call `Register` if enabled — only after PR 0b's
  route-scoped middleware restructure)
- A single `GET /console/_ping` that renders a minimal page
  using the layout + a sample flash + sample UI partial

Acceptance for PR 1:

- `make build-css && make build && ./bin/pkgmirror` boots
- `curl http://localhost:8080/console/_ping` returns HTML with
  the layout chrome and the security headers from
  `securityheaders.go` (Content-Security-Policy, X-Content-Type-
  Options, X-Frame-Options, Referrer-Policy; HSTS when TLS)
- Setting `PKGMIRROR_CONSOLE_ENABLED=false` cleanly removes the
  route
- Setting `PKGMIRROR_CONSOLE_DEV_DIR=internal/console` and
  editing a template reflects on refresh without rebuild
- `make gen-keys` prints values that boot accepts as keys
- `make build` (no Tailwind installed on the build host) still
  succeeds because the committed `console.css` is what gets
  embedded — CI's unit-test job doesn't need Tailwind
- `tools/tailwindcss/install.sh` exits non-zero if the
  downloaded binary's SHA-256 doesn't match the pinned value

Once PR 1 lands, PRs 2a/2b/2c are the security-sensitive
foundation; PRs 3–8 are all "more of the same."

---

## 11. Testing conventions

Per page package, a `<page>_test.go` covers four cases at
minimum:

| Case | Asserts |
| --- | --- |
| **Happy path GET** | 200 status; expected heading or distinguishing string in body |
| **Anonymous request** | 303 to `/console/login` (mode=password) or 401 (mode=proxy-header) |
| **Wrong role / wrong tenant** (data-plane pages only) | 403 |
| **CSRF reject** (forms only) | 403 + friendly page on POST without token |

For pages with forms, two more:

| Case | Asserts |
| --- | --- |
| **Validation failure** | 200 + form re-rendered + Errors map populated |
| **Success → redirect** | 303 + Location header + flash message visible on follow-up GET |

The fixture builder (§2.7) makes each test ~10 lines.

Both auth modes get covered: tests run with mode=password
(default for tests), plus a per-page `_proxy_test.go` that
exercises mode=proxy-header.

### 11.1. Test fixture must handle the macOS APFS SQLite-cleanup race

Per [.github/copilot-instructions.md](../.github/copilot-instructions.md),
every grey-box fixture closes the DB and explicitly
`os.RemoveAll`s the temp dir in `t.Cleanup`. The console
`testutil.New` follows the existing pattern:

```go
t.Cleanup(func() {
    _ = db.Close()
    _ = os.RemoveAll(dir)  // load-bearing on macOS APFS
})
```

### 11.2. PR 2 (a/b/c) threat-model test matrix

The auth PRs warrant a dedicated security test matrix, separate
from the per-page tests. Each row is its own table-driven test
in `internal/console/middleware/auth_test.go` (and the related
rate-limit + CSRF tests):

| Scenario | Expected |
| --- | --- |
| **proxy-header**: trusted source + user header → identity | resolves; user auto-created if new |
| **proxy-header**: untrusted source + user header → anonymous | header IGNORED; no user creation |
| **proxy-header**: trusted source + spoofed `X-Forwarded-For` header from untrusted upstream | the spoof DOESN'T promote the source to trusted (use the immediate remote addr, not the forwarded chain, unless explicitly configured) |
| **proxy-header**: empty `TRUSTED_PROXIES` at boot | boot FAILS LOUD with a clear error |
| **proxy-header**: bootstrap admin env var matches + user not admin | promote + audit row + log |
| **proxy-header**: bootstrap admin env var matches + user already admin | no-op; no duplicate audit row |
| **password**: login attempt 1-10 within 60s | accepted (or denied if creds wrong) |
| **password**: login attempt 11 within 60s | 429 Too Many Requests; rate limit fires |
| **password**: rate limit uses centralized client-IP resolver | spoofed `X-Forwarded-For` does NOT bypass rate limit unless from a trusted proxy |
| **password**: valid login | session cookie set; redirect to `return_to` or `/console/` |
| **password**: invalid login | inline error on form; same wording for both "user doesn't exist" and "wrong password" (don't leak which) |
| **password**: token-verified set-password flow | accepts user+PAT, sets hash, issues session |
| **password**: session expired | redirect to `/console/login`; `return_to` stashed |
| **CSRF**: POST without token | 403 + friendly page |
| **CSRF**: POST with stale token | 403 + friendly page |
| **session auth**: DB error during user lookup | request fails with 500 (fail closed) |
| **proxy-header auth**: DB error during GetOrCreate | request fails with 500 (fail closed) |

The centralized client-IP resolver (`console.ClientIP(r,
trusted)`) gets its own table-driven test for trusted-proxy
hop walking.

---

## 12. What lands in DECISIONS.md

Appended at the end of PR 1 (or PR 2 if implementing both at
once). Template:

```markdown
## 2026-NN-NN — Web console (initial vertical slice)

**Decision:** Ship a server-rendered admin console at /console.
See [plans/web-console.md] for architecture and
[plans/web-console-implementation-plan.md] for conventions.

**Resolved choices** (verbatim from web-console.md §10):

- Templating: html/template, no templ, no HTMX
- Styling: Tailwind via standalone CLI + copilot-api theme tokens
- Auth: dual-mode (proxy-header + password), configurable
- CSRF: gorilla/csrf
- Sessions: gorilla/sessions CookieStore (no SQLite table)
- Listener: same listener as registry (route-group separation)
- Scope: admin-only for MVP

**Cross-cutting contract changes** (PRs 0a and 0b):

- `auth.Identity` grew a `CredentialKind` field;
  `IsSystemAdmin` / `CanRead` / `CanWrite` switch on Kind so
  session/proxy identities (Token=nil) authorize correctly
  without weakening token-auth scopes for registry/admin APIs
- `internal/server/server.go` lifted `auth.Middleware` from
  global to route-scoped; registry + admin REST keep
  TokenAuthenticator; /console mounts its own authenticator

**Schema migration (v3 → v4):**

- `users.password_hash` + `users.password_set_unix` (both
  nullable; idempotent; proxy-header-only deployments never
  touch them)
- `audit_log.actor_kind` (TEXT: 'token' / 'session' / 'proxy')
  so audit queries can filter by credential source

**New direct deps:**

- `github.com/gorilla/csrf` (CSRF middleware)
- `github.com/gorilla/sessions` (transitive through gin-contrib)
- `github.com/gin-contrib/sessions` (gin wrapper)
- `golang.org/x/crypto/argon2` (was indirect; now direct for password hashing)

**Trade-offs accepted:**

- Tailwind CSS file churns in git on every template change
- Visual match to copilot-api is approximate, not pixel-for-pixel
- Session size capped at ~4 KB (cookie limit); fine at our scale
- Production password mode requires explicit session/CSRF keys
  (or an explicit `PKGMIRROR_CONSOLE_ALLOW_EPHEMERAL_KEYS=true`)
- Raw package delete deferred to v2; only quarantine workflow
  in v1
- Group-to-role mapping from proxy headers deferred to v2
```

Subsequent PRs append their own short entries only for *new*
decisions (e.g. "added validator/v10 because rules form grew
past 10 fields" — when/if).

---

## 13. What this plan deliberately does NOT specify

These are decided during implementation by the engineer doing
the work. Listed here so reviewers don't ask:

- Exact CSS class lists in templates (use whatever the
  partial-extraction rule pulls out)
- Exact wording of flash messages (terse + actionable; second
  person)
- Exact pagination defaults (start with 50/page, adjust if any
  list feels wrong)
- Exact slog output format (stdlib JSON handler is fine; can
  switch to text in dev via env)
- Whether `template.FuncMap` lives in `templates.go` or its own
  file — both are fine
- Exact 404/403/500 page wording (apologetic + actionable)

If any of these turn into a debate during a PR review, the
reviewer's preference wins; this plan does not have an opinion.

### 13.1. Rules this plan DOES enforce

The positive complement to §13's negatives:

- **No silent errors in auth code.** Both `Authenticator`
  implementations (§2.2) return real errors on DB or session
  corruption; only missing credentials become anonymous. Console
  auth fails closed.
- **All destructive actions use POST**, not DELETE (HTML form
  constraint).
- **All state-changing actions produce an audit row** per the
  table in §0.6. Logging is separate from audit.
- **POST-Redirect-GET is mandatory** for successful state
  changes (§5.1).
- **CSRF tokens on every form** via `{{ .Base.CSRFField }}`
  (§5.3).
- **Hex-encoded keys in env vars; byte-length validated after
  decode** (§2.2 session, §8).
- **No `{{ define "page-js" }}` blocks** — flat template
  namespace; collisions silently overwrite (§2.3).
- **No template func receives the request.** CSRF + identity
  go via `BaseData` (§2.1 types, §2.2 csrf).
- **Test fixtures close the DB then `os.RemoveAll`** the temp
  dir in `t.Cleanup` (§11.1).
