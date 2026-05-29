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

---

## 1. Phased PR sequence

The §9 effort table in web-console.md lists 12 steps totaling
~4 days. They're best landed as **eight independently
shippable PRs**, in this order. Each PR ends with the stress
loop green and is reviewable on its own.

| # | PR | What lands | Estimate |
| --- | --- | --- | --- |
| 1 | **foundation** | `Console` struct, embed.FS loader, layout + base CSS, `/console/_ping`, dev-dir override | 1.5 days |
| 2 | **auth** | both modes (proxy-header + password), session, CSRF, login/logout, schema migration, rate limit | 1 day |
| 3 | **dashboard** | `pages/home/` — first real page, read-only, exercises the layout + partials | 0.5 day |
| 4 | **tenants** | `pages/tenants/` — list, detail, members. First page with state-changing forms + flash messages | 0.5 day |
| 5 | **packages** | `pages/packages/` — per-tenant list + version detail + delete. First data-plane page | 0.5 day |
| 6 | **audit** | `pages/audit/` — query + detail. Read-only but exercises pagination + filtering | 0.5 day |
| 7 | **rules** | `pages/rules/` — list, edit, dry-run preview. Most complex page; biggest form | 1 day |
| 8 | **tokens + profile** | `pages/tokens/` + `pages/profile/` — mint/revoke + own-profile management | 0.5 day |

PR 1 is the foundation; PR 2 unblocks all subsequent pages.
PRs 3–8 can ship in any order after PR 2.

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
// Sketch — full implementation in PR 1
func (c *Console) Render(gc *gin.Context, page string, body any) {
    if !c.Config.Enabled {
        gc.String(http.StatusNotFound, "console disabled")
        return
    }
    wrapper := struct {
        Page string
        Base BaseData
        Body any
    }{
        Page: page,
        Base: c.baseData(gc),
        Body: body,
    }
    gc.HTML(http.StatusOK, "layouts/base", wrapper)
}
```

The layout template references `.Page` to pick which page
template to embed:

```html
{{ define "layouts/base" }}<!doctype html>
<html><head>...</head><body>
  <nav>...</nav>
  <main>
    {{ template "partials/flash" .Base.Flash }}
    {{ if eq .Page "pages/tenants/list" }}{{ template "pages/tenants/list" .Body }}
    {{ else if eq .Page "pages/tenants/detail" }}{{ template "pages/tenants/detail" .Body }}
    {{ /* ... or use a registry-of-template-names ... */ }}
    {{ end }}
  </main>
  {{ block "page-js" . }}{{ end }}
</body></html>{{ end }}
```

Yes, the giant `if/else` is awkward. We can clean it up with a
template func: `{{ tmpl .Page .Body }}` where `tmpl` looks up
and executes the named template. Add the func when the page
count grows past ~5.

#### ★ `internal/console/templates.go`

The recursive `embed.FS` walker described in web-console.md §4.5.
~50 lines. Plus the template func map:

```go
var funcs = template.FuncMap{
    "csrfField":   csrfFieldFunc,    // emitted by middleware/csrf.go
    "default":     defaultFunc,      // {{ .X | default "fallback" }}
    "dict":        dictFunc,         // {{ template "x" (dict "k" "v") }}
    "humanBytes":  humanBytesFunc,
    "humanTime":   humanTimeFunc,    // relative time ago
    "timefmt":     timefmtFunc,
    "lower":       strings.ToLower,
    "upper":       strings.ToUpper,
    "title":       titleFunc,
    "join":        strings.Join,
    "tmpl":        tmplFunc,         // {{ tmpl "pages/tenants/list" .Body }}
}
```

The `dict` func is the only non-obvious one and it's the one
that makes UI partials usable from page templates:

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
    Identity    *auth.Identity   // nil for anonymous
    Memberships []*tenants.Tenant // tenants the user can access
    ActiveTenant *tenants.Tenant  // nil unless we're under /console/t/:tenant
    Flash       []FlashMessage    // see §6
    CSRFField   template.HTML     // pre-rendered <input type="hidden" name="gorilla.csrf.Token" ...>
    AppVersion  string
    ConsolePath string             // e.g. "/console" — for href-building
    Now         time.Time
}

type FlashMessage struct {
    Severity string  // "info" | "success" | "warning" | "danger"
    Text     string
}
```

### 2.2. Middleware (PR 1 foundation + PR 2 auth)

#### ★ `internal/console/middleware/auth.go`

Two `Authenticator` implementations + the gate helpers. Both
modes here, mode chosen at boot via `Console.Config.AuthMode`.
~200 lines.

