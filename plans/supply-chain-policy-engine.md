# Plan: supply-chain policy engine

**Status:** approved, not yet implemented
**Scope of this plan:** the foundational policy engine + audit log + two
concrete controls — **cooldown** and **license allowlist**. Per-version
blocklist is deferred but the schema is designed to accommodate it (and
any future control) without changes.

Companion doc: [`docs/supply-chain-security.md`](../docs/supply-chain-security.md)
(the broader brainstorm of controls). This plan covers only what we are
implementing in the first pass.

---

## 1. Goal

Build a single, data-driven policy engine that every supply-chain control
plugs into. Controls share:

- one schema for rule storage (`policy_rules`)
- one schema for audit (`audit_log`)
- one engine interface that handlers call
- one specificity model for cascading
- one set of hook points (Ingest at write, Read at serve, Read at list)

Get this right once and every future control (typosquat, OSV match,
sigstore verification, velocity caps, SBOM gate, …) is a self-contained
**~100-300 LOC evaluator** with no changes to handlers, models, or
schema.

## 2. The unifying abstraction

Every supply-chain control answers the same question:

> *Given a subject, an action, and the request context, do we
> Allow / Warn / Quarantine / Deny?*

```go
// internal/policy/policy.go

type Action int
const (
    ActionIngest Action = iota   // upload OR pull-through fetch
    ActionRead                    // download or listing inclusion
)

type Decision int
const (
    Allow Decision = iota
    Warn                          // serve but record
    Quarantine                    // store but hide from indices/serves
    Deny                          // refuse
)

type Subject struct {
    TenantID  int64
    Format    models.Type       // "go", "pypi", …
    Package   string             // canonical lookup key
    Version   string
    Filename  string

    // Computed / looked-up facts, populated lazily by enrichers.
    // Used today: "license", "ingest_age_days".
    // Future:    "risk_score", "osv_severity", "sigstore_verified".
    Attrs map[string]any
}

type Result struct {
    Decision Decision
    Reason   string   // human-readable; surfaced in HTTP body + audit
    RuleID   int64    // rule that fired (0 if no rule applied)
}

type Engine interface {
    Evaluate(ctx context.Context, s Subject, a Action) Result
}

type Evaluator interface {
    Kind() string                                             // "cooldown" | "license_allow" | …
    Evaluate(ctx context.Context, s Subject, a Action, rules []Rule) Result
}
```

Handlers know only about `Engine`. The engine dispatches per `kind` to
registered evaluators.

## 3. Schema

Two new tables and two new columns on `package_versions`. Added as
migration **v3**.

### 3.1 `policy_rules`

```sql
CREATE TABLE policy_rules (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT    NOT NULL,           -- human label, e.g. "acme-prod 14d cooldown"
    kind         TEXT    NOT NULL,           -- "cooldown" | "license_allow" | (future) "block" | …

    -- Selector. NULL = "any" for that dimension.
    -- More-set dimensions = more specific.
    tenant_id          INTEGER REFERENCES tenants(id) ON DELETE CASCADE,
    format             TEXT,
    package_lower_name TEXT,
    version_pattern    TEXT,                 -- glob (semver ranges later, per-format)

    -- Decision payload.
    action       TEXT NOT NULL,              -- "deny" | "quarantine" | "warn"
    config_json  TEXT NOT NULL DEFAULT '{}', -- kind-specific config
    priority     INTEGER NOT NULL DEFAULT 100, -- tiebreaker among equal-specificity rules; lower wins

    enabled      INTEGER NOT NULL DEFAULT 1,
    created_unix INTEGER NOT NULL,
    created_by_user_id INTEGER REFERENCES users(id),
    expires_unix INTEGER NOT NULL DEFAULT 0  -- 0 = never
);

CREATE INDEX idx_rules_lookup ON policy_rules(enabled, kind, tenant_id, format);
```

### 3.2 `audit_log`

Designed in from day 1 (decision: yes, emphatically).

