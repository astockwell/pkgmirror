// Package models contains database access for packages, versions, files,
// blobs, and properties.
package models

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Type identifies a package format (e.g. "go", "npm").
type Type string

const (
	TypeGo        Type = "go"
	TypePyPI      Type = "pypi"
	TypeNpm       Type = "npm"
	TypeRubyGems  Type = "rubygems"
	TypeContainer Type = "container"
	TypeGeneric   Type = "generic"
	TypeAlpine    Type = "alpine"
	TypeMaven     Type = "maven"
	TypeDebian    Type = "debian"
	TypeRPM       Type = "rpm"
)

// PropertyRefType identifies the entity a property is attached to.
type PropertyRefType int

const (
	PropertyRefPackage PropertyRefType = 0
	PropertyRefVersion PropertyRefType = 1
	PropertyRefFile    PropertyRefType = 2
)

// Sentinel errors.
var (
	ErrPackageNotExist         = errors.New("package does not exist")
	ErrVersionNotExist         = errors.New("package version does not exist")
	ErrFileNotExist            = errors.New("package file does not exist")
	ErrBlobNotExist            = errors.New("package blob does not exist")
	ErrDuplicatePackageVersion = errors.New("package version already exists")
	ErrDuplicatePackageFile    = errors.New("package file already exists")
)

// Package represents a logical package (a name within a format) belonging
// to a tenant.
type Package struct {
	ID          int64
	TenantID    int64
	Type        Type
	Name        string
	LowerName   string
	CreatedUnix int64
}

// Version represents a specific version of a package.
type Version struct {
	ID           int64
	PackageID    int64
	Version      string
	LowerVersion string
	MetadataJSON string
	CreatedUnix  int64

	// License is the SPDX identifier, populated at ingest by per-format
	// extractors. Empty means "unknown".
	License sql.NullString
	// QuarantineReason, when non-NULL, marks this version as hidden from
	// indices and serves. Admin tooling clears it to promote the version.
	QuarantineReason sql.NullString
	// QuarantinedByRuleID identifies which policy rule triggered the
	// quarantine, when one did.
	QuarantinedByRuleID sql.NullInt64
}

// IsQuarantined reports whether this version is currently hidden.
func (v *Version) IsQuarantined() bool { return v != nil && v.QuarantineReason.Valid }

// Blob represents the bytes of a stored object.
type Blob struct {
	ID          int64
	Size        int64
	HashMD5     string
	HashSHA1    string
	HashSHA256  string
	HashSHA512  string
	CreatedUnix int64
}

// File represents a file attached to a version, pointing at a blob.
type File struct {
	ID          int64
	VersionID   int64
	BlobID      int64
	Name        string
	LowerName   string
	IsLead      bool
	CreatedUnix int64
}

// Property is a key/value attached to a package, version, or file.
type Property struct {
	ID      int64
	RefType PropertyRefType
	RefID   int64
	Name    string
	Value   string
}

// Store wraps a *sql.DB and provides typed operations.
type Store struct{ DB *sql.DB }

// New returns a Store wrapping db.
func New(db *sql.DB) *Store { return &Store{DB: db} }

// ----- Packages -----

// GetOrCreatePackage returns the package within the given tenant matching
// (type, name), creating it if needed.
func (s *Store) GetOrCreatePackage(ctx context.Context, tenantID int64, t Type, name string) (*Package, error) {
	return s.GetOrCreatePackageWithLookup(ctx, tenantID, t, name, strings.ToLower(name))
}

