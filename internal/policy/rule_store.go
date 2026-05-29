package policy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// RuleStore reads/writes the policy_rules table.
type RuleStore struct{ DB *sql.DB }

// NewRuleStore wraps a *sql.DB.
func NewRuleStore(db *sql.DB) *RuleStore { return &RuleStore{DB: db} }

// ErrRuleNotExist is returned when a rule lookup turns up nothing.
var ErrRuleNotExist = errors.New("policy rule does not exist")

const ruleColumns = `id, name, kind, tenant_id, format, package_lower_name,
        version_pattern, action, config_json, priority, enabled,
        created_unix, created_by_user_id, expires_unix`

func scanRule(scanner interface {
	Scan(dest ...any) error
}) (Rule, error) {
	var (
		r          Rule
		tenantID   sql.NullInt64
		format     sql.NullString
		pkgName    sql.NullString
		versPat    sql.NullString
		configJSON sql.NullString
		createdBy  sql.NullInt64
		enabled    int
	)
	if err := scanner.Scan(
		&r.ID, &r.Name, &r.Kind, &tenantID, &format, &pkgName,
		&versPat, &r.Action, &configJSON, &r.Priority, &enabled,
		&r.CreatedUnix, &createdBy, &r.ExpiresUnix,
	); err != nil {
		return r, err
	}
	if tenantID.Valid {
		r.TenantID = tenantID.Int64
	}
	if format.Valid {
		r.Format = format.String
	}
	if pkgName.Valid {
		r.PackageLowerName = pkgName.String
	}
	if versPat.Valid {
		r.VersionPattern = versPat.String
	}
	if configJSON.Valid {
		r.ConfigJSON = []byte(configJSON.String)
	}
	if createdBy.Valid {
		r.CreatedByUserID = createdBy.Int64
	}
	r.Enabled = enabled != 0
	return r, nil
}

// ListEnabled returns every rule with enabled = 1.
func (s *RuleStore) ListEnabled(ctx context.Context) ([]Rule, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT `+ruleColumns+` FROM policy_rules WHERE enabled = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListAll returns every rule, enabled or not, ordered by priority then
// name. Used by the web console rules page (operators need to see
// disabled rules to re-enable them).
func (s *RuleStore) ListAll(ctx context.Context) ([]Rule, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT `+ruleColumns+` FROM policy_rules ORDER BY priority ASC, name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetByID looks up a single rule by ID. Used by the web console
// rules-detail/edit page.
func (s *RuleStore) GetByID(ctx context.Context, id int64) (Rule, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT `+ruleColumns+` FROM policy_rules WHERE id = ? LIMIT 1`, id)
	r, err := scanRule(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Rule{}, ErrRuleNotExist
		}
		return Rule{}, err
	}
	return r, nil
}

// GetByName looks up a single rule by its human label. Rule names should
// be unique by convention (the YAML loader relies on it).
func (s *RuleStore) GetByName(ctx context.Context, name string) (Rule, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT `+ruleColumns+` FROM policy_rules WHERE name = ? LIMIT 1`, name)
	r, err := scanRule(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Rule{}, ErrRuleNotExist
		}
		return Rule{}, err
	}
	return r, nil
}

// Upsert inserts or updates a rule keyed by name. Returns the rule's ID.
func (s *RuleStore) Upsert(ctx context.Context, r Rule) (int64, error) {
	if r.Name == "" || r.Kind == "" || r.Action == "" {
		return 0, fmt.Errorf("rule.Upsert: name, kind, and action are required")
	}
	if len(r.ConfigJSON) == 0 {
		r.ConfigJSON = []byte("{}")
	}
	if r.CreatedUnix == 0 {
		r.CreatedUnix = time.Now().Unix()
	}
	if r.Priority == 0 {
		r.Priority = 100
	}

	existing, err := s.GetByName(ctx, r.Name)
	if err != nil && !errors.Is(err, ErrRuleNotExist) {
		return 0, err
	}
	enabled := 0
	if r.Enabled {
		enabled = 1
	}

	if errors.Is(err, ErrRuleNotExist) {
		res, ierr := s.DB.ExecContext(ctx,
			`INSERT INTO policy_rules
			   (name, kind, tenant_id, format, package_lower_name,
			    version_pattern, action, config_json, priority, enabled,
			    created_unix, created_by_user_id, expires_unix)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.Name, r.Kind, nullInt(r.TenantID), nullString(r.Format),
			nullString(r.PackageLowerName), nullString(r.VersionPattern),
			r.Action, string(r.ConfigJSON), r.Priority, enabled,
			r.CreatedUnix, nullInt(r.CreatedByUserID), r.ExpiresUnix,
		)
		if ierr != nil {
			return 0, fmt.Errorf("insert rule: %w", ierr)
		}
		id, _ := res.LastInsertId()
		return id, nil
	}

	_, uerr := s.DB.ExecContext(ctx,
		`UPDATE policy_rules
		    SET kind = ?, tenant_id = ?, format = ?, package_lower_name = ?,
		        version_pattern = ?, action = ?, config_json = ?, priority = ?,
		        enabled = ?, expires_unix = ?
		  WHERE id = ?`,
		r.Kind, nullInt(r.TenantID), nullString(r.Format),
		nullString(r.PackageLowerName), nullString(r.VersionPattern),
		r.Action, string(r.ConfigJSON), r.Priority, enabled, r.ExpiresUnix,
		existing.ID,
	)
	if uerr != nil {
		return 0, fmt.Errorf("update rule: %w", uerr)
	}
	return existing.ID, nil
}

// SetEnabled flips a rule's enabled bit by ID.
func (s *RuleStore) SetEnabled(ctx context.Context, id int64, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := s.DB.ExecContext(ctx,
		`UPDATE policy_rules SET enabled = ? WHERE id = ?`, v, id)
	return err
}

// Delete removes a rule by ID.
func (s *RuleStore) Delete(ctx context.Context, id int64) error {
	_, err := s.DB.ExecContext(ctx,
		`DELETE FROM policy_rules WHERE id = ?`, id)
	return err
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return strings.TrimSpace(s)
}

func nullInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}
