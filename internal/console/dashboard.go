package console

import (
	"time"

	"github.com/astockwell/pkgmirror/internal/audit"
	"github.com/astockwell/pkgmirror/internal/auth"

	"github.com/gin-gonic/gin"
)

// dashboardData drives pages/dashboard.tmpl.
type dashboardData struct {
	TotalTenants   int
	TotalPackages  int
	TotalVersions  int
	YourTenants    int
	RecentIngests  []dashboardIngest
	Decisions24h   []dashboardDecision
	WindowHours    int
}

type dashboardIngest struct {
	WhenUnix int64
	Format   string
	Package  string
	Version  string
	Decision string
	TenantID int64
}

type dashboardDecision struct {
	Decision string
	Count    int64
}

// dashboard renders the /console/ landing page. Read-only.
//
// Three widgets pull from independent stores; we run them sequentially
// rather than in goroutines because at console scale the cost is
// trivial and the ordering keeps the failure mode simple (any one
// failure -> 500 via RenderError).
func (c *Console) dashboard(gc *gin.Context) {
	ctx := gc.Request.Context()
	id := auth.FromContext(gc)

	totalTenants, err := c.tenants.Count(ctx)
	if err != nil {
		c.RenderError(gc, "count tenants", err)
		return
	}
	totalPackages, err := c.models.CountPackages(ctx, 0)
	if err != nil {
		c.RenderError(gc, "count packages", err)
		return
	}
	totalVersions, err := c.models.CountVersions(ctx, 0)
	if err != nil {
		c.RenderError(gc, "count versions", err)
		return
	}

	// "Your" counts come from the memberships already resolved in
	// baseData but are also handy on the dashboard.
	var yours int
	if id != nil {
		yours = len(id.Memberships)
	}

	// Recent ingests: pull last 10 audit rows with action=ingest.
	rawEvents, err := audit.ListEvents(ctx, c.models.DB, audit.Query{
		Action: "ingest",
		Limit:  10,
	})
	if err != nil {
		c.RenderError(gc, "list recent ingests", err)
		return
	}
	recent := make([]dashboardIngest, 0, len(rawEvents))
	for _, e := range rawEvents {
		recent = append(recent, dashboardIngest{
			WhenUnix: e.CreatedUnix,
			Format:   e.Format,
			Package:  e.Package,
			Version:  e.Version,
			Decision: e.Decision,
			TenantID: e.TenantID,
		})
	}

	// Decision rollup: last 24h.
	since := time.Now().Add(-24 * time.Hour).Unix()
	rollup, err := audit.DecisionRollup(ctx, c.models.DB, since, 0)
	if err != nil {
		c.RenderError(gc, "summarize decisions", err)
		return
	}
	decisions := make([]dashboardDecision, 0, len(rollup))
	for d, n := range rollup {
		decisions = append(decisions, dashboardDecision{Decision: d, Count: n})
	}
	// Deterministic order for the table: allow, warn, quarantine, deny,
	// then anything else alphabetically.
	sortDecisions(decisions)

	c.Render(gc, "pages/dashboard", dashboardData{
		TotalTenants:  totalTenants,
		TotalPackages: totalPackages,
		TotalVersions: totalVersions,
		YourTenants:   yours,
		RecentIngests: recent,
		Decisions24h:  decisions,
		WindowHours:   24,
	})
}

// sortDecisions orders a fixed set of well-known decisions first, then
// anything else alphabetically. Stable enough for a dashboard widget.
func sortDecisions(in []dashboardDecision) {
	priority := map[string]int{
		"allow":      0,
		"warn":       1,
		"quarantine": 2,
		"deny":       3,
	}
	// Insertion sort; tiny N (4-ish), no need for sort.Slice overhead.
	for i := 1; i < len(in); i++ {
		j := i
		for j > 0 && less(in[j], in[j-1], priority) {
			in[j], in[j-1] = in[j-1], in[j]
			j--
		}
	}
}

func less(a, b dashboardDecision, priority map[string]int) bool {
	pa, oka := priority[a.Decision]
	pb, okb := priority[b.Decision]
	if oka && okb {
		return pa < pb
	}
	if oka {
		return true
	}
	if okb {
		return false
	}
	return a.Decision < b.Decision
}