// GetOrCreatePackageWithLookup is GetOrCreatePackage with an explicit
// lookup key. Used by formats with non-trivial name canonicalization
// (e.g. PyPI's PEP 503 normalization), where the display name should
// preserve the user-supplied form but lookups must match the canonical
// key. The lookup key MUST already be lowercased / canonicalized.
func (s *Store) GetOrCreatePackageWithLookup(ctx context.Context, tenantID int64, t Type, displayName, lookupName string) (*Package, error) {
	if p, err := s.GetPackageByLookup(ctx, tenantID, t, lookupName); err == nil {
		return p, nil
	} else if !errors.Is(err, ErrPackageNotExist) {
		return nil, err
	}

	now := time.Now().Unix()
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO packages (tenant_id, type, name, lower_name, created_unix)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(tenant_id, type, lower_name) DO NOTHING`,
		tenantID, string(t), displayName, lookupName, now)
	if err != nil {
		return nil, fmt.Errorf("insert package: %w", err)
	}
	if id, _ := res.LastInsertId(); id > 0 {
		return &Package{ID: id, TenantID: tenantID, Type: t, Name: displayName, LowerName: lookupName, CreatedUnix: now}, nil
	}
	return s.GetPackageByLookup(ctx, tenantID, t, lookupName)
}

// GetPackage looks up a package by (tenant, type, name) case-insensitively.
func (s *Store) GetPackage(ctx context.Context, tenantID int64, t Type, name string) (*Package, error) {
	return s.GetPackageByLookup(ctx, tenantID, t, strings.ToLower(name))
}

// GetPackageByLookup looks up a package by its canonical lookup key
// (already lowercased / format-normalized).
func (s *Store) GetPackageByLookup(ctx context.Context, tenantID int64, t Type, lookupName string) (*Package, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT id, tenant_id, type, name, lower_name, created_unix
		   FROM packages WHERE tenant_id = ? AND type = ? AND lower_name = ?`,
		tenantID, string(t), lookupName)
	p := &Package{}
	var typ string
	if err := row.Scan(&p.ID, &p.TenantID, &typ, &p.Name, &p.LowerName, &p.CreatedUnix); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPackageNotExist
		}
		return nil, err
	}
	p.Type = Type(typ)
	return p, nil
}

// ListPackages returns packages within a tenant. If tenantID == 0, all
// tenants are included (admin-only callers should use this). If t == ""
// all types are returned.
func (s *Store) ListPackages(ctx context.Context, tenantID int64, t Type) ([]*Package, error) {
	var (
		rows *sql.Rows
		err  error
	)
	switch {
	case tenantID == 0 && t == "":
		rows, err = s.DB.QueryContext(ctx,
			`SELECT id, tenant_id, type, name, lower_name, created_unix
			   FROM packages ORDER BY tenant_id, lower_name ASC`)
	case tenantID == 0:
		rows, err = s.DB.QueryContext(ctx,
			`SELECT id, tenant_id, type, name, lower_name, created_unix
			   FROM packages WHERE type = ? ORDER BY tenant_id, lower_name ASC`, string(t))
	case t == "":
		rows, err = s.DB.QueryContext(ctx,
			`SELECT id, tenant_id, type, name, lower_name, created_unix
			   FROM packages WHERE tenant_id = ? ORDER BY lower_name ASC`, tenantID)
	default:
		rows, err = s.DB.QueryContext(ctx,
			`SELECT id, tenant_id, type, name, lower_name, created_unix
			   FROM packages WHERE tenant_id = ? AND type = ? ORDER BY lower_name ASC`,
			tenantID, string(t))
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Package
	for rows.Next() {
		p := &Package{}
		var typ string
		if err := rows.Scan(&p.ID, &p.TenantID, &typ, &p.Name, &p.LowerName, &p.CreatedUnix); err != nil {
			return nil, err
		}
		p.Type = Type(typ)
		out = append(out, p)
	}
	return out, rows.Err()
}

// ----- Versions -----

// CreateVersion inserts a new version. Returns ErrDuplicatePackageVersion
// if (package_id, lower_version) already exists.
func (s *Store) CreateVersion(ctx context.Context, packageID int64, version, metadataJSON string) (*Version, error) {
	if metadataJSON == "" {
		metadataJSON = "{}"
	}
	lower := strings.ToLower(version)
	now := time.Now().Unix()
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO package_versions (package_id, version, lower_version, metadata_json, created_unix)
		 VALUES (?, ?, ?, ?, ?)`,
		packageID, version, lower, metadataJSON, now)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicatePackageVersion
		}
		return nil, fmt.Errorf("insert version: %w", err)
	}
	id, _ := res.LastInsertId()
	return &Version{
		ID:           id,
		PackageID:    packageID,
		Version:      version,
		LowerVersion: lower,
		MetadataJSON: metadataJSON,
		CreatedUnix:  now,
	}, nil
}

// versionColumns lists every column read by version-scanning queries.
// Keep the order in sync with scanVersion.
const versionColumns = `id, package_id, version, lower_version, metadata_json,
        created_unix, license, quarantine_reason, quarantined_by_rule_id`

func scanVersion(scanner interface {
	Scan(dest ...any) error
}) (*Version, error) {
	v := &Version{}
	if err := scanner.Scan(
		&v.ID, &v.PackageID, &v.Version, &v.LowerVersion, &v.MetadataJSON,
		&v.CreatedUnix, &v.License, &v.QuarantineReason, &v.QuarantinedByRuleID,
	); err != nil {
		return nil, err
	}
	return v, nil
}

// GetVersion looks up a (package_id, version) pair.
func (s *Store) GetVersion(ctx context.Context, packageID int64, version string) (*Version, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT `+versionColumns+`
		   FROM package_versions WHERE package_id = ? AND lower_version = ?`,
		packageID, strings.ToLower(version))
	v, err := scanVersion(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrVersionNotExist
		}
		return nil, err
	}
	return v, nil
}