```go
// Mode selector: returned by Console.New() based on config.
func NewAuthenticator(cfg Config, users *users.Store, tenants *tenants.Store) (auth.Authenticator, error) {
    switch cfg.AuthMode {
    case "proxy-header":
        return newProxyHeader(cfg, users, tenants), nil
    case "password":
        return newSession(cfg, users, tenants), nil
    default:
        return nil, fmt.Errorf("unknown PKGMIRROR_CONSOLE_AUTH_MODE=%q", cfg.AuthMode)
    }
}
```

Gates registered as gin middleware (called from console.go's
Register):

```go
func (m *Middleware) RequireAuth() gin.HandlerFunc {
    return func(c *gin.Context) {
        id := auth.FromContext(c)
        if id == nil {
            if m.AuthMode == "password" {
                // PRG pattern: stash return-to, redirect to login
                m.sessions.Get(c).Set("return_to", c.Request.URL.RequestURI())
                _ = m.sessions.Get(c).Save()
                c.Redirect(http.StatusSeeOther, "/console/login")
            } else {
                c.String(http.StatusUnauthorized, "unauthorized")
            }
            c.Abort()
            return
        }
        c.Next()
    }
}

func (m *Middleware) RequireSystemAdmin() gin.HandlerFunc { /* ... */ }
func (m *Middleware) RequireTenantMember() gin.HandlerFunc { /* checks :tenant param against id.Memberships */ }
func (m *Middleware) RequireTenantAdmin() gin.HandlerFunc { /* same but role=admin */ }
```

#### ★ `internal/console/middleware/csrf.go`

Wraps gorilla/csrf as a gin middleware. ~40 lines.

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

The `csrfField` template func reads from the request and emits
the hidden input — wired in `console.go`'s funcs map:

```go
func csrfFieldFunc(c *gin.Context) template.HTML {
    return csrf.TemplateField(c.Request)
}
```

#### ○ `internal/console/middleware/session.go`

Direct gin-contrib/sessions wiring. ~30 lines. Cookie-only store
(no SQLite session table per web-console.md §5.4).

```go
func NewSessionMiddleware(cfg Config) (gin.HandlerFunc, error) {
    if len(cfg.SessionAuthKey) != 64 || len(cfg.SessionEncKey) != 32 {
        return nil, fmt.Errorf("session keys: need 64 byte auth + 32 byte enc")
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
  <link rel="stylesheet" href="/static/css/console.css?v={{ .Base.AppVersion }}">
  <link rel="icon" href="/static/favicon.ico">
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
  <script src="/static/js/console.js?v={{ .Base.AppVersion }}" defer></script>
  {{ block "page-js" . }}{{ end }}
</body>
</html>{{ end }}
```

The `tmpl` func is the template-name dispatcher; alternative is
the giant `if/else` shown above.

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
   shadcn/ui-derived color variables; MIT-compatible. */
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
JS goes in `{{ define "page-js" }}` blocks in individual page
templates (web-console.md §4.9).

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
  {{ csrfField }}
  {{ template "ui/formfield" (dict
        "label" "Name"
        "name" "name"
        "value" .Name
        "error" (index .Errors "name")
        "required" true) }}
  {{ template "ui/formfield" (dict
        "label" "Visibility"
        "name" "visibility"
        "value" .Visibility
        "error" (index .Errors "visibility")
        "type" "select"
        "options" (slice "public" "private")) }}
  <button type="submit">Create</button>
