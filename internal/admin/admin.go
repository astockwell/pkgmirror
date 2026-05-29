// Package admin exposes operator-facing HTTP endpoints for managing
// supply-chain policy rules, browsing the audit log, and promoting or
// rejecting quarantined versions.
//
// Every endpoint requires the caller's token to carry the "admin" scope
// AND the caller user to have is_admin=1; the bootstrap admin token
// minted on first boot satisfies both. See docs/auth.md.
package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/astockwell/pkgmirror/internal/audit"
	"github.com/astockwell/pkgmirror/internal/auth"
	"github.com/astockwell/pkgmirror/internal/models"
	"github.com/astockwell/pkgmirror/internal/policy"

	"github.com/gin-gonic/gin"
)

// Handler exposes the admin API.
type Handler struct {
	Models *models.Store
	Rules  *policy.RuleStore
	Audit  audit.Logger // for emitting rule_create / promote_quarantined events
}

// Register mounts the admin routes on r.  All routes are gated by
// requireSystemAdmin which calls auth.RequireSystemAdmin under the hood.
//
// Optional middleware (typically the auth.Middleware that populates
// *auth.Identity into the gin context) is prepended onto the /admin
// group ahead of the system-admin gate. Pass it from server.New when
// the engine itself no longer applies auth globally.
func (h *Handler) Register(r *gin.Engine, middleware ...gin.HandlerFunc) {
	handlers := append([]gin.HandlerFunc{}, middleware...)
	handlers = append(handlers, h.requireSystemAdmin)
	g := r.Group("/admin", handlers...)

	g.GET("/rules", h.listRules)
	g.POST("/rules", h.upsertRule)
	g.POST("/rules/:id/enabled", h.setRuleEnabled)
	g.DELETE("/rules/:id", h.deleteRule)

	g.GET("/audit", h.listAudit)

	g.GET("/quarantine", h.listQuarantine)
	g.POST("/quarantine/:version_id/promote", h.promoteQuarantine)
	g.POST("/quarantine/:version_id/reject", h.rejectQuarantine)
}

// requireSystemAdmin gates every admin route.
func (h *Handler) requireSystemAdmin(c *gin.Context) {
	id := auth.FromContext(c)
	if id == nil || !id.IsSystemAdmin() {
		c.Header("WWW-Authenticate", `Basic realm="pkgmirror admin"`)
		c.String(http.StatusForbidden, "admin scope required")
		c.Abort()
		return
	}
	c.Next()
}

// --- rules ---

type ruleDTO struct {
	ID               int64  `json:"id"`
	Name             string `json:"name"`
	Kind             string `json:"kind"`
	TenantID         int64  `json:"tenant_id,omitempty"`
	Format           string `json:"format,omitempty"`
	PackageLowerName string `json:"package,omitempty"`
	VersionPattern   string `json:"version_pattern,omitempty"`
	Action           string `json:"action"`
	Config           any    `json:"config,omitempty"`
	Priority         int    `json:"priority,omitempty"`
	Enabled          bool   `json:"enabled"`
	CreatedUnix      int64  `json:"created_unix"`
	ExpiresUnix      int64  `json:"expires_unix,omitempty"`
}

func ruleToDTO(r policy.Rule) ruleDTO {
	d := ruleDTO{
		ID: r.ID, Name: r.Name, Kind: r.Kind,
		TenantID: r.TenantID, Format: r.Format,
		PackageLowerName: r.PackageLowerName, VersionPattern: r.VersionPattern,
		Action: r.Action, Priority: r.Priority,
		Enabled: r.Enabled, CreatedUnix: r.CreatedUnix, ExpiresUnix: r.ExpiresUnix,
	}
	if len(r.ConfigJSON) > 0 {
		var cfg any
		if err := json.Unmarshal(r.ConfigJSON, &cfg); err == nil {
			d.Config = cfg
		}
	}
	return d
}

func (h *Handler) listRules(c *gin.Context) {
	rs, err := h.Rules.ListEnabled(c.Request.Context())
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	out := make([]ruleDTO, 0, len(rs))
	for _, r := range rs {
		out = append(out, ruleToDTO(r))
	}
	c.JSON(http.StatusOK, gin.H{"rules": out})
}

type upsertRuleReq struct {
	Name             string `json:"name"`
	Kind             string `json:"kind"`
	TenantID         int64  `json:"tenant_id,omitempty"`
	Format           string `json:"format,omitempty"`
	PackageLowerName string `json:"package,omitempty"`
	VersionPattern   string `json:"version_pattern,omitempty"`
	Action           string `json:"action"`
	Config           any    `json:"config,omitempty"`
	Priority         int    `json:"priority,omitempty"`
	Enabled          *bool  `json:"enabled,omitempty"`
	ExpiresUnix      int64  `json:"expires_unix,omitempty"`
}

