package console

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/astockwell/pkgmirror/internal/auth"
	"github.com/astockwell/pkgmirror/internal/console/middleware"
	"github.com/astockwell/pkgmirror/internal/upstream"

	"github.com/gin-gonic/gin"
)

// ---- view: /console/tenants/:name/upstreams ----

// upstreamsPageData carries the per-format admin view. One row per
// format that has a compiled-in default (10 today; generic + container
// are excluded). Source = "default" when the tenant has no row; "custom"
// when the override is present.
type upstreamsPageData struct {
	TenantName string
	Rows       []upstreamRow
	Errors     map[string]string // keyed by format; populated on POST validation failure
	Edited     string            // format that was just edited (highlights the row)
}

type upstreamRow struct {
	Format            string
	Mode              string
	UpstreamURL       string
	MetadataTTLSec    int64
	DefaultURL        string
	DefaultHosts      []string
	HasOverride       bool
	AdapterShipped    bool // false until the per-format PR lands
	UpdatedUnix       int64
}

// tenantUpstreams renders the read+edit page for tenant_upstreams. One
// row per compiled-in format; admins can edit mode/URL/TTL inline.
func (c *Console) tenantUpstreams(gc *gin.Context) {
	ctx := gc.Request.Context()

	t, ok := c.resolveTenantFromPath(gc)
	if !ok {
		return
	}

	persisted, err := c.upstreams.ListByTenant(ctx, t.ID)
	if err != nil {
		c.RenderError(gc, "list upstream config", err)
		return
	}

	rows := buildUpstreamRows(persisted)
	c.Render(gc, "pages/tenants/upstreams", upstreamsPageData{
		TenantName: t.Name,
		Rows:       rows,
		Errors:     map[string]string{},
	})
}

// buildUpstreamRows merges the persisted overrides with the compiled-in
// defaults so the template gets one row per format regardless of
// whether the operator has touched it yet. Sorted by format name.
func buildUpstreamRows(persisted map[string]*upstream.PersistedConfig) []upstreamRow {
	defaults := upstream.AllDefaults()
	out := make([]upstreamRow, 0, len(defaults))
	for format, def := range defaults {
		row := upstreamRow{
			Format:         format,
			Mode:           string(upstream.ModeCacheAndServe),
			DefaultURL:     def.URL,
			DefaultHosts:   def.Hosts,
			AdapterShipped: def.PullThroughSupported,
		}
		if !def.PullThroughSupported {
			// No adapter yet -> the resolver clamps to off regardless
			// of the row. Surface that in the UI so operators aren't
			// confused when they set cache_and_serve and nothing happens.
			row.Mode = string(upstream.ModeOff)
		}
		if p, ok := persisted[format]; ok {
			row.HasOverride = true
			row.Mode = p.Mode
			if p.UpstreamURL.Valid {
				row.UpstreamURL = p.UpstreamURL.String
			}
			row.MetadataTTLSec = p.MetadataTTLSec
			row.UpdatedUnix = p.UpdatedUnix
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Format < out[j].Format })
	return out
}

// ---- mutation: POST /console/tenants/:name/upstreams/:format ----