// ListVersions returns all versions of a package, sorted oldest-first by
// creation time.
func (s *Store) ListVersions(ctx context.Context, packageID int64) ([]*Version, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT `+versionColumns+`
		   FROM package_versions WHERE package_id = ? ORDER BY created_unix ASC`,
		packageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Version
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetLatestVersion returns the most-recently-created version of a package.
func (s *Store) GetLatestVersion(ctx context.Context, packageID int64) (*Version, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT `+versionColumns+`
		   FROM package_versions WHERE package_id = ?
		   ORDER BY created_unix DESC LIMIT 1`,
		packageID)
	v, err := scanVersion(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrVersionNotExist
		}
		return nil, err
	}
	return v, nil
}

// QuarantineVersion marks a version as quarantined: stored but hidden.
// ruleID may be 0 if the quarantine is operator-initiated.
func (s *Store) QuarantineVersion(ctx context.Context, versionID, ruleID int64, reason string) error {
	var ruleArg any
	if ruleID != 0 {
		ruleArg = ruleID
	}
	res, err := s.DB.ExecContext(ctx,
		`UPDATE package_versions
		    SET quarantine_reason = ?, quarantined_by_rule_id = ?
		  WHERE id = ?`, reason, ruleArg, versionID)
	if err != nil {
		return fmt.Errorf("quarantine version: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrVersionNotExist
	}
	return nil
}

// PromoteVersion clears quarantine on a version.
func (s *Store) PromoteVersion(ctx context.Context, versionID int64) error {
	res, err := s.DB.ExecContext(ctx,
		`UPDATE package_versions
		    SET quarantine_reason = NULL, quarantined_by_rule_id = NULL
		  WHERE id = ?`, versionID)
	if err != nil {
		return fmt.Errorf("promote version: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrVersionNotExist
	}
	return nil
}

// SetLicense records the SPDX license expression for a version.
func (s *Store) SetLicense(ctx context.Context, versionID int64, license string) error {
	var arg any
	if license != "" {
		arg = license
	}
	_, err := s.DB.ExecContext(ctx,
		`UPDATE package_versions SET license = ? WHERE id = ?`, arg, versionID)
	return err
}

// UpdateVersionMetadata replaces the metadata_json column for one
// version. Used by formats (Maven, in particular) where the
// metadata-bearing file may be uploaded after the first artifact for
// the version, so the version row exists with empty metadata at the
// time we receive the canonical metadata source.
func (s *Store) UpdateVersionMetadata(ctx context.Context, versionID int64, metadataJSON string) error {
	res, err := s.DB.ExecContext(ctx,
		`UPDATE package_versions SET metadata_json = ? WHERE id = ?`,
		metadataJSON, versionID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrVersionNotExist
	}
	return nil
}

// QuarantinedView is a denormalized row joining packages + versions used
// by the admin quarantine listing.
type QuarantinedView struct {
	VersionID    int64
	PackageID    int64
	TenantID     int64
	Type         Type
	PackageName  string
	Version      string
	CreatedUnix  int64
	Reason       string
	RuleID       int64
}

// ListQuarantined returns every quarantined version, optionally scoped
// to a tenant (tenantID = 0 means "all").
func (s *Store) ListQuarantined(ctx context.Context, tenantID int64) ([]QuarantinedView, error) {
	var (
		rows *sql.Rows
		err  error
	)
	q := `SELECT v.id, v.package_id, p.tenant_id, p.type, p.name, v.version,
	             v.created_unix, COALESCE(v.quarantine_reason, ''),
	             COALESCE(v.quarantined_by_rule_id, 0)
	        FROM package_versions v
	        JOIN packages p ON p.id = v.package_id
	       WHERE v.quarantine_reason IS NOT NULL`
	if tenantID != 0 {
		q += " AND p.tenant_id = ?"
		rows, err = s.DB.QueryContext(ctx, q+" ORDER BY v.created_unix DESC", tenantID)
	} else {
		rows, err = s.DB.QueryContext(ctx, q+" ORDER BY v.created_unix DESC")
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuarantinedView
	for rows.Next() {
		var v QuarantinedView
		var typ string
		if err := rows.Scan(&v.VersionID, &v.PackageID, &v.TenantID, &typ,
			&v.PackageName, &v.Version, &v.CreatedUnix, &v.Reason, &v.RuleID); err != nil {
			return nil, err
		}
		v.Type = Type(typ)
		out = append(out, v)
	}
	return out, rows.Err()
}

// ----- Blobs -----

// GetOrCreateBlob inserts a blob if one with the same SHA-256 doesn't exist.
func (s *Store) GetOrCreateBlob(ctx context.Context, b Blob) (*Blob, error) {
	if existing, err := s.GetBlobBySHA256(ctx, b.HashSHA256); err == nil {
		return existing, nil
	} else if !errors.Is(err, ErrBlobNotExist) {
		return nil, err
	}

	now := time.Now().Unix()
	b.CreatedUnix = now
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO package_blobs (size, hash_md5, hash_sha1, hash_sha256, hash_sha512, created_unix)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(hash_sha256) DO NOTHING`,
		b.Size, b.HashMD5, b.HashSHA1, b.HashSHA256, b.HashSHA512, now)
	if err != nil {
		return nil, fmt.Errorf("insert blob: %w", err)
	}
	if id, _ := res.LastInsertId(); id > 0 {
		b.ID = id
		return &b, nil
	}
	return s.GetBlobBySHA256(ctx, b.HashSHA256)
}

// GetBlobBySHA256 looks up a blob by SHA-256 hex digest.
func (s *Store) GetBlobBySHA256(ctx context.Context, sha256Hex string) (*Blob, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT id, size, hash_md5, hash_sha1, hash_sha256, hash_sha512, created_unix
		   FROM package_blobs WHERE hash_sha256 = ?`, sha256Hex)
	b := &Blob{}
	if err := row.Scan(&b.ID, &b.Size, &b.HashMD5, &b.HashSHA1, &b.HashSHA256, &b.HashSHA512, &b.CreatedUnix); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrBlobNotExist
		}
		return nil, err
	}
	return b, nil
}