func (h *Handler) upsertRule(c *gin.Context) {
	var req upsertRuleReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.String(http.StatusBadRequest, "bad json: %v", err)
		return
	}
	if req.Name == "" || req.Kind == "" || req.Action == "" {
		c.String(http.StatusBadRequest, "name, kind, action are required")
		return
	}

	var configJSON json.RawMessage
	if req.Config != nil {
		b, err := json.Marshal(req.Config)
		if err != nil {
			c.String(http.StatusBadRequest, "encode config: %v", err)
			return
		}
		configJSON = b
	} else {
		configJSON = json.RawMessage(`{}`)
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	r := policy.Rule{
		Name:             req.Name,
		Kind:             req.Kind,
		TenantID:         req.TenantID,
		Format:           req.Format,
		PackageLowerName: req.PackageLowerName,
		VersionPattern:   req.VersionPattern,
		Action:           req.Action,
		ConfigJSON:       configJSON,
		Priority:         req.Priority,
		Enabled:          enabled,
		ExpiresUnix:      req.ExpiresUnix,
	}
	id, err := h.Rules.Upsert(c.Request.Context(), r)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	h.emit(c, audit.Event{
		Action:  "rule_upsert",
		Reason:  req.Name,
		RuleID:  id,
		Extra:   map[string]any{"kind": req.Kind, "action": req.Action},
	})
	c.JSON(http.StatusOK, gin.H{"id": id})
}

func (h *Handler) setRuleEnabled(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.String(http.StatusBadRequest, "id: %v", err)
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.String(http.StatusBadRequest, "bad json: %v", err)
		return
	}
	if err := h.Rules.SetEnabled(c.Request.Context(), id, req.Enabled); err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	h.emit(c, audit.Event{Action: "rule_set_enabled", RuleID: id, Extra: map[string]any{"enabled": req.Enabled}})
	c.Status(http.StatusNoContent)
}

func (h *Handler) deleteRule(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.String(http.StatusBadRequest, "id: %v", err)
		return
	}
	// Emit the audit event BEFORE the delete so the row's rule_id FK
	// still points at a real row. The ON DELETE SET NULL on audit_log
	// will then NULL it out, which is the semantically correct "the
	// referenced rule no longer exists" state.
	h.emit(c, audit.Event{Action: "rule_delete", RuleID: id})
	if err := h.Rules.Delete(c.Request.Context(), id); err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	c.Status(http.StatusNoContent)
}

// --- audit ---

func (h *Handler) listAudit(c *gin.Context) {
	q := audit.Query{
		Format:   c.Query("format"),
		Package:  c.Query("package"),
		Version:  c.Query("version"),
		Action:   c.Query("action"),
		Decision: c.Query("decision"),
	}
	if v := c.Query("tenant_id"); v != "" {
		q.TenantID, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := c.Query("user_id"); v != "" {
		q.UserID, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := c.Query("limit"); v != "" {
		q.Limit, _ = strconv.Atoi(v)
	}
	if v := c.Query("since"); v != "" {
		q.Since, _ = strconv.ParseInt(v, 10, 64)
	}
	events, err := audit.ListEvents(c.Request.Context(), h.Models.DB, q)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"events": events})
}

// --- quarantine ---

func (h *Handler) listQuarantine(c *gin.Context) {
	var tenantID int64
	if v := c.Query("tenant_id"); v != "" {
		tenantID, _ = strconv.ParseInt(v, 10, 64)
	}
	rows, err := h.Models.ListQuarantined(c.Request.Context(), tenantID)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"quarantined": rows})
}

func (h *Handler) promoteQuarantine(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("version_id"), 10, 64)
	if err != nil {
		c.String(http.StatusBadRequest, "version_id: %v", err)
		return
	}
	if err := h.Models.PromoteVersion(c.Request.Context(), id); err != nil {
		if errors.Is(err, models.ErrVersionNotExist) {
			c.String(http.StatusNotFound, "no such version")
			return
		}
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	h.emit(c, audit.Event{
		Action: "promote_quarantined",
		Reason: c.Query("reason"),
		Extra:  map[string]any{"version_id": id},
	})
	c.Status(http.StatusNoContent)
}

func (h *Handler) rejectQuarantine(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("version_id"), 10, 64)
	if err != nil {
		c.String(http.StatusBadRequest, "version_id: %v", err)
		return
	}
	// Reject = update quarantine_reason with operator note; bytes stay
	// in storage for forensics. Deleting is destructive and intentionally
	// not exposed here.
	reason := c.Query("reason")
	if reason == "" {
		reason = "rejected by admin"
	}
	if err := h.Models.QuarantineVersion(c.Request.Context(), id, 0, "REJECTED: "+reason); err != nil {
		if errors.Is(err, models.ErrVersionNotExist) {
			c.String(http.StatusNotFound, "no such version")
			return
		}
		c.String(http.StatusInternalServerError, "%v", err)
		return
	}
	h.emit(c, audit.Event{
		Action: "reject_quarantined",
		Reason: reason,
		Extra:  map[string]any{"version_id": id},
	})
	c.Status(http.StatusNoContent)
}

// --- helpers ---

func (h *Handler) emit(c *gin.Context, e audit.Event) {
	if h.Audit == nil {
		return
	}
	actor := policy.ActorFromContext(c.Request.Context())
	e.UserID = actor.UserID
	e.TokenID = actor.TokenID
	e.RequestID = actor.RequestID
	e.RemoteAddr = actor.RemoteAddr
	e.UserAgent = actor.UserAgent
	h.Audit.Log(e)
}
