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
	Tenant    *tenants.Tenant
	Packages  []packageRow
	CanManage bool // system admin -> render delete buttons

	// ProvenanceFilter is the active ?provenance= query param value
	// ("" / "uploaded" / "pull_through"); the template uses it to
	// render the current select-option as selected.
	ProvenanceFilter string
}

type packageRow struct {
	PackageID  int64
	Type       string
	Name       string
	Versions   int
	Provenance string // human-readable form of created_via; "" suppresses badge
}

// packagesGlobalData drives pages/packages/global. Lists every package
// across every tenant the viewer can read - same access model as
// tenantsList (system admins see all; everyone else sees only their
// tenants). The sidebar Packages link routes here so users have a
// useful landing page even before they've picked a tenant.
type packagesGlobalData struct {
	Rows             []globalPackageRow
	ProvenanceFilter string // "" / "uploaded" / "pull_through"
}

type globalPackageRow struct {
	TenantName string
	Type       string
	Name       string
	Versions   int
	Provenance string // human-readable form of created_via
}

// packagesGlobal is the sidebar Packages destination - a cross-tenant
// listing scoped to whatever tenants the viewer can read. Read-only;
// delete + per-package actions live on the per-tenant detail page so
// we don't have to reimplement the system-admin gating here.
func (c *Console) packagesGlobal(gc *gin.Context) {
	ctx := gc.Request.Context()
	id := auth.FromContext(gc)
	provFilter := gc.Query("provenance")

	all, err := c.tenants.List(ctx)
	if err != nil {
		c.RenderError(gc, "list tenants", err)
		return
	}
	rows := []globalPackageRow{}
	for _, t := range all {
		if id == nil || !id.CanRead(t.ID) {
			continue
		}
		pkgs, err := c.models.ListPackages(ctx, t.ID, "")
		if err != nil {
			c.RenderError(gc, "list packages", err)
			return
		}
		for _, p := range pkgs {
			if provFilter != "" && string(p.CreatedVia) != provFilter {
				continue
			}
			vers, _ := c.models.ListVersions(ctx, p.ID)
			rows = append(rows, globalPackageRow{
				TenantName: t.Name,
				Type:       string(p.Type),
				Name:       p.Name,
				Versions:   len(vers),
				Provenance: provenanceLabel(p.CreatedVia),
			})
		}
	}
	c.Render(gc, "pages/packages/global", packagesGlobalData{
		Rows:             rows,
		ProvenanceFilter: provFilter,
	})
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
	provFilter := gc.Query("provenance")
	rows := make([]packageRow, 0, len(pkgs))
	for _, p := range pkgs {
		if provFilter != "" && string(p.CreatedVia) != provFilter {
			continue
		}
		vers, _ := c.models.ListVersions(ctx, p.ID)
		rows = append(rows, packageRow{
			PackageID:  p.ID,
			Type:       string(p.Type),
			Name:       p.Name,
			Versions:   len(vers),
			Provenance: provenanceLabel(p.CreatedVia),
		})
	}
	c.Render(gc, "pages/packages/by-tenant", packagesByTenantData{
		Tenant: t, Packages: rows,
		CanManage:        id.IsSystemAdmin(),
		ProvenanceFilter: provFilter,
	})
}

// packageDetailData drives pages/packages/detail.
type packageDetailData struct {
	Tenant    *tenants.Tenant
	Package   *models.Package
	Versions  []versionRow
	CanManage bool // system admin -> render delete buttons + provenance flip form

	// ProvenanceLabel is the human-readable form of Package.CreatedVia
	// used by the badge. "mirrored from upstream" / "uploaded".
	ProvenanceLabel string
	// ProvenanceFlipTarget is the value to set on the flip-form button
	// (i.e. the OPPOSITE of the current provenance). Empty when the
	// current value isn't a known constant - we'd rather not surface
	// the toggle than guess wrong.
	ProvenanceFlipTarget string
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
//
// :pkgname is a catch-all (see route registration) so module paths
// containing slashes (e.g. Go's "rsc.io/quote") survive routing.
// gc.Param("pkgname") returns the captured suffix with the leading
// slash still attached - strip it before lookup.
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
	pkgName := strings.TrimPrefix(gc.Param("pkgname"), "/")
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
		CanManage:            id.IsSystemAdmin(),
		ProvenanceLabel:      provenanceLabel(pkg.CreatedVia),
		ProvenanceFlipTarget: provenanceFlipTarget(pkg.CreatedVia),
	})
}

