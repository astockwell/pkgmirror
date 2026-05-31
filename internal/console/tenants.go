package console

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/astockwell/pkgmirror/internal/audit"
	"github.com/astockwell/pkgmirror/internal/auth"
	"github.com/astockwell/pkgmirror/internal/console/middleware"
	"github.com/astockwell/pkgmirror/internal/models"
	"github.com/astockwell/pkgmirror/internal/tenants"
	"github.com/astockwell/pkgmirror/internal/users"

	"github.com/gin-gonic/gin"
)

// tenantNamePattern enforces the same shape as the registry-level
// tenant resolver: letters, digits, hyphens, underscores. Length is
// 2-40 chars (Forgejo/Gitea-style; conservative).
var tenantNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{2,40}$`)

// ---- list ----

type tenantListData struct {
	Tenants []tenantRow
}

type tenantRow struct {
	ID            int64
	Name          string
	VisibilityStr string
	IsPublic      bool
	PackageCount  int
}

func (c *Console) tenantsList(gc *gin.Context) {
	ctx := gc.Request.Context()
	id := auth.FromContext(gc)

	all, err := c.tenants.List(ctx)
	if err != nil {
		c.RenderError(gc, "list tenants", err)
		return
	}
	rows := make([]tenantRow, 0, len(all))
	for _, t := range all {
		// Non-admins see only tenants they can read; system admins see
		// everything (CanRead returns true via IsSystemAdmin).
		if id == nil || !id.CanRead(t.ID) {
			continue
		}
		count, _ := c.models.CountPackages(ctx, t.ID)
		rows = append(rows, tenantRow{
			ID:            t.ID,
			Name:          t.Name,
			VisibilityStr: visibilityString(t.Visibility),
			IsPublic:      t.Visibility == tenants.VisibilityPublic,
			PackageCount:  count,
		})
	}
	c.Render(gc, "pages/tenants/list", tenantListData{Tenants: rows})
}

// ---- detail ----

type tenantDetailData struct {
	Tenant       *tenants.Tenant
	Members      []memberRow
	PackageCount int

	// PackagesUploaded + PackagesPullThrough split PackageCount by
	// the packages.created_via column. Both sum to PackageCount when
	// no packages predate migration v6's safer 'uploaded' default; in
	// older deployments they sum to PackageCount minus a tiny tail
	// of unrecognized values that the helper round-trips as the raw
	// string.
	PackagesUploaded    int
	PackagesPullThrough int

	CanManage bool // whether to render member-management forms
}

type memberRow struct {
	UserID    int64
	UserName  string
	UserEmail string
	IsAdmin   bool
	RoleName  string
	RoleValue int
}

func (c *Console) tenantDetail(gc *gin.Context) {
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

	members, err := c.tenants.ListMembers(ctx, t.ID)
	if err != nil {
		c.RenderError(gc, "list tenant members", err)
		return
	}
	count, _ := c.models.CountPackages(ctx, t.ID)

	// Provenance split for the count summary. ListPackages already
	// SELECTs created_via; this avoids a separate GROUP BY query.
	var uploadedN, pullThroughN int
	if pkgs, perr := c.models.ListPackages(ctx, t.ID, ""); perr == nil {
		for _, p := range pkgs {
			switch p.CreatedVia {
			case models.CreatedViaUploaded:
				uploadedN++
			case models.CreatedViaPullThrough:
				pullThroughN++
			}
		}
	}

	rows := make([]memberRow, 0, len(members))
	for _, m := range members {
		email := ""
		if m.UserEmail.Valid {
			email = m.UserEmail.String
		}
		rows = append(rows, memberRow{
			UserID:    m.UserID,
			UserName:  m.UserName,
			UserEmail: email,
			IsAdmin:   m.IsAdmin,
			RoleName:  roleString(m.Role),
			RoleValue: int(m.Role),
		})
	}

	c.Render(gc, "pages/tenants/detail", tenantDetailData{
		Tenant:              t,
		Members:             rows,
		PackageCount:        count,
		PackagesUploaded:    uploadedN,
		PackagesPullThrough: pullThroughN,
		CanManage:           id.IsSystemAdmin(),
	})
}

// ---- new (form) + create (POST) ----

type tenantCreateForm struct {
	Name       string
	Visibility string
	Errors     map[string]string
}

func (c *Console) tenantNew(gc *gin.Context) {
	c.Render(gc, "pages/tenants/new", tenantCreateForm{
		Visibility: "private",
		Errors:     map[string]string{},
	})
}

func (c *Console) tenantCreate(gc *gin.Context) {
	ctx := gc.Request.Context()
	id := auth.FromContext(gc)

	f := tenantCreateForm{
		Name:       strings.TrimSpace(gc.PostForm("name")),
		Visibility: gc.PostForm("visibility"),
		Errors:     map[string]string{},
	}

	if f.Name == "" {
		f.Errors["name"] = "Required."
	} else if !tenantNamePattern.MatchString(f.Name) {
		f.Errors["name"] = "Letters, digits, - and _ only. 2-40 characters."
	}
	if f.Visibility != "public" && f.Visibility != "private" {
		f.Errors["visibility"] = "Must be public or private."
	}
	if len(f.Errors) > 0 {
		c.Render(gc, "pages/tenants/new", f)
		return
	}

	vis := tenants.VisibilityPrivate
	if f.Visibility == "public" {
		vis = tenants.VisibilityPublic
	}
	t, err := c.tenants.Create(ctx, f.Name, vis)
	if err != nil {
		if errors.Is(err, tenants.ErrDuplicate) {
			f.Errors["name"] = "A tenant with that name already exists."
			c.Render(gc, "pages/tenants/new", f)
			return
		}
		c.RenderError(gc, "create tenant", err)
		return
	}

	c.auditTenant(gc, id, t.ID, "tenants.create", map[string]any{
		"visibility": visibilityString(vis),
	})
	middleware.AddFlash(gc, middleware.FlashSuccess, fmt.Sprintf("Tenant %q created.", t.Name))
	gc.Redirect(http.StatusSeeOther, "/console/tenants/"+t.Name)
}

// ---- visibility update ----

func (c *Console) tenantSetVisibility(gc *gin.Context) {
	ctx := gc.Request.Context()
	id := auth.FromContext(gc)

	t, ok := c.resolveTenantFromPath(gc)
	if !ok {
		return
	}
	want := gc.PostForm("visibility")
	var vis tenants.Visibility
	switch want {
	case "public":
		vis = tenants.VisibilityPublic
	case "private":
		vis = tenants.VisibilityPrivate
	default:
		middleware.AddFlash(gc, middleware.FlashDanger, "Invalid visibility.")
		gc.Redirect(http.StatusSeeOther, "/console/tenants/"+t.Name)
		return
	}
	if vis == t.Visibility {
		gc.Redirect(http.StatusSeeOther, "/console/tenants/"+t.Name)
		return
	}
	if err := c.tenants.SetVisibility(ctx, t.ID, vis); err != nil {
		c.RenderError(gc, "update tenant visibility", err)
		return
	}
	c.auditTenant(gc, id, t.ID, "tenants.update", map[string]any{
		"visibility_from": visibilityString(t.Visibility),
		"visibility_to":   visibilityString(vis),
	})
	middleware.AddFlash(gc, middleware.FlashSuccess, "Visibility updated.")
	gc.Redirect(http.StatusSeeOther, "/console/tenants/"+t.Name)
}

// ---- member add ----

func (c *Console) tenantMemberAdd(gc *gin.Context) {
	ctx := gc.Request.Context()
	id := auth.FromContext(gc)

	t, ok := c.resolveTenantFromPath(gc)
	if !ok {
		return
	}
	username := strings.TrimSpace(gc.PostForm("username"))
	roleStr := gc.PostForm("role")
	role, ok := parseRole(roleStr)
	if !ok || username == "" {
		middleware.AddFlash(gc, middleware.FlashDanger, "Username and role are required.")
		gc.Redirect(http.StatusSeeOther, "/console/tenants/"+t.Name)
		return
	}
	u, err := c.users.GetByName(ctx, username)
	if err != nil {
		if errors.Is(err, users.ErrNotExist) {
			middleware.AddFlash(gc, middleware.FlashDanger, fmt.Sprintf("No user named %q.", username))
			gc.Redirect(http.StatusSeeOther, "/console/tenants/"+t.Name)
			return
		}
		c.RenderError(gc, "look up user", err)
		return
	}
	if err := c.tenants.AddMember(ctx, t.ID, u.ID, role); err != nil {
		c.RenderError(gc, "add tenant member", err)
		return
	}
	c.auditTenant(gc, id, t.ID, "tenants.member.add", map[string]any{
		"target_user_id": u.ID,
		"target_user":    u.Name,
		"role":           roleString(role),
	})
	middleware.AddFlash(gc, middleware.FlashSuccess, fmt.Sprintf("Added %s as %s.", u.Name, roleString(role)))
	gc.Redirect(http.StatusSeeOther, "/console/tenants/"+t.Name)
}

// ---- member remove (POST .../delete) ----

func (c *Console) tenantMemberRemove(gc *gin.Context) {
	ctx := gc.Request.Context()
	id := auth.FromContext(gc)

	t, ok := c.resolveTenantFromPath(gc)
	if !ok {
		return
	}
	uidStr := gc.Param("user_id")
	uid, err := strconv.ParseInt(uidStr, 10, 64)
	if err != nil || uid <= 0 {
		c.RenderNotFound(gc, "user not found")
		return
	}
	target, err := c.users.GetByID(ctx, uid)
	if err != nil {
		if errors.Is(err, users.ErrNotExist) {
			c.RenderNotFound(gc, "user not found")
			return
		}
		c.RenderError(gc, "look up user", err)
		return
	}
	if err := c.tenants.RemoveMember(ctx, t.ID, uid); err != nil {
		c.RenderError(gc, "remove tenant member", err)
		return
	}
	c.auditTenant(gc, id, t.ID, "tenants.member.remove", map[string]any{
		"target_user_id": uid,
		"target_user":    target.Name,
	})
	middleware.AddFlash(gc, middleware.FlashSuccess, fmt.Sprintf("Removed %s.", target.Name))
	gc.Redirect(http.StatusSeeOther, "/console/tenants/"+t.Name)
}

// ---- helpers ----

// resolveTenantFromPath looks up :name (sole path param) and renders
// either a 404 (unknown tenant) or a 500 (DB error). Returns (t, true)
// on success; (nil, false) on any failure (response already written).
func (c *Console) resolveTenantFromPath(gc *gin.Context) (*tenants.Tenant, bool) {
	name := gc.Param("name")
	if name == "" {
		c.RenderNotFound(gc, "tenant not specified")
		return nil, false
	}
	t, err := c.tenants.GetByName(gc.Request.Context(), name)
	if err != nil {
		if errors.Is(err, tenants.ErrNotExist) {
			c.RenderNotFound(gc, "tenant %q not found", name)
			return nil, false
		}
		c.RenderError(gc, "look up tenant", err)
		return nil, false
	}
	return t, true
}

func (c *Console) auditTenant(gc *gin.Context, id *auth.Identity, tenantID int64, action string, extra map[string]any) {
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

func visibilityString(v tenants.Visibility) string {
	if v == tenants.VisibilityPublic {
		return "public"
	}
	return "private"
}

func roleString(r tenants.Role) string {
	switch r {
	case tenants.RoleReader:
		return "reader"
	case tenants.RoleWriter:
		return "writer"
	case tenants.RoleTenantAdmin:
		return "admin"
	default:
		return fmt.Sprintf("role-%d", int(r))
	}
}

func parseRole(s string) (tenants.Role, bool) {
	switch s {
	case "reader":
		return tenants.RoleReader, true
	case "writer":
		return tenants.RoleWriter, true
	case "admin":
		return tenants.RoleTenantAdmin, true
	}
	return 0, false
}
