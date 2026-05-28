package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// TenantResolver maps tenant names to IDs. The tenants.Store satisfies it.
type TenantResolver interface {
	ResolveTenantID(ctx context.Context, name string) (int64, error)
}

// yamlRule is the on-disk form of one rule.
type yamlRule struct {
	Name           string         `yaml:"name"`
	Kind           string         `yaml:"kind"`
	Enabled        *bool          `yaml:"enabled,omitempty"`
	Tenant         string         `yaml:"tenant,omitempty"`
	Format         string         `yaml:"format,omitempty"`
	Package        string         `yaml:"package,omitempty"`
	VersionPattern string         `yaml:"version_pattern,omitempty"`
	Action         string         `yaml:"action"`
	Config         map[string]any `yaml:"config,omitempty"`
	Priority       int            `yaml:"priority,omitempty"`
	ExpiresAt      string         `yaml:"expires_at,omitempty"`
}

type yamlConfig struct {
	Rules []yamlRule `yaml:"rules"`
}

// LoadYAMLFile reads a YAML rule file and returns the parsed rules. It
// does not touch the database; pair with RuleStore.Upsert. Returns an
// empty slice if path is empty (caller's "no file configured" case).
func LoadYAMLFile(ctx context.Context, path string, resolver TenantResolver) ([]Rule, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var doc yamlConfig
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	out := make([]Rule, 0, len(doc.Rules))
	for i, y := range doc.Rules {
		r, err := y.toRule(ctx, resolver)
		if err != nil {
			return nil, fmt.Errorf("rule #%d (%s): %w", i+1, y.Name, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// SyncYAMLFile reads path and upserts each rule into store, returning
// the number of rules processed. Rules absent from the file are NOT
// removed (operator hotfixes survive). A pruning sync mode can be added
// later.
func SyncYAMLFile(ctx context.Context, path string, store *RuleStore, resolver TenantResolver) (int, error) {
	rules, err := LoadYAMLFile(ctx, path, resolver)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rules {
		if _, err := store.Upsert(ctx, r); err != nil {
			return n, fmt.Errorf("upsert %q: %w", r.Name, err)
		}
		n++
	}
	return n, nil
}

func (y yamlRule) toRule(ctx context.Context, resolver TenantResolver) (Rule, error) {
	if y.Name == "" {
		return Rule{}, fmt.Errorf("name is required")
	}
	if y.Kind == "" {
		return Rule{}, fmt.Errorf("kind is required")
	}
	if y.Action == "" {
		return Rule{}, fmt.Errorf("action is required")
	}

	enabled := true
	if y.Enabled != nil {
		enabled = *y.Enabled
	}

	var tenantID int64
	if y.Tenant != "" {
		if resolver == nil {
			return Rule{}, fmt.Errorf("tenant %q referenced but no resolver supplied", y.Tenant)
		}
		id, err := resolver.ResolveTenantID(ctx, y.Tenant)
		if err != nil {
			return Rule{}, fmt.Errorf("resolve tenant %q: %w", y.Tenant, err)
		}
		tenantID = id
	}

	var expires int64
	if y.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, y.ExpiresAt)
		if err != nil {
			return Rule{}, fmt.Errorf("expires_at: %w", err)
		}
		expires = t.Unix()
	}

	configJSON := []byte("{}")
	if len(y.Config) > 0 {
		b, err := json.Marshal(y.Config)
		if err != nil {
			return Rule{}, fmt.Errorf("encode config: %w", err)
		}
		configJSON = b
	}

	return Rule{
		Name:             y.Name,
		Kind:             y.Kind,
		TenantID:         tenantID,
		Format:           strings.ToLower(strings.TrimSpace(y.Format)),
		PackageLowerName: strings.ToLower(strings.TrimSpace(y.Package)),
		VersionPattern:   strings.TrimSpace(y.VersionPattern),
		Action:           y.Action,
		ConfigJSON:       configJSON,
		Priority:         y.Priority,
		Enabled:          enabled,
		CreatedUnix:      time.Now().Unix(),
		ExpiresUnix:      expires,
	}, nil
}
