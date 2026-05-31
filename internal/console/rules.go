package console

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/astockwell/pkgmirror/internal/audit"
	"github.com/astockwell/pkgmirror/internal/auth"
	"github.com/astockwell/pkgmirror/internal/console/middleware"
	"github.com/astockwell/pkgmirror/internal/policy"

	"github.com/gin-gonic/gin"
)

// rulesListData drives pages/rules/list.
type rulesListData struct {
	Rules []ruleRow
}

type ruleRow struct {
	ID         int64
	Name       string
	Kind       string
	Action     string
	Priority   int
	Enabled    bool
	Format     string
	TenantID   int64
	Package    string
	VersionPat string
	ExpiresAt  int64
}

// rulesDeps is set on Console at construction (added in PR 7).
// Removed once Console grew a rules field directly.

func (c *Console) rulesList(gc *gin.Context) {
	ctx := gc.Request.Context()
	if c.rules == nil {
		c.RenderError(gc, "list rules", errors.New("rule store not configured"))
		return
	}
	all, err := c.rules.ListAll(ctx)
	if err != nil {
		c.RenderError(gc, "list rules", err)
		return
	}
	rows := make([]ruleRow, 0, len(all))
	for _, r := range all {
		rows = append(rows, ruleRow{
			ID: r.ID, Name: r.Name, Kind: r.Kind, Action: r.Action,
			Priority: r.Priority, Enabled: r.Enabled,
			Format: r.Format, TenantID: r.TenantID,
			Package: r.PackageLowerName, VersionPat: r.VersionPattern,
			ExpiresAt: r.ExpiresUnix,
		})
	}
	c.Render(gc, "pages/rules/list", rulesListData{Rules: rows})
}

// ruleEditData drives pages/rules/edit. ID == 0 means "new rule".
type ruleEditData struct {
	ID         int64
	Name       string
	Kind       string
	Action     string
	Priority   int
	Enabled    bool
	Format     string
	TenantID   int64
	Package    string
	VersionPat string
	ConfigJSON string
	ExpiresAt  int64
	Errors     map[string]string

	// Tenants is rendered as a hint under the Tenant ID input so the
	// admin doesn't have to grep the DB for the numeric ID. Empty when
	// the tenant store lookup fails (degraded, not fatal).
	Tenants []tenantHint
}

// tenantHint is the (id, name) pair rendered in the rule edit form.
type tenantHint struct {
	ID   int64
	Name string
}

// listTenantHints loads every tenant for the edit form's helper line.
// Errors are swallowed - the field is decorative.
func (c *Console) listTenantHints(gc *gin.Context) []tenantHint {
	all, err := c.tenants.List(gc.Request.Context())
	if err != nil {
		return nil
	}
	out := make([]tenantHint, 0, len(all))
	for _, t := range all {
		out = append(out, tenantHint{ID: t.ID, Name: t.Name})
	}
	return out
}

func (c *Console) ruleNew(gc *gin.Context) {
	// Default config_json that actually works for the default kind
	// (cooldown). Saves the admin from needing to know the JSON shape
	// just to get past form validation on first save.
	c.Render(gc, "pages/rules/edit", ruleEditData{
		Priority:   100,
		Enabled:    true,
		Action:     "warn",
		Kind:       "cooldown",
		ConfigJSON: `{"min_age_days": 14}`,
		Errors:     map[string]string{},
		Tenants:    c.listTenantHints(gc),
	})
}

func (c *Console) ruleEdit(gc *gin.Context) {
	ctx := gc.Request.Context()
	id, err := strconv.ParseInt(gc.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.RenderNotFound(gc, "rule not specified")
		return
	}
	r, err := c.rules.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, policy.ErrRuleNotExist) {
			c.RenderNotFound(gc, "rule not found")
			return
		}
		c.RenderError(gc, "look up rule", err)
		return
	}
	data := ruleEditFromPolicy(r)
	data.Tenants = c.listTenantHints(gc)
	c.Render(gc, "pages/rules/edit", data)
}

func ruleEditFromPolicy(r policy.Rule) ruleEditData {
	cfg := "{}"
	if len(r.ConfigJSON) > 0 {
		cfg = string(r.ConfigJSON)
	}
	return ruleEditData{
		ID: r.ID, Name: r.Name, Kind: r.Kind, Action: r.Action,
		Priority: r.Priority, Enabled: r.Enabled,
		Format: r.Format, TenantID: r.TenantID,
		Package: r.PackageLowerName, VersionPat: r.VersionPattern,
		ConfigJSON: cfg, ExpiresAt: r.ExpiresUnix,
		Errors: map[string]string{},
	}
}