```sql
CREATE TABLE audit_log (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    created_unix    INTEGER NOT NULL,

    -- Who. Either may be NULL for anonymous reads on public tenants
    -- or for system-originated events (bootstrap, rule sync).
    actor_user_id   INTEGER REFERENCES users(id)  ON DELETE SET NULL,
    actor_token_id  INTEGER REFERENCES tokens(id) ON DELETE SET NULL,
    request_id      TEXT,                       -- correlation id from middleware
    remote_addr     TEXT,
    user_agent      TEXT,

    -- What.
    tenant_id       INTEGER REFERENCES tenants(id) ON DELETE SET NULL,
    action          TEXT NOT NULL,              -- "ingest" | "read" | "rule_create" | "rule_update" | "rule_delete" | "promote_quarantined"
    format          TEXT,
    package         TEXT,
    version         TEXT,
    filename        TEXT,

    -- Decision (NULL for non-policy events).
    decision        TEXT,                       -- "allow" | "warn" | "quarantine" | "deny"
    rule_id         INTEGER REFERENCES policy_rules(id) ON DELETE SET NULL,
    reason          TEXT,

    extra_json      TEXT NOT NULL DEFAULT '{}'  -- evaluator-specific evidence (e.g. licenses considered)
);

CREATE INDEX idx_audit_tenant_time ON audit_log(tenant_id, created_unix DESC);
CREATE INDEX idx_audit_pkg          ON audit_log(format, package, version);
CREATE INDEX idx_audit_actor        ON audit_log(actor_user_id, created_unix DESC);
```

**Write semantics:**

- Audit writes go through `audit.Log(...)` which **never blocks the
  request critical path**. Internally a bounded buffered channel (default
  4096) and a single drainer goroutine. On overflow we drop and emit a
  Prometheus-style counter; never block the handler.
- Every `ActionIngest` is logged (success or denial).
- Every non-Allow `ActionRead` is logged.
- Successful `ActionRead`s are logged only when a tenant has
  `audit_reads = 1` (see §3.3). Default off — high-traffic registries
  generate enormous read volume; compliance-sensitive tenants opt in.
- Every rule mutation is logged.
- Every quarantine promotion / rejection is logged.

**Retention:** tenant-configurable, default 90 days. Pruner runs in a
nightly goroutine; deletes rows older than `tenants.audit_retention_days`.

### 3.3 New columns on `tenants`

```sql
ALTER TABLE tenants ADD COLUMN audit_reads          INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tenants ADD COLUMN audit_retention_days INTEGER NOT NULL DEFAULT 90;
```

### 3.4 New columns on `package_versions`

```sql
ALTER TABLE package_versions ADD COLUMN license           TEXT;             -- SPDX expression; nullable
ALTER TABLE package_versions ADD COLUMN quarantine_reason TEXT;             -- non-null = quarantined
ALTER TABLE package_versions ADD COLUMN quarantined_by_rule_id INTEGER REFERENCES policy_rules(id) ON DELETE SET NULL;
```

A row is "quarantined" iff `quarantine_reason IS NOT NULL`. Quarantined
versions are **stored but hidden**: omitted from `/simple/<name>/`,
`/@v/list`, `@latest`, etc.; serving `.zip`/`.whl`/file-download returns
403 with the reason. Admin tooling can clear the column to promote.

(Upstream-published time, which we discussed for cooldown, is **not**
added now per the agreed decision: cooldown uses `created_unix`.)

## 4. Specificity model

For a given `(subject, kind)`, the engine collects all matching enabled
rules and sorts them by:

```
specificity =  16*(version_pattern != "")
            +   8*(package_lower_name != "")
            +   4*(tenant_id != NULL)
            +   2*(format != "")
```

Then by `priority ASC` for ties. The result is a deterministic ordering
the per-kind evaluator can fold however it needs.

(No `attr_predicate` term — we agreed to defer attribute-based selectors.
When added, it slots in as the +1 bit.)

### Worked example (cooldown rules)

