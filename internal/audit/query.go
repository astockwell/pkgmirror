package audit

import (
	"context"
	"database/sql"
	"strings"
)

// Query is a read filter for ListEvents.
type Query struct {
	TenantID int64  // 0 = all tenants
	Format   string // "" = any
	Package  string // "" = any
	Version  string // "" = any
	UserID   int64  // 0 = any
	Action   string // "" = any (e.g. "ingest")
	Decision string // "" = any
	Limit    int    // 0 -> 100; capped at 1000
	Since    int64  // 0 = no lower bound (Unix seconds)
}

// ListEvents queries the audit_log table with the supplied filter,
// returning newest-first.
func ListEvents(ctx context.Context, db *sql.DB, q Query) ([]Event, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	var (
		where []string
		args  []any
	)
	if q.TenantID != 0 {
		where = append(where, "tenant_id = ?")
		args = append(args, q.TenantID)
	}
	if q.Format != "" {
		where = append(where, "format = ?")
		args = append(args, q.Format)
	}
	if q.Package != "" {
		where = append(where, "package = ?")
		args = append(args, q.Package)
	}
	if q.Version != "" {
		where = append(where, "version = ?")
		args = append(args, q.Version)
	}
	if q.UserID != 0 {
		where = append(where, "actor_user_id = ?")
		args = append(args, q.UserID)
	}
	if q.Action != "" {
		where = append(where, "action = ?")
		args = append(args, q.Action)
	}
	if q.Decision != "" {
		where = append(where, "decision = ?")
		args = append(args, q.Decision)
	}
	if q.Since != 0 {
		where = append(where, "created_unix >= ?")
		args = append(args, q.Since)
	}

	sqlText := `SELECT created_unix, actor_user_id, actor_token_id, request_id,
                       remote_addr, user_agent, tenant_id, action,
                       format, package, version, filename,
                       decision, rule_id, reason, extra_json
                  FROM audit_log`
	if len(where) > 0 {
		sqlText += " WHERE " + strings.Join(where, " AND ")
	}
	sqlText += " ORDER BY created_unix DESC LIMIT ?"
	args = append(args, limit)

	rows, err := db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var (
			ev                                                  Event
			userID, tokenID, tenantID, ruleID                   sql.NullInt64
			requestID, remote, ua                               sql.NullString
			format, pkg, version, filename, decision, reason    sql.NullString
			extra                                               sql.NullString
		)
		if err := rows.Scan(
			&ev.CreatedUnix, &userID, &tokenID, &requestID,
			&remote, &ua, &tenantID, &ev.Action,
			&format, &pkg, &version, &filename,
			&decision, &ruleID, &reason, &extra,
		); err != nil {
			return nil, err
		}
		if userID.Valid {
			ev.UserID = userID.Int64
		}
		if tokenID.Valid {
			ev.TokenID = tokenID.Int64
		}
		if tenantID.Valid {
			ev.TenantID = tenantID.Int64
		}
		if ruleID.Valid {
			ev.RuleID = ruleID.Int64
		}
		ev.RequestID = requestID.String
		ev.RemoteAddr = remote.String
		ev.UserAgent = ua.String
		ev.Format = format.String
		ev.Package = pkg.String
		ev.Version = version.String
		ev.Filename = filename.String
		ev.Decision = decision.String
		ev.Reason = reason.String
		out = append(out, ev)
	}
	return out, rows.Err()
}