</form>
{{ end }}
```

The `ui/formfield` partial handles the error display + the
input population uniformly. Five lines per field is the bar.

### 5.3. CSRF tokens

Every form: `{{ csrfField }}` at the top. The template func is
already wired (§2.2). Forgetting it means the POST 403s and the
user sees the friendly CSRF error page — fail loud.

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
Two scenarios:

### 7.1. Mode=proxy-header

The first user to reach `/console/` with a trusted-source request
+ the configured user header set:

1. Triggers `Users.GetOrCreate` — a `users` row is auto-created
2. Has **no admin role** by default — sees only their own
   profile + tenants they're already members of

So how does the first admin get admin? One of:

- Bootstrap admin grant: an env var
  `PKGMIRROR_CONSOLE_BOOTSTRAP_ADMIN=alice@example.com` that,
  if set, auto-promotes the named user the first time they log
  in (one-shot; logged loudly)
- Out-of-band: an operator runs
  `pkgmirror admin promote alice@example.com` (the existing CLI)

The env-var path is simpler for an initial deploy; the CLI is
the right answer afterwards.

### 7.2. Mode=password

The bootstrap admin token (the existing `PKGMIRROR_ADMIN_TOKEN`
mechanism) already creates a `users` row for the admin. That
user can `pkgmirror admin set-password <user>` from the CLI to
set their initial password, then log into the console.

A friendlier flow for "no password set yet" users: when an
existing-but-passwordless user hits `/console/login`, they get
a "Set a password first" page that requires their PAT for
verification, then sets the password. Same wire as a normal
password reset, just bootstrapped by token instead of email.

### 7.3. The README documents both

`README.md` "Using the console" gets a one-paragraph block per
mode showing the exact first-login dance.

---

## 8. Configuration conventions

All env vars are `PKGMIRROR_*`-prefixed. Console-specific ones
are `PKGMIRROR_CONSOLE_*`. The full v1 list:

| Var | Default | Notes |
| --- | --- | --- |
| `PKGMIRROR_CONSOLE_ENABLED` | `true` | Wires/strips the `/console` + `/static` route groups |
| `PKGMIRROR_CONSOLE_AUTH_MODE` | `password` | `proxy-header` or `password` |
| `PKGMIRROR_CONSOLE_AUTH_USER_HEADER` | `X-Forwarded-User` | proxy-header mode only |
| `PKGMIRROR_CONSOLE_AUTH_EMAIL_HEADER` | `X-Forwarded-Email` | proxy-header mode only; optional |
| `PKGMIRROR_CONSOLE_AUTH_GROUPS_HEADER` | `X-Forwarded-Groups` | proxy-header mode only; optional |
| `PKGMIRROR_CONSOLE_TRUSTED_PROXIES` | _(empty)_ | proxy-header mode: comma-separated IP/CIDR allowlist. **Required** in proxy-header mode; boot fails loud if empty |
| `PKGMIRROR_CONSOLE_BOOTSTRAP_ADMIN` | _(empty)_ | proxy-header mode: username/email that gets auto-promoted to admin on first login |
| `PKGMIRROR_SESSION_AUTH_KEY` | _(generated)_ | 64 bytes hex; if unset, generated and printed once on boot |
| `PKGMIRROR_SESSION_ENC_KEY` | _(generated)_ | 32 bytes hex; if unset, generated and printed once on boot |
| `PKGMIRROR_CSRF_KEY` | _(generated)_ | 32 bytes hex; if unset, generated and printed once on boot |
| `PKGMIRROR_SESSION_TTL` | `24h` | Go duration string |
| `PKGMIRROR_CONSOLE_DEV_DIR` | _(empty)_ | Dev only: path to load templates + static from disk instead of embed |
| `PKGMIRROR_CONSOLE_DARK_MODE_DEFAULT` | `auto` | `auto` (follow OS), `light`, `dark` |

**Generated keys printed once on boot:** matches the existing
`PKGMIRROR_ADMIN_TOKEN` pattern. Operators must capture them on
first boot and set them as env vars for restart-stability.

A `make gen-keys` target prints fresh values to copy:

```makefile
gen-keys:
	@printf 'PKGMIRROR_SESSION_AUTH_KEY=%s\n' "$$(openssl rand -hex 64)"
	@printf 'PKGMIRROR_SESSION_ENC_KEY=%s\n'  "$$(openssl rand -hex 32)"
	@printf 'PKGMIRROR_CSRF_KEY=%s\n'         "$$(openssl rand -hex 32)"
```

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

PR 1 ships **only**:

- `console.go` + `embed.go` + `types.go` + `templates.go`
- `middleware/recover.go` + `requestid.go` + `accesslog.go` + `session.go` + `csrf.go`
- `layouts/base.tmpl` + `layouts/auth.tmpl`
- `partials/flash.tmpl` + `sidebar.tmpl` + `breadcrumb.tmpl`
- `partials/ui/{button,card,alert}.tmpl` (start with three)
- `static/css/{input.css,console.css}` + `tailwind.config.js`
- `static/js/console.js` + `static/favicon.ico`
- `tools/tailwindcss/install.sh` + Makefile `build-css` / `watch-css` / `gen-keys`
- `cmd/pkgmirror/main.go` wiring (read config, construct Console, call Register if enabled)
- A single `GET /console/_ping` that renders a minimal page
  using the layout + a sample flash + sample UI partial

Acceptance for PR 1:

- `make build-css && go build && ./bin/pkgmirror` boots
- `curl http://localhost:8080/console/_ping` returns HTML with
  the layout chrome
- Setting `PKGMIRROR_CONSOLE_ENABLED=false` cleanly removes the
  route
- Setting `PKGMIRROR_CONSOLE_DEV_DIR=internal/console` and
  editing a template reflects on refresh without rebuild
- `make gen-keys` prints valid keys

Once PR 1 lands, PRs 2–8 are all "more of the same."

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
- Auth: dual mode (proxy-header + password), configurable
- CSRF: gorilla/csrf
- Sessions: gorilla/sessions CookieStore (no SQLite table)
- Listener: same listener as registry (route-group separation)
- Scope: admin-only for MVP

**Trade-offs accepted:**

- Tailwind CSS file churns in git on every template change
- Visual match to copilot-api is approximate, not pixel-for-pixel
- Session size capped at ~4 KB (cookie limit); fine at our scale

**Schema additions:** `password_hash` + `password_set_unix` columns
on `users`. Nullable. Idempotent migration.
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