// provenanceLabel renders a CreatedVia value for human display.
// Unknown values (operators occasionally set future-binary values
// via direct UPDATE) round-trip to the raw string so the page
// stays informative.
func provenanceLabel(v models.CreatedVia) string {
	switch v {
	case models.CreatedViaUploaded:
		return "uploaded"
	case models.CreatedViaPullThrough:
		return "mirrored from upstream"
	}
	return string(v)
}

// provenanceFlipTarget returns the CreatedVia value the flip button
// should set. Returns empty for unknown current values so the
// template can hide the form rather than guess.
func provenanceFlipTarget(v models.CreatedVia) string {
	switch v {
	case models.CreatedViaUploaded:
		return string(models.CreatedViaPullThrough)
	case models.CreatedViaPullThrough:
		return string(models.CreatedViaUploaded)
	}
	return ""
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

// ---- destructive: delete package + delete version ----
//
// Both are system-admin only and POST-only with CSRF; the templates
// wrap the buttons in a JS confirm() prompt that names the target.
// Blobs themselves are NOT removed - they're content-addressed; an
// orphan GC pass collects unreferenced blobs (out of scope for now).

// packageDelete drops the entire package + every version + every file
// row. Properties hanging off any of those are cleaned up too (see
// models.DeletePackage). Audit row emitted before mutation so an op
// failure still leaves a forensic trail.
//
// Routed by :package_id rather than (tenant, type, name) because the
// GET show page uses a *pkgname catch-all and Gin/httprouter forbid
// sibling routes under one. The handler resolves the package row first,
// then derives the tenant for the redirect target. System-admin gating
// is enforced by the route group.
func (c *Console) packageDelete(gc *gin.Context) {
	ctx := gc.Request.Context()
	id := auth.FromContext(gc)

	pkgID, err := strconv.ParseInt(gc.Param("package_id"), 10, 64)
	if err != nil || pkgID <= 0 {
		c.RenderNotFound(gc, "package not specified")
		return
	}
	pkg, err := c.models.GetPackageByID(ctx, pkgID)
	if err != nil {
		if errors.Is(err, models.ErrPackageNotExist) {
			c.RenderNotFound(gc, "package #%d not found", pkgID)
			return
		}
		c.RenderError(gc, "look up package", err)
		return
	}
	t, err := c.tenants.GetByID(ctx, pkg.TenantID)
	if err != nil {
		c.RenderError(gc, "look up tenant", err)
		return
	}
	// Count versions for the audit row + flash message before we drop.
	vers, _ := c.models.ListVersions(ctx, pkg.ID)

	if err := c.models.DeletePackage(ctx, pkg.ID); err != nil {
		c.RenderError(gc, "delete package", err)
		return
	}
	c.auditPackage(gc, id, t.ID, "tenants.package.delete", map[string]any{
		"package_id":    pkg.ID,
		"package_type":  string(pkg.Type),
		"package_name":  pkg.Name,
		"version_count": len(vers),
	})
	middleware.AddFlash(gc, middleware.FlashSuccess,
		fmt.Sprintf("Deleted package %s/%s (%d version%s).", pkg.Type, pkg.Name, len(vers), plural(len(vers))))
	gc.Redirect(http.StatusSeeOther,
		"/console/tenants/"+t.Name+"/packages")
}

// packageSetProvenance is the admin override for packages.created_via.
// Looks up the package by ID, validates the new value against
// models.CreatedVia.Valid(), persists, and audits.
// Per plans/created-via-package-ownership.md, the security-relevant
// implication ('pull_through' enables the /simple/ merge and the
// upload-against-pull_through 409) is shown in the form's confirm
// dialog rather than buried in a tooltip.
//
// Routed by :package_id; see packageDelete for the rationale.
func (c *Console) packageSetProvenance(gc *gin.Context) {
	ctx := gc.Request.Context()
	id := auth.FromContext(gc)

	pkgID, err := strconv.ParseInt(gc.Param("package_id"), 10, 64)
	if err != nil || pkgID <= 0 {
		c.RenderNotFound(gc, "package not specified")
		return
	}

	newVia := models.CreatedVia(gc.PostForm("created_via"))
	if !newVia.Valid() {
		c.RenderError(gc, "set package provenance",
			fmt.Errorf("created_via %q is not a recognized value", newVia))
		return
	}

	pkg, err := c.models.GetPackageByID(ctx, pkgID)
	if err != nil {
		if errors.Is(err, models.ErrPackageNotExist) {
			c.RenderNotFound(gc, "package #%d not found", pkgID)
			return
		}
		c.RenderError(gc, "look up package", err)
		return
	}
	t, err := c.tenants.GetByID(ctx, pkg.TenantID)
	if err != nil {
		c.RenderError(gc, "look up tenant", err)
		return
	}
	old := pkg.CreatedVia

	if err := c.models.SetPackageCreatedVia(ctx, pkg.ID, newVia); err != nil {
		c.RenderError(gc, "update provenance", err)
		return
	}
	c.auditPackage(gc, id, t.ID, "tenants.package.set_provenance", map[string]any{
		"package_id":   pkg.ID,
		"package_type": string(pkg.Type),
		"package_name": pkg.Name,
		"old":          string(old),
		"new":          string(newVia),
		"source":       "console",
	})
	middleware.AddFlash(gc, middleware.FlashSuccess,
		fmt.Sprintf("Provenance for %s/%s set to %q.",
			pkg.Type, pkg.Name, provenanceLabel(newVia)))
	gc.Redirect(http.StatusSeeOther,
		"/console/tenants/"+t.Name+"/packages/"+string(pkg.Type)+"/"+pkg.Name)
}

// versionDelete drops a single version from a package; the package row
// itself stays. Version's files (CASCADE) and any properties on the
// version + its files (transactional cleanup in models.DeleteVersion)
// go too. :version_id must belong to the named :package_id - we
// enforce that to stop an admin from deleting a foreign version by
// guessing IDs.
//
// Routed by :package_id; see packageDelete for the rationale.
func (c *Console) versionDelete(gc *gin.Context) {
	ctx := gc.Request.Context()
	id := auth.FromContext(gc)

	pkgID, err := strconv.ParseInt(gc.Param("package_id"), 10, 64)
	if err != nil || pkgID <= 0 {
		c.RenderNotFound(gc, "package not specified")
		return
	}
	versionID, err := strconv.ParseInt(gc.Param("version_id"), 10, 64)
	if err != nil || versionID <= 0 {
		c.RenderNotFound(gc, "version not specified")
		return
	}

	pkg, err := c.models.GetPackageByID(ctx, pkgID)
	if err != nil {
		if errors.Is(err, models.ErrPackageNotExist) {
			c.RenderNotFound(gc, "package #%d not found", pkgID)
			return
		}
		c.RenderError(gc, "look up package", err)
		return
	}
	t, err := c.tenants.GetByID(ctx, pkg.TenantID)
	if err != nil {
		c.RenderError(gc, "look up tenant", err)
		return
	}

	// Belt-and-braces: the version row must belong to this package.
	// Walking the list is fine here - admin pages are not hot-path
	// and packages with thousands of versions are rare.
	vers, err := c.models.ListVersions(ctx, pkg.ID)
	if err != nil {
		c.RenderError(gc, "list versions", err)
		return
	}
	var match *models.Version
	for _, v := range vers {
		if v.ID == versionID {
			match = v
			break
		}
	}
	if match == nil {
		c.RenderNotFound(gc, "version #%d does not belong to package %s/%s", versionID, pkg.Type, pkg.Name)
		return
	}

	if err := c.models.DeleteVersion(ctx, versionID); err != nil {
		c.RenderError(gc, "delete version", err)
		return
	}
	c.auditPackage(gc, id, t.ID, "tenants.package.version.delete", map[string]any{
		"package_id":   pkg.ID,
		"package_type": string(pkg.Type),
		"package_name": pkg.Name,
		"version_id":   versionID,
		"version":      match.Version,
	})
	middleware.AddFlash(gc, middleware.FlashSuccess,
		fmt.Sprintf("Deleted version %s of %s/%s.", match.Version, pkg.Type, pkg.Name))
	gc.Redirect(http.StatusSeeOther,
		"/console/tenants/"+t.Name+"/packages/"+string(pkg.Type)+"/"+pkg.Name)
}

// auditPackage records a package- or version-scoped admin action.
// Shape mirrors auditTenant; kept separate so the action vocabulary
// stays grep-friendly when reviewing the audit log.
func (c *Console) auditPackage(gc *gin.Context, id *auth.Identity, tenantID int64, action string, extra map[string]any) {
	if c.audit == nil {
		return
	}
	ev := audit.Event{
		Action:     action,
		ActorKind:  "session",
		TenantID:   tenantID,
		RequestID:  middleware.RequestIDFrom(gc),
		RemoteAddr: middleware.ClientIPFor(gc.Request, c.cfg.TrustedProxies),
		UserAgent:  gc.Request.UserAgent(),
		Extra:      extra,
	}
	if id != nil && id.User != nil {
		ev.UserID = id.User.ID
	}
	c.audit.Log(ev)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