// ----- Files -----

// CreateFile inserts a file referencing a blob. Returns
// ErrDuplicatePackageFile if (version_id, lower_name) already exists.
func (s *Store) CreateFile(ctx context.Context, f File) (*File, error) {
	lower := strings.ToLower(f.Name)
	now := time.Now().Unix()
	isLead := 0
	if f.IsLead {
		isLead = 1
	}
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO package_files (version_id, blob_id, name, lower_name, is_lead, created_unix)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		f.VersionID, f.BlobID, f.Name, lower, isLead, now)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicatePackageFile
		}
		return nil, fmt.Errorf("insert file: %w", err)
	}
	id, _ := res.LastInsertId()
	f.ID = id
	f.LowerName = lower
	f.CreatedUnix = now
	return &f, nil
}

// ListFilesByVersion returns all files attached to a version.
func (s *Store) ListFilesByVersion(ctx context.Context, versionID int64) ([]*File, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, version_id, blob_id, name, lower_name, is_lead, created_unix
		   FROM package_files WHERE version_id = ? ORDER BY name ASC`, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*File
	for rows.Next() {
		f := &File{}
		var isLead int
		if err := rows.Scan(&f.ID, &f.VersionID, &f.BlobID, &f.Name, &f.LowerName, &isLead, &f.CreatedUnix); err != nil {
			return nil, err
		}
		f.IsLead = isLead != 0
		out = append(out, f)
	}
	return out, rows.Err()
}

// GetFileByVersionAndName returns the file row matching (version_id,
// lower_name = LOWER(name)). Returns ErrFileNotExist on miss.
func (s *Store) GetFileByVersionAndName(ctx context.Context, versionID int64, name string) (*File, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT id, version_id, blob_id, name, lower_name, is_lead, created_unix
		   FROM package_files WHERE version_id = ? AND lower_name = ?`,
		versionID, strings.ToLower(name))
	f := &File{}
	var isLead int
	if err := row.Scan(&f.ID, &f.VersionID, &f.BlobID, &f.Name, &f.LowerName, &isLead, &f.CreatedUnix); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrFileNotExist
		}
		return nil, err
	}
	f.IsLead = isLead != 0
	return f, nil
}

