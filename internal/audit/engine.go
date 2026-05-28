package audit

import (
	"context"

	"github.com/astockwell/pkgmirror/internal/policy"
)

// TenantAuditQuerier abstracts the tenants.Store dependency so this
// package doesn't have to import the larger tenants package.
type TenantAuditQuerier interface {
	// AuditReadsEnabled reports whether successful read events should be
	// logged for the given tenant. Used to gate the high-volume "successful
	// download" stream behind explicit opt-in.
	AuditReadsEnabled(ctx context.Context, tenantID int64) bool
}

// WrapEngine returns a policy.Engine that delegates to inner and emits
// one audit event per Evaluate call, with this default filter:
//
//	- ActionIngest:           always logged (success or failure).
//	- ActionRead, non-Allow:  always logged.
//	- ActionRead, Allow:      logged only when the tenant opts in via
//	                          tenants.audit_reads = 1.
//
// The audit write is non-blocking — overflow is dropped and visible via
// logger.Drops().
func WrapEngine(inner policy.Engine, logger Logger, q TenantAuditQuerier) policy.Engine {
	return &wrappedEngine{inner: inner, logger: logger, q: q}
}

type wrappedEngine struct {
	inner  policy.Engine
	logger Logger
	q      TenantAuditQuerier
}

func (e *wrappedEngine) Evaluate(ctx context.Context, s policy.Subject, a policy.Action) policy.Result {
	r := e.inner.Evaluate(ctx, s, a)
	if !e.shouldLog(ctx, a, r, s.TenantID) {
		return r
	}
	actor := policy.ActorFromContext(ctx)
	e.logger.Log(Event{
		UserID:     actor.UserID,
		TokenID:    actor.TokenID,
		RequestID:  actor.RequestID,
		RemoteAddr: actor.RemoteAddr,
		UserAgent:  actor.UserAgent,
		TenantID:   s.TenantID,
		Action:     a.String(),
		Format:     s.Format,
		Package:    s.Package,
		Version:    s.Version,
		Filename:   s.Filename,
		Decision:   r.Decision.String(),
		RuleID:     r.RuleID,
		Reason:     r.Reason,
	})
	return r
}

func (e *wrappedEngine) shouldLog(ctx context.Context, a policy.Action, r policy.Result, tenantID int64) bool {
	if a == policy.ActionIngest {
		return true
	}
	if r.Decision != policy.Allow {
		return true
	}
	// ActionRead + Allow: opt-in per tenant.
	if e.q == nil {
		return false
	}
	return e.q.AuditReadsEnabled(ctx, tenantID)
}