// tenantUpstreamUpsert validates the form fields and persists. Errors
// re-render the page with the row highlighted. Successful updates
// redirect (PRG) so refresh doesn't replay.
func (c *Console) tenantUpstreamUpsert(gc *gin.Context) {
	ctx := gc.Request.Context()
	id := auth.FromContext(gc)

	t, ok := c.resolveTenantFromPath(gc)
	if !ok {
		return
	}
	format := strings.ToLower(strings.TrimSpace(gc.Param("format")))
	if format == "" {
		c.RenderNotFound(gc, "format required")
		return
	}
	def, hasDefault := upstream.LookupDefault(format)
	if !hasDefault {
		c.RenderNotFound(gc, "format %q has no upstream", format)
		return
	}

	mode := strings.TrimSpace(gc.PostForm("mode"))
	urlStr := strings.TrimSpace(gc.PostForm("upstream_url"))
	ttlStr := strings.TrimSpace(gc.PostForm("metadata_ttl_sec"))

	if !upstream.Mode(mode).Valid() {
		c.reRenderUpstreams(gc, t.Name, t.ID, format, "Mode must be off, cache_and_serve, or cache_only.")
		return
	}

	// Empty URL means "use the compiled-in default" - persist NULL.
	if urlStr != "" {
		u, err := url.Parse(urlStr)
		if err != nil || u.Scheme == "" || u.Host == "" {
			c.reRenderUpstreams(gc, t.Name, t.ID, format, "Upstream URL must be a fully-qualified https:// URL.")
			return
		}
		if u.Scheme != "https" && u.Scheme != "http" {
			c.reRenderUpstreams(gc, t.Name, t.ID, format, "Upstream URL must use http:// or https://.")
			return
		}
		// We do NOT enforce the hostname allowlist here at write time.
		// Why: ops set the allowlist via PKGMIRROR_UPSTREAM_ALLOWED_HOSTS
		// at boot, and the fetcher rechecks at request time. If the
		// admin saved a URL to mirror.internal but the allowlist isn't
		// extended yet, the fetcher will return ErrUpstreamForbidden -
		// fail closed at request time. Re-validating here would force
		// a config-order dependency between env vars and DB writes
		// that isn't worth the complexity for v1.
		_ = u
		_ = def
	}

	var ttl int64
	if ttlStr != "" {
		n, err := strconv.ParseInt(ttlStr, 10, 64)
		if err != nil || n < 0 || n > 86400 {
			c.reRenderUpstreams(gc, t.Name, t.ID, format, "Metadata TTL must be 0-86400 seconds.")
			return
		}
		ttl = n
	}

	if err := c.upstreams.Set(ctx, t.ID, format, mode, urlStr, ttl); err != nil {
		c.RenderError(gc, "save upstream config", err)
		return
	}
	c.auditTenant(gc, id, t.ID, "tenants.upstream.update", map[string]any{
		"format":           format,
		"mode":             mode,
		"upstream_url":     urlStr,
		"metadata_ttl_sec": ttl,
	})
	middleware.AddFlash(gc, middleware.FlashSuccess,
		fmt.Sprintf("Updated %s upstream for tenant %q.", format, t.Name))
	gc.Redirect(http.StatusSeeOther, "/console/tenants/"+t.Name+"/upstreams")
}

// ---- mutation: POST /console/tenants/:name/upstreams/:format/delete ----

// tenantUpstreamReset removes the per-tenant override so the resolver
// falls back to compiled-in defaults.
func (c *Console) tenantUpstreamReset(gc *gin.Context) {
	ctx := gc.Request.Context()
	id := auth.FromContext(gc)

	t, ok := c.resolveTenantFromPath(gc)
	if !ok {
		return
	}
	format := strings.ToLower(strings.TrimSpace(gc.Param("format")))
	if _, hasDefault := upstream.LookupDefault(format); !hasDefault {
		c.RenderNotFound(gc, "format %q has no upstream", format)
		return
	}
	if err := c.upstreams.Delete(ctx, t.ID, format); err != nil {
		c.RenderError(gc, "reset upstream config", err)
		return
	}
	c.auditTenant(gc, id, t.ID, "tenants.upstream.reset", map[string]any{
		"format": format,
	})
	middleware.AddFlash(gc, middleware.FlashSuccess,
		fmt.Sprintf("Reset %s upstream for tenant %q to compiled-in default.", format, t.Name))
	gc.Redirect(http.StatusSeeOther, "/console/tenants/"+t.Name+"/upstreams")
}

// reRenderUpstreams is the validation-error path: rebuild the page,
// attach the per-row error, highlight the row that was being edited.
func (c *Console) reRenderUpstreams(gc *gin.Context, tenantName string, tenantID int64, format, errMsg string) {
	ctx := gc.Request.Context()
	persisted, err := c.upstreams.ListByTenant(ctx, tenantID)
	if err != nil {
		c.RenderError(gc, "reload upstream config", err)
		return
	}
	c.Render(gc, "pages/tenants/upstreams", upstreamsPageData{
		TenantName: tenantName,
		Rows:       buildUpstreamRows(persisted),
		Errors:     map[string]string{format: errMsg},
		Edited:     format,
	})
}
