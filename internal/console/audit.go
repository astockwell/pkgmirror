package console

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/astockwell/pkgmirror/internal/audit"

	"github.com/gin-gonic/gin"
)

// auditPageData drives pages/audit/list.
type auditPageData struct {
	Filter  auditFilter
	Events  []auditRow
	HasMore bool
}

type auditFilter struct {
	TenantID int64
	Action   string
	Decision string
	UserID   int64
	Limit    int
}

type auditRow struct {
	WhenUnix   int64
	Action     string
	ActorKind  string
	ActorID    int64
	TenantID   int64
	Format     string
	Package    string
	Version    string
	Decision   string
	Reason     string
	RequestID  string
	RemoteAddr string
}

// auditList renders the audit query page. system-admin only (mounted
// on the adminOnly group). Filters come from query string so the same
// URL is bookmarkable + shareable.
func (c *Console) auditList(gc *gin.Context) {
	ctx := gc.Request.Context()
	filter := parseAuditFilter(gc)

	q := audit.Query{
		TenantID: filter.TenantID,
		Action:   filter.Action,
		Decision: filter.Decision,
		UserID:   filter.UserID,
		Limit:    filter.Limit,
	}
	events, err := audit.ListEvents(ctx, c.models.DB, q)
	if err != nil {
		c.RenderError(gc, "list audit events", err)
		return
	}

	rows := make([]auditRow, 0, len(events))
	for _, e := range events {
		rows = append(rows, auditRow{
			WhenUnix:   e.CreatedUnix,
			Action:     e.Action,
			ActorKind:  e.ActorKind,
			ActorID:    e.UserID,
			TenantID:   e.TenantID,
			Format:     e.Format,
			Package:    e.Package,
			Version:    e.Version,
			Decision:   e.Decision,
			Reason:     e.Reason,
			RequestID:  e.RequestID,
			RemoteAddr: e.RemoteAddr,
		})
	}

	c.Render(gc, "pages/audit/list", auditPageData{
		Filter:  filter,
		Events:  rows,
		HasMore: len(rows) >= filter.Limit,
	})
}

// parseAuditFilter pulls filter values from the query string with
// sensible defaults. Unknown / blank values become zero/empty.
func parseAuditFilter(gc *gin.Context) auditFilter {
	f := auditFilter{
		Action:   strings.TrimSpace(gc.Query("action")),
		Decision: strings.TrimSpace(gc.Query("decision")),
		Limit:    100,
	}
	if v := strings.TrimSpace(gc.Query("tenant_id")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			f.TenantID = n
		}
	}
	if v := strings.TrimSpace(gc.Query("user_id")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			f.UserID = n
		}
	}
	if v := strings.TrimSpace(gc.Query("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			f.Limit = n
		}
	}
	if f.Limit > 1000 {
		f.Limit = 1000 // matches audit.ListEvents cap
	}
	return f
}

// auditExportCSV streams the current query results as CSV.
// Kept simple; no rate-limit because system-admin-only + capped at the
// same 1000-row limit as the page.
func (c *Console) auditExportCSV(gc *gin.Context) {
	ctx := gc.Request.Context()
	filter := parseAuditFilter(gc)
	if filter.Limit > 1000 {
		filter.Limit = 1000
	}
	events, err := audit.ListEvents(ctx, c.models.DB, audit.Query{
		TenantID: filter.TenantID,
		Action:   filter.Action,
		Decision: filter.Decision,
		UserID:   filter.UserID,
		Limit:    filter.Limit,
	})
	if err != nil {
		c.RenderError(gc, "export audit", err)
		return
	}
	gc.Header("Content-Type", "text/csv; charset=utf-8")
	gc.Header("Content-Disposition", `attachment; filename="audit.csv"`)
	gc.Status(http.StatusOK)
	w := gc.Writer
	_, _ = w.WriteString("created_unix,action,actor_kind,actor_user_id,tenant_id,format,package,version,decision,reason,request_id,remote_addr\n")
	for _, e := range events {
		_, _ = w.WriteString(csvJoin(
			strconv.FormatInt(e.CreatedUnix, 10),
			e.Action,
			e.ActorKind,
			strconv.FormatInt(e.UserID, 10),
			strconv.FormatInt(e.TenantID, 10),
			e.Format, e.Package, e.Version,
			e.Decision, e.Reason, e.RequestID, e.RemoteAddr,
		))
		_, _ = w.WriteString("\n")
	}
}

// csvJoin produces an RFC4180-ish CSV row. Quotes values that contain
// commas, quotes, or newlines; doubles internal quotes.
func csvJoin(fields ...string) string {
	parts := make([]string, len(fields))
	for i, v := range fields {
		parts[i] = csvField(v)
	}
	return strings.Join(parts, ",")
}

func csvField(s string) string {
	if !strings.ContainsAny(s, ",\"\n\r") {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
