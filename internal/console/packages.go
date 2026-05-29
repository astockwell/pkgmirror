package console

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/astockwell/pkgmirror/internal/audit"
	"github.com/astockwell/pkgmirror/internal/auth"
	"github.com/astockwell/pkgmirror/internal/console/middleware"
	"github.com/astockwell/pkgmirror/internal/models"
	"github.com/astockwell/pkgmirror/internal/tenants"

	"github.com/gin-gonic/gin"
)

// packagesByTenantData drives pages/packages/by-tenant.
type packagesByTenantData struct {
	Tenant   *tenants.Tenant
	Packages []packageRow
}

type packageRow struct {
	PackageID int64
	Type      string
	Name      string
	Versions  int
}

// packagesByTenant lists every package inside a tenant. Read-only.
func (c *Console) packagesByTenant(gc *gin.Context) {
	ctx := gc.Request.Context()
	id := auth.FromContext(gc)
	t, ok := c.resolveTenantFromPath(gc)
	if !ok {
		return
	}
	if id == nil || !id.CanRead(t.ID) {
		c.RenderForbidden(gc, "You don't have access to tenant %q.", t.Name)
		return
	}

	pkgs, err := c.models.ListPackages(ctx, t.ID, "")
	if err != nil {
		c.RenderError(gc, "list packages", err)
		return
	}
	rows := make([]packageRow, 0, len(pkgs))
	for _, p := range pkgs {
		vers, _ := c.models.ListVersions(ctx, p.ID)
		rows = append(rows, packageRow{
			PackageID: p.ID,
			Type:      string(p.Type),
			Name:      p.Name,
			Versions:  len(vers),
		})
	}
	c.Render(gc, "pages/packages/by-tenant", packagesByTenantData{
		Tenant: t, Packages: rows,
	})
}

// packageDetailData drives pages/packages/detail.
type packageDetailData struct {
	Tenant   *tenants.Tenant
	Package  *models.Package
	Versions []versionRow
}

type versionRow struct {
	VersionID    int64
	Version      string
	CreatedUnix  int64
	Quarantined  bool
	Reason       string
	License      string
}

// packageDetail shows one package's versions inside a tenant.
func (c *Console) packageDetail(gc *gin.Context) {
	ctx := gc.Request.Context()
	id := auth.FromContext(gc)
	t, ok := c.resolveTenantFromPath(gc)
	if !ok {
		return
	}
	if id == nil || !id.CanRead(t.ID) {
		c.RenderForbidden(gc, "You don't have access to tenant %q.", t.Name)
		return
	}
	pkgType := gc.Param("type")
	pkgName := gc.Param("pkgname")
	if pkgType == "" || pkgName == "" {
		c.RenderNotFound(gc, "package not specified")
		return
	}

	pkg, err := c.models.GetPackage(ctx, t.ID, models.Type(pkgType), pkgName)
	if err != nil {
		if errors.Is(err, models.ErrPackageNotExist) {
			c.RenderNotFound(gc, "package %s/%s not found in tenant %s", pkgType, pkgName, t.Name)
			return
		}
		c.RenderError(gc, "look up package", err)
		return
	}
	vers, err := c.models.ListVersions(ctx, pkg.ID)
	if err != nil {
		c.RenderError(gc, "list versions", err)
		return
	}
	rows := make([]versionRow, 0, len(vers))
	for _, v := range vers {
		row := versionRow{
			VersionID:   v.ID,
			Version:     v.Version,
			CreatedUnix: v.CreatedUnix,
			Quarantined: v.IsQuarantined(),
		}
		if v.QuarantineReason.Valid {
			row.Reason = v.QuarantineReason.String
		}
		if v.License.Valid {
			row.License = v.License.String
		}
		rows = append(rows, row)
	}
	c.Render(gc, "pages/packages/detail", packageDetailData{
		Tenant: t, Package: pkg, Versions: rows,
	})
}