| Rule | (tenant, format, pkg, ver) | Specificity | min_age_days |
| --- | --- | --- | --- |
| A | (`acme-prod`, *, *, *) | 4 | 14 |
| B | (`acme-prod`, `pypi`, *, *) | 6 | 21 |
| C | (`acme-prod`, `pypi`, `requests`, *) | 14 | 0 |

For subject `acme-prod / pypi / requests / 2.32.0`, all three match. The
cooldown evaluator's fold rule is "highest specificity wins"; C applies,
`min_age_days = 0`, no cooldown. Same matching machinery, no special
"override" mechanism: an override is just a more-specific rule with a
loosened parameter.

## 5. Engine flow

```go
func (e *engine) Evaluate(ctx context.Context, s Subject, a Action) Result {
    // 1. Enrich subject — populate s.Attrs lazily (e.g. fetch license).
    //    For v1: license enrichment runs at Ingest only (we already have
    //    it as a property from the upload form for PyPI); a future
    //    LicenseLookup interface lets us add fetchers per format.
    e.enrich(ctx, &s)

    // 2. Per-kind evaluation in a fixed order (cheapest deny first
    //    eventually — for v1 the order is: license_allow, cooldown).
    var deepest Result = Result{Decision: Allow}
    for _, ev := range e.evaluators {
        rules := e.matchingRules(s, ev.Kind())
        r := ev.Evaluate(ctx, s, a, rules)
        if r.Decision > deepest.Decision { // ordering: Allow<Warn<Quarantine<Deny
            deepest = r
        }
        if deepest.Decision == Deny {
            break // short-circuit on hard deny
        }
    }
    e.audit(ctx, s, a, deepest)   // never blocks the caller
    return deepest
}
```

Notes:

- **Rule cache.** Rules are loaded into memory at boot and refreshed by a
  hash-versioned poller (every 30s; cheap because the table is small).
  Handler-path evaluation never touches the DB for rules.
- **Fail mode** (per agreed decision):
  - On Ingest: any engine error → `Deny` ("fail-closed").
  - On Read:   any engine error → `Allow` ("fail-open"), logged.
- **Auditing.** The engine always writes one audit row per evaluation
  (except suppressed read-successes per §3.2). Audit writes go to a
  buffered channel; never block the handler.

## 6. The two controls

### 6.1 Cooldown

```jsonc
// config_json
{ "min_age_days": 7 }
```

- Fold rule: **highest specificity wins** (single rule applied).
- Source of age (per agreed decision): `now - package_versions.created_unix`.
- Action: typically `quarantine` on Ingest (so the bytes are stored for
  later promotion) and `deny` on Read (so the version is hidden until
  mature). The `action` column in the rule controls this; the example
  YAML below shows the recommended pattern.
- Implementation lives in `internal/policy/cooldown/cooldown.go`.

### 6.2 License allowlist

```jsonc
{
  "allow": ["MIT", "Apache-2.0", "BSD-3-Clause", "ISC", "MPL-2.0"],
  "on_unknown": "warn"   // "deny" | "warn" | "allow"
}
```

- Fold rule: **most specific rule REPLACES (no merging)**. Lists do not
  union across the cascade — that would make it impossible to *narrow*
  the allowlist for a more-specific scope.
- License source:
  - PyPI: already in our upload form → goes into `package_versions.license`
    at ingest. No change to handlers beyond writing the column.
  - Other formats: per-format extractor added as each format lands.
    Until then, `license IS NULL` and `on_unknown` decides.
- Action: typically `deny` on Ingest (no point storing what we'll never
  let anyone use) and `deny` on Read (defense in depth, in case the rule
  was added after ingest).
- Implementation lives in `internal/policy/license/license.go`.

### 6.3 Blocklist (deferred per agreed decision)

Not implemented in this pass. When added, it's `kind="block"` with
`(package_lower_name, version_pattern)` set and `action="deny"`. No
schema or engine change required. The plan documents this only to
confirm the architecture accommodates it.

## 7. Decision answers, recorded here

For permanent record:

1. **Quarantine semantics:** stored but hidden. Bytes land in blob
   storage; `package_versions.quarantine_reason` is set; indices skip the
   row; serves 403 with reason. Admin can promote by clearing the column.
