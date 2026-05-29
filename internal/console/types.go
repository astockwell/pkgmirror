package console

import (
	"html/template"
	"time"

	"github.com/astockwell/pkgmirror/internal/auth"
	"github.com/astockwell/pkgmirror/internal/console/middleware"
	"github.com/astockwell/pkgmirror/internal/tenants"
)

// BaseData is the per-request page chrome populated by Console.baseData.
// Every page template receives it as `.Base`.
type BaseData struct {
	Identity     *auth.Identity            // nil for anonymous requests
	Memberships  []*tenants.Tenant         // tenants the user can read
	ActiveTenant *tenants.Tenant           // non-nil under /console/t/:tenant
	Flash        []middleware.FlashMessage // popped from session this request
	CSRFField    template.HTML             // pre-rendered <input type="hidden" ...>
	ScriptURLs   []string                  // per-page external JS, iterated in the layout
	AppVersion   string                    // for cache-busting static asset URLs
	ConsolePath  string                    // e.g. "/console" — for href-building
	DarkMode     bool                      // resolved per-request from cookie/config
	PageTitle    string                    // set by handler before Render
	Now          time.Time
}

// pageWrapper is what gets executed against the layout template.
// `.Page` selects the page template via the layout's `{{ tmpl .Page .Body }}`
// dispatcher; `.Body` is the page-specific data struct.
type pageWrapper struct {
	Page string
	Base BaseData
	Body any
}

// errorPageData renders pages/_error.
type errorPageData struct {
	Verb      string
	RequestID string
}

// notFoundPageData renders pages/_notfound.
type notFoundPageData struct {
	Message string
}

// forbiddenPageData renders pages/_forbidden.
type forbiddenPageData struct {
	Message string
}

// pingPageData renders pages/_ping (the PR 1 proof-of-life page).
type pingPageData struct {
	Title   string
	Message string
}