// ruleUpsert handles POST /console/rules (create) and
// POST /console/rules/:id (update). Same code path either way; Upsert
// keys by Name.
func (c *Console) ruleUpsert(gc *gin.Context) {
	ctx := gc.Request.Context()
	id := auth.FromContext(gc)
	f := parseRuleForm(gc)

	// Validate.
	if f.Name == "" {
		f.Errors["name"] = "Required."
	} else if len(f.Name) > 80 {
		f.Errors["name"] = "Max 80 characters."
	}
	switch f.Kind {
	case "cooldown", "license", "blocklist":
		// known
	default:
		f.Errors["kind"] = "Unknown kind."
	}
	switch f.Action {
	case "warn", "quarantine", "deny":
		// known
	default:
		f.Errors["action"] = "Must be warn, quarantine, or deny."
	}
	if f.Priority < 1 || f.Priority > 10000 {
		f.Errors["priority"] = "1-10000."
	}
	if !json.Valid([]byte(f.ConfigJSON)) {
		f.Errors["config_json"] = "Must be valid JSON (or empty object {})."
	}
	if len(f.Errors) > 0 {
		// Re-populate the tenant-name hint list so the re-render
		// keeps showing it. Cheap; tenant list is small.
		f.Tenants = c.listTenantHints(gc)
		c.Render(gc, "pages/rules/edit", f)
		return
	}

	r := policy.Rule{
		ID: f.ID, Name: f.Name, Kind: f.Kind, Action: f.Action,
		TenantID:         f.TenantID,
		Format:           f.Format,
		PackageLowerName: strings.ToLower(strings.TrimSpace(f.Package)),
		VersionPattern:   strings.TrimSpace(f.VersionPat),
		ConfigJSON:       []byte(f.ConfigJSON),
		Priority:         f.Priority,
		Enabled:          f.Enabled,
		ExpiresUnix:      f.ExpiresAt,
	}
	if id != nil && id.User != nil {
		r.CreatedByUserID = id.User.ID
	}
	rid, err := c.rules.Upsert(ctx, r)
	if err != nil {
		c.RenderError(gc, "save rule", err)
		return
	}
	c.auditRule(gc, id, "rules.update", rid, r.Name, map[string]any{
		"action":   r.Action,
		"kind":     r.Kind,
		"enabled":  r.Enabled,
		"priority": r.Priority,
	})
	middleware.AddFlash(gc, middleware.FlashSuccess, fmt.Sprintf("Rule %q saved.", r.Name))
	gc.Redirect(http.StatusSeeOther, "/console/rules")
}

func (c *Console) ruleSetEnabled(gc *gin.Context) {
	ctx := gc.Request.Context()
	id := auth.FromContext(gc)
	rid, err := strconv.ParseInt(gc.Param("id"), 10, 64)
	if err != nil || rid <= 0 {
		c.RenderNotFound(gc, "rule not specified")
		return
	}
	enabled := strings.EqualFold(gc.PostForm("enabled"), "true")
	if err := c.rules.SetEnabled(ctx, rid, enabled); err != nil {
		c.RenderError(gc, "toggle rule", err)
		return
	}
	action := "rules.disable"
	verb := "disabled"
	if enabled {
		action = "rules.enable"
		verb = "enabled"
	}
	c.auditRule(gc, id, action, rid, "", nil)
	middleware.AddFlash(gc, middleware.FlashSuccess, fmt.Sprintf("Rule #%d %s.", rid, verb))
	gc.Redirect(http.StatusSeeOther, "/console/rules")
}

func (c *Console) ruleDelete(gc *gin.Context) {
	ctx := gc.Request.Context()
	id := auth.FromContext(gc)
	rid, err := strconv.ParseInt(gc.Param("id"), 10, 64)
	if err != nil || rid <= 0 {
		c.RenderNotFound(gc, "rule not specified")
		return
	}
	r, _ := c.rules.GetByID(ctx, rid)
	if err := c.rules.Delete(ctx, rid); err != nil {
		c.RenderError(gc, "delete rule", err)
		return
	}
	c.auditRule(gc, id, "rules.delete", rid, r.Name, nil)
	middleware.AddFlash(gc, middleware.FlashSuccess, fmt.Sprintf("Rule %q deleted.", r.Name))
	gc.Redirect(http.StatusSeeOther, "/console/rules")
}

// parseRuleForm pulls the form into a ruleEditData. Defaults sane.
func parseRuleForm(gc *gin.Context) ruleEditData {
	f := ruleEditData{
		Name:       strings.TrimSpace(gc.PostForm("name")),
		Kind:       strings.TrimSpace(gc.PostForm("kind")),
		Action:     strings.TrimSpace(gc.PostForm("action")),
		Format:     strings.TrimSpace(gc.PostForm("format")),
		Package:    strings.TrimSpace(gc.PostForm("package")),
		VersionPat: strings.TrimSpace(gc.PostForm("version_pattern")),
		ConfigJSON: strings.TrimSpace(gc.PostForm("config_json")),
		Enabled:    strings.EqualFold(gc.PostForm("enabled"), "on"),
		Errors:     map[string]string{},
	}
	if f.ConfigJSON == "" {
		f.ConfigJSON = "{}"
	}
	if v := strings.TrimSpace(gc.PostForm("priority")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.Priority = n
		}
	}
	if f.Priority == 0 {
		f.Priority = 100
	}
	if v := strings.TrimSpace(gc.PostForm("tenant_id")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			f.TenantID = n
		}
	}
	if v := strings.TrimSpace(gc.PostForm("expires_unix")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			f.ExpiresAt = n
		}
	}
	if v := strings.TrimSpace(gc.PostForm("id")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			f.ID = n
		}
	}
	return f
}

func (c *Console) auditRule(gc *gin.Context, id *auth.Identity, action string, ruleID int64, name string, extra map[string]any) {
	if c.audit == nil {
		return
	}
	if extra == nil {
		extra = map[string]any{}
	}
	if name != "" {
		extra["rule_name"] = name
	}
	ev := audit.Event{
		Action:     action,
		ActorKind:  "session",
		RequestID:  middleware.RequestIDFrom(gc),
		RemoteAddr: middleware.ClientIPFor(gc.Request, c.cfg.TrustedProxies),
		UserAgent:  gc.Request.UserAgent(),
		RuleID:     ruleID,
		Extra:      extra,
	}
	if id != nil && id.User != nil {
		ev.UserID = id.User.ID
	}
	c.audit.Log(ev)
}