// DeleteFile removes one file row by id. Does NOT touch the blob it
// references — blob lifecycle is managed independently (content-
// addressed; an orphan GC pass collects unreferenced blobs later).
// Missing rows are not an error.
func (s *Store) DeleteFile(ctx context.Context, fileID int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM package_files WHERE id = ?`, fileID)
	return err
}

// DeleteVersion removes a version row by id; ON DELETE CASCADE on the
// package_files FK drops all file rows attached to it. Missing rows are
// not an error.
func (s *Store) DeleteVersion(ctx context.Context, versionID int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM package_versions WHERE id = ?`, versionID)
	return err
}

// ----- Properties -----

// SetProperty inserts (or replaces) a property for (refType, refID, name).
func (s *Store) SetProperty(ctx context.Context, refType PropertyRefType, refID int64, name, value string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM package_properties WHERE ref_type = ? AND ref_id = ? AND name = ?`,
		int(refType), refID, name); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO package_properties (ref_type, ref_id, name, value) VALUES (?, ?, ?, ?)`,
		int(refType), refID, name, value); err != nil {
		return err
	}
	return tx.Commit()
}

// GetProperty fetches a single property's value. Returns ("", false) if missing.
func (s *Store) GetProperty(ctx context.Context, refType PropertyRefType, refID int64, name string) (string, bool, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT value FROM package_properties WHERE ref_type = ? AND ref_id = ? AND name = ?`,
		int(refType), refID, name)
	var v string
	if err := row.Scan(&v); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	return v, true, nil
}

// GetPropertiesByPrefix returns every (name, value) property attached to
// (refType, refID) whose name starts with prefix. Used by the npm
// dist-tag machinery (per-version properties keyed "npm.tag.<tag>").
func (s *Store) GetPropertiesByPrefix(ctx context.Context, refType PropertyRefType, refID int64, prefix string) ([]Property, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, ref_type, ref_id, name, value
		   FROM package_properties
		  WHERE ref_type = ? AND ref_id = ? AND name LIKE ? || '%'`,
		int(refType), refID, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Property
	for rows.Next() {
		var p Property
		var rt int
		if err := rows.Scan(&p.ID, &rt, &p.RefID, &p.Name, &p.Value); err != nil {
			return nil, err
		}
		p.RefType = PropertyRefType(rt)
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeleteProperty removes a single (refType, refID, name) property if
// present. Missing rows are not an error.
func (s *Store) DeleteProperty(ctx context.Context, refType PropertyRefType, refID int64, name string) error {
	_, err := s.DB.ExecContext(ctx,
		`DELETE FROM package_properties WHERE ref_type = ? AND ref_id = ? AND name = ?`,
		int(refType), refID, name)
	return err
}

// GetBlobByID fetches a blob row by primary key.
func (s *Store) GetBlobByID(ctx context.Context, id int64) (*Blob, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT id, size, hash_md5, hash_sha1, hash_sha256, hash_sha512, created_unix
		   FROM package_blobs WHERE id = ?`, id)
	b := &Blob{}
	if err := row.Scan(&b.ID, &b.Size, &b.HashMD5, &b.HashSHA1, &b.HashSHA256, &b.HashSHA512, &b.CreatedUnix); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrBlobNotExist
		}
		return nil, err
	}
	return b, nil
}

// isUniqueViolation returns true if err corresponds to a SQLite UNIQUE
// constraint failure. modernc.org/sqlite encodes SQLite extended error codes
// in the error message; we match on the substring to avoid pulling its
// internal error type into our package.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "constraint failed: UNIQUE")
}