2. **Fail mode:** Deny on Ingest, Allow on Read.
3. **Computed attributes / risk scores:** deferred. The `Subject.Attrs`
   map is already in place for future enrichers; no schema impact.
4. **Cooldown time source:** `package_versions.created_unix` (ingest
   time). No `upstream_published_unix` column added now.
5. **Rule storage:** DB is source of truth. A YAML config at boot is
   parsed and upserted into the table; see §10 for the format.
6. **Audit log:** first-class, designed in from day 1. Schema in §3.2.

## 8. Hook points in code

Three call sites. Each is one line of integration:

| Site | Action | On non-Allow |
| --- | --- | --- |
| `goproxy.upload`, `pypi.upload` (and every future format upload) before `Storage.Put` | `ActionIngest` | Deny → 403 with `Reason`; Quarantine → set `package_versions.quarantine_reason`, still record the file |
| Per-version filtering in index handlers (`/simple/<name>/`, `/@v/list`, `@latest`) | `ActionRead` | Omit version from the listing if Decision ≥ Quarantine |
| File-serve handlers (`.zip`, `.whl`, `/files/...`) before `Service.OpenFile` | `ActionRead` | 403 with `Reason` |

Tenant-scoped UI listings get the same treatment.

## 9. Audit log shape, in practice

A few canonical events, for clarity:

```jsonc
// Successful PyPI upload
{
  "ts": 1748420000,
  "actor_user_id": 17, "actor_token_id": 42,
  "request_id": "01HZX9...",
  "tenant_id": 1,
  "action": "ingest",
  "format": "pypi", "package": "requests", "version": "2.32.0",
  "filename": "requests-2.32.0-py3-none-any.whl",
  "decision": "allow",
  "rule_id": null,
  "reason": null,
  "extra_json": "{\"size\":1234567,\"sha256\":\"…\"}"
}

// Cooldown caused Quarantine at ingest
{ … same shape …,
  "action": "ingest", "decision": "quarantine",
  "rule_id": 8,
  "reason": "cooldown: created 2026-05-20T... < min_age_days=7",
  "extra_json": "{\"age_days\":4}"
}

// Read attempt blocked because version is quarantined
{ … same shape …,
  "action": "read", "decision": "deny",
  "rule_id": 8,
  "reason": "version is in quarantine: cooldown not yet elapsed"
}

// Admin promotion of a quarantined version
{ … "action": "promote_quarantined", "decision": null,
  "reason": "manual review by Alice",
  "extra_json": "{\"previous_quarantine_reason\":\"cooldown...\"}"
}
```

Format / package / version are denormalized into every row deliberately,
so a CVE incident query is one indexed scan:

```sql
SELECT actor_user_id, MAX(created_unix)
FROM audit_log
WHERE format = 'pypi' AND package = 'ua-parser-js' AND version = '0.7.29'
  AND action = 'read' AND decision = 'allow'
GROUP BY actor_user_id;
```

## 10. YAML config format (v1)

A file (path from `PKGMIRROR_POLICY_FILE`) is read at boot and on
SIGHUP. Rules with matching `name` are upserted; rules absent from the
file are **not** removed (so DB hotfixes survive). A separate
`--sync-policy --prune` admin command will support full sync later.

```yaml
# pkgmirror/policy.yaml
rules:

  - name: org-default-cooldown
    kind: cooldown
    enabled: true
    action: quarantine          # on ingest: store-but-hide; on read: handled by quarantine
    config:
      min_age_days: 7

  - name: prod-tenant-strict-cooldown
    kind: cooldown
    enabled: true
    tenant: acme-prod
    action: quarantine
    config:
      min_age_days: 21

  - name: prod-tenant-pypi-stricter
    kind: cooldown
    enabled: true
    tenant: acme-prod
    format: pypi
    action: quarantine
    config:
      min_age_days: 30

  - name: prod-tenant-pypi-requests-fastpath
    kind: cooldown
    enabled: true
    tenant: acme-prod
    format: pypi
    package: requests
    action: warn                # don't even quarantine, just record
    config:
      min_age_days: 0

  - name: org-license-allowlist
    kind: license_allow
    enabled: true
    action: deny
    config:
      allow: [MIT, Apache-2.0, BSD-3-Clause, ISC, MPL-2.0, BSD-2-Clause, Unlicense]
      on_unknown: warn
```