// quarantineListData drives pages/quarantine/list.
type quarantineListData struct {
	Rows []quarantineRow
}

type quarantineRow struct {
	VersionID   int64
	TenantID    int64
	Type        string
	PackageName string
	Version     string
	Reason      string
	CreatedUnix int64
}

// quarantineList shows every quarantined version in the deployment.
// System admin only (mounted under RequireSystemAdmin).
func (c *Console) quarantineList(gc *gin.Context) {
	ctx := gc.Request.Context()
	rows, err := c.models.ListQuarantined(ctx, 0)
	if err != nil {
		c.RenderError(gc, "list quarantined", err)
		return
	}
	out := make([]quarantineRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, quarantineRow{
			VersionID:   r.VersionID,
			TenantID:    r.TenantID,
			Type:        string(r.Type),
			PackageName: r.PackageName,
			Version:     r.Version,
			Reason:      r.Reason,
			CreatedUnix: r.CreatedUnix,
		})
	}
	c.Render(gc, "pages/quarantine/list", quarantineListData{Rows: out})
}

// quarantinePromote clears the quarantine on a version. POST-only.
func (c *Console) quarantinePromote(gc *gin.Context) {
	c.quarantineAction(gc, "promote")
}

// quarantineReject keeps the bytes but marks the version "REJECTED:"
// for forensics. The plan defers raw delete to v2 (overlaps with
// retention policy) so this is the destructive end of the quarantine
// flow today.
func (c *Console) quarantineReject(gc *gin.Context) {
	c.quarantineAction(gc, "reject")
}

func (c *Console) quarantineAction(gc *gin.Context, kind string) {
	ctx := gc.Request.Context()
	id := auth.FromContext(gc)

	versionID, err := strconv.ParseInt(gc.Param("version_id"), 10, 64)
	if err != nil || versionID <= 0 {
		c.RenderNotFound(gc, "version not specified")
		return
	}
	reason := strings.TrimSpace(gc.PostForm("reason"))

	switch kind {
	case "promote":
		if err := c.models.PromoteVersion(ctx, versionID); err != nil {
			if errors.Is(err, models.ErrVersionNotExist) {
				c.RenderNotFound(gc, "version not found")
				return
			}
			c.RenderError(gc, "promote version", err)
			return
		}
		c.auditQuarantine(gc, id, "quarantine.promote", versionID, reason)
		middleware.AddFlash(gc, middleware.FlashSuccess, fmt.Sprintf("Version #%d promoted.", versionID))

	case "reject":
		if reason == "" {
			reason = "rejected by admin"
		}
		// Reuse QuarantineVersion with REJECTED prefix per the
		// existing admin REST flow; keeps the schema small.
		if err := c.models.QuarantineVersion(ctx, versionID, 0, "REJECTED: "+reason); err != nil {
			if errors.Is(err, models.ErrVersionNotExist) {
				c.RenderNotFound(gc, "version not found")
				return
			}
			c.RenderError(gc, "reject version", err)
			return
		}
		c.auditQuarantine(gc, id, "quarantine.reject", versionID, reason)
		middleware.AddFlash(gc, middleware.FlashSuccess, fmt.Sprintf("Version #%d marked rejected.", versionID))
	}

	gc.Redirect(http.StatusSeeOther, "/console/quarantine")
}

func (c *Console) auditQuarantine(gc *gin.Context, id *auth.Identity, action string, versionID int64, reason string) {
	if c.audit == nil {
		return
	}
	ev := audit.Event{
		Action:     action,
		ActorKind:  "session",
		RequestID:  middleware.RequestIDFrom(gc),
		RemoteAddr: middleware.ClientIPFor(gc.Request, c.cfg.TrustedProxies),
		UserAgent:  gc.Request.UserAgent(),
		Reason:     reason,
		Extra:      map[string]any{"version_id": versionID},
	}
	if id != nil && id.User != nil {
		ev.UserID = id.User.ID
	}
	c.audit.Log(ev)
}