## 11. Implementation order (low-risk, each step independently shippable)

Revised per agreed decisions (blocklist removed; audit log moved earlier).

| # | Step | Independently shippable? |
| --- | --- | --- |
| 1 | `internal/policy` package: types, no-op `Engine`, hook plumbing into all current handlers. **Zero behavior change.** | Yes — should land as one PR, all existing tests pass. |
| 2 | Migration v3: `policy_rules`, `audit_log`, new columns on `tenants` and `package_versions`. | Yes |
| 3 | `internal/audit` package with bounded-channel writer + nightly pruner. Wired into the no-op engine so we get audit on every Ingest/Read from day 1. | Yes — proves audit volume + cost before any controls are active. |
| 4 | Rule loader (DB + YAML upsert at boot, 30s memory refresh). Quarantine helpers on `models.Version` (`Quarantine`, `Promote`, `IsQuarantined`). | Yes |
| 5 | **Cooldown evaluator** + integration tests + black-box test that uploads a fresh version, asserts it's quarantined, fast-forwards `created_unix` via a test hook, asserts it's promoted. | Yes — first user-visible control. |
| 6 | **License allowlist evaluator** + PyPI license-column write at ingest + tests. | Yes |
| 7 | Admin endpoints (or CLI): list rules, list audit, list quarantined versions, promote/reject. | Yes |
| 8 | Docs update: `docs/auth.md` cross-references; new `docs/supply-chain.md` explains the runtime model for operators. | Yes |

Each step is its own commit / PR with green tests at every step.

## 12. Performance and correctness notes

- **Rule cache** uses a single `[]Rule` slice partitioned by kind, with a
  pre-sorted index by (`tenant_id`, `format`). Matching is O(matches) per
  evaluation, not O(rules). For typical scale (hundreds of rules) this
  is < 10 µs per call.
- **Audit writes** must not slow the request path. The bounded channel
  + dedicated drainer is the simplest correct design; overflow drops are
  exposed as a counter so we can size the buffer.
- **Migration safety.** v3 is purely additive — no data rewrites, no
  table drops. Rollback is `PRAGMA user_version = 2` plus dropping the
  new tables/columns; we'll script this in a `migrate down` helper
  before merge.
- **Test fixtures.** The unit-test fixture (`newFixture(t)` in
  `internal/packages/<fmt>/handler_test.go`) gets a small extension that
  takes a `[]policy.Rule` and seeds them. Existing tests pass nil and
  see no behavior change.
- **Black-box impact.** The black-box harness gets one new helper:
  `WithRules(rules []YAML)`, which writes the YAML before container
  start. The existing PyPI / goproxy conformance tests don't need rules
  and stay unchanged.

## 13. Out of scope / future work

Documented here so we don't have to argue about it later:

- **Per-version blocklist.** Architecture supports it; deferred per
  decision. Add as `kind="block"` evaluator.
- **Attribute predicates / risk scores.** `Subject.Attrs` is already in
  place; an `attr_predicate` selector column joins later.
- **Pull-through cache mode.** Cooldown semantics change subtly once
  pull-through is real (upstream published time matters). Plan revisits
  the time source in §6.1 at that point.
- **Sigstore / PEP 740 attestation verification.** A separate evaluator;
  no schema change needed.
- **OSV / vulnerability scanning.** A separate evaluator; needs a
  daemon-side OSV mirror. Schema unchanged.
- **Per-rule expiry visible in UI.** The column is there; admin UI is
  out of scope for this plan.

---

**Confirmation:** if any item in §11 changes shape, update this file in
the same PR so the plan remains the canonical record of what we
decided to build.
