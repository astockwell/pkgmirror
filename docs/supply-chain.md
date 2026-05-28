# Supply-chain controls

This document is the **operator runbook** for the supply-chain policy
engine. It covers what's enforced today, how to configure it, where
audit trails live, and what to do when a version gets quarantined.

For the architectural design see
[`plans/implemented/supply-chain-policy-engine.md`](../plans/implemented/supply-chain-policy-engine.md).
For the broader brainstorm of future controls see
[`plans/supply-chain-security-brainstorm.md`](../plans/supply-chain-security-brainstorm.md).

---

## What's enforced today

| Control | What it does | Status |
| --- | --- | --- |
| **Cooldown** | Hide a version until it has been in the mirror for at least N days. Reduces blast radius of compromised upstream releases. | shipped |
| **License allowlist** | Refuse (or quarantine / warn) when an artifact's SPDX license is not in the allowed set. | shipped (PyPI today; per-format extractor needed for go/npm/etc.) |
| **Audit log** | Every ingest, every non-Allow decision, and optionally every read recorded with actor, request id, decision, reason. | shipped |
| **Quarantine** | "Stored but hidden" status for a version. Admin can promote or reject. | shipped |
| Per-version blocklist | Deny by exact (name, version). | deferred — architecture supports it; not implemented |
| Sigstore / PEP 740 attestation | Verify provenance signatures. | future |
| OSV vulnerability gate | Block on known CVE. | future |
| Typosquat detection, velocity caps, post-install scanning | Heuristic controls. | future |

---

## How decisions are made

Every package operation goes through one engine call:

```
Subject (tenant, format, package, version, filename, attrs)
       + Action (ingest | read)
                  ↓
       policy.Engine.Evaluate
                  ↓
       For each registered Evaluator (cooldown, license_allow, …):
           pick matching rules for this Subject
           let the evaluator fold them into a Result
       Strictest decision across all evaluators wins.
                  ↓
       Allow → proceed normally
       Warn → proceed; audit row records it
       Quarantine → store-but-hide on ingest; 403/omit-from-listings on read
       Deny → 403 on ingest and on read
```

**Strictest wins**: `Allow < Warn < Quarantine < Deny`. If cooldown
returns Warn and license_allow returns Deny, Deny wins. There is no way
for a later evaluator to "rescue" a denied subject — short-circuit on
Deny ensures the engine is fast even with many rules.

**Audit always happens.** Every `ActionIngest` is logged. Every non-Allow
`ActionRead` is logged. Successful reads are logged only when the tenant
has `audit_reads = 1`.

---

## Rule scoping (cascade model)

Rules cascade by **specificity**. A rule's selector is the set of
fields it pins:

| If the rule sets… | Specificity contribution |
| --- | --- |
| `version_pattern` (glob) | +16 |
| `package` | +8 |
| `tenant` | +4 |
| `format` | +2 |
| (nothing → "any") | 0 |

For a given Subject, the engine finds all matching rules of a given
kind and sorts by `(specificity DESC, priority ASC)`. Each evaluator
then folds them per its own semantics:

- **Cooldown**: most-specific rule wins (single rule applied). Same
  for **License**.
- **Per-version blocklist** (when shipped): any matching rule denies.

This means **a more-specific rule narrows or relaxes a broader one**,
without any "override" mechanism. A `min_age_days: 0` rule scoped to
`(prod, pypi, requests)` is the fast-path exemption for `requests` only.

---

## Configuring rules

Rules are stored in the `policy_rules` table. Two ways to manage them:

### 1. YAML file at boot (recommended for declarative config)

Point `PKGMIRROR_POLICY_FILE` at a YAML file. On boot, every rule is
upserted into the table by `name` (so updating the file changes the
existing row; new rules get added). **Rules absent from the file are
not removed** — DB hotfixes survive.

#### Rule fields

Every entry in `rules:` accepts the same fields. Selector fields are all
optional — leave them out to mean "any".

| Field | Required | Purpose |
| --- | --- | --- |
| `name` | yes | Unique key; re-using a name updates that row. |
| `kind` | yes | `cooldown` \| `license_allow` (more land later). |
| `action` | yes | `deny` \| `quarantine` \| `warn`. |
| `config` | per-kind | Kind-specific knobs (see the per-control sections). |
| `enabled` | no (default `true`) | Set `false` to keep the rule but disable it. |
| **Selector — scope dimensions:** | | |
| `tenant` | no | Tenant **name**. Empty = applies across all tenants. |
| `format` | no | `go`, `pypi`, …  Empty = applies to all formats. |
| `package` | no | Canonical (lowercased / PEP-503-normalized) package name. Empty = all packages. |
| `version_pattern` | no | Glob (e.g. `2.*`, `v1.2.3`). Empty = all versions. |
| **Tiebreak / lifecycle:** | | |
| `priority` | no (default `100`) | Lower wins when two rules have equal specificity. |
| `expires_at` | no | RFC-3339 timestamp; after this the rule is ignored. |

The four selector fields are the cascade dimensions described in
[Rule scoping](#rule-scoping-cascade-model) above. **More-set fields =
more specific = wins over less-specific rules of the same kind.** You
can express every combination you need — any subset of (tenant × format
× package × version_pattern) — by setting or omitting each field.

#### Example: a layered cooldown policy

```yaml
# /etc/pkgmirror/policy.yaml
rules:

  # Layer 1 — broadest. Applies to every tenant + every format.
  # Specificity: 0.
  - name: org-default-cooldown
    kind: cooldown
    action: quarantine
    config:
      min_age_days: 7

  # Layer 2 — tenant-scoped. Applies to acme-prod for every format.
  # Specificity: 4 (tenant).
  - name: prod-tenant-strict-cooldown
    kind: cooldown
    tenant: acme-prod
    action: quarantine
    config:
      min_age_days: 21

  # Layer 3 — tenant + format. Applies to acme-prod's pypi packages only.
  # Specificity: 6 (tenant + format).
  - name: prod-tenant-pypi-stricter
    kind: cooldown
    tenant: acme-prod
    format: pypi
    action: quarantine
    config:
      min_age_days: 30

  # Layer 4 — tenant + format + package. The fast-path exemption for
  # `requests` only inside acme-prod's pypi mirror.
  # Specificity: 14 (tenant + format + package).
  - name: prod-tenant-pypi-requests-fastpath
    kind: cooldown
    tenant: acme-prod
    format: pypi
    package: requests
    action: warn
    config:
      min_age_days: 0          # explicit "no cooldown"

  # Layer 5 — tenant + format + package + version pattern. Block one
  # specific bad release while everything else flows normally.
  # Specificity: 30 (all four selectors).
  - name: block-cve-version
    kind: block            # NOTE: blocklist is not yet implemented;
    tenant: acme-prod      #       shown for shape only.
    format: pypi
    package: ua-parser-js
    version_pattern: 0.7.29
    action: deny
    config: {}

  # ---- A different kind: license allowlist ----

  - name: org-license-allowlist
    kind: license_allow
    action: deny             # off-list licenses are refused at ingest
    config:
      allow: [MIT, Apache-2.0, BSD-3-Clause, ISC, MPL-2.0, BSD-2-Clause, Unlicense]
      on_unknown: warn

  # Same kind, narrower scope: a legal-approved exception for one
  # internal package that's GPL.
  - name: acme-internal-gpl-exception
    kind: license_allow
    tenant: acme-prod
    format: pypi
    package: acme-internal-gpl-thing
    action: deny
    config:
      allow: [GPL-3.0]
```

#### Worked example: which rule applies?

Take the subject `acme-prod / pypi / requests / 2.32.0`.

For `kind: cooldown`, four rules match: `org-default-cooldown`,
`prod-tenant-strict-cooldown`, `prod-tenant-pypi-stricter`, and
`prod-tenant-pypi-requests-fastpath`. The engine sorts by
`(specificity DESC, priority ASC)` and the cooldown evaluator applies
the first one: **`prod-tenant-pypi-requests-fastpath`** (specificity
14, `min_age_days: 0` → Allow).

Now take `acme-prod / pypi / foo / 1.0.0`. The same first three rules
match; the fast-path rule does not (its `package: requests` filter
excludes it). The winner is now **`prod-tenant-pypi-stricter`**
(specificity 6, 30-day cooldown).

This is the entirety of the cascade model — no special-case overrides,
no implicit precedence rules, just data.

After editing, restart pkgmirror to re-sync (a `SIGHUP`-triggered reload
is on the roadmap). The in-memory cache also refreshes from the DB every
30s, so SQL-level changes propagate without restart.

### 2. Admin HTTP API (good for ops + emergencies)

All admin endpoints require a token that:

- belongs to a user with `is_admin = 1`, **and**
- carries scope `admin`.

The bootstrap admin token printed on first boot satisfies both.

```sh
TOKEN=pkm_…   # the bootstrap admin token

# List active rules
curl -s -H "Authorization: Bearer $TOKEN" \
    http://localhost:8080/admin/rules | jq

# Upsert (name is the key — same name = update)
curl -s -X POST -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    -d '{
          "name": "block-bad-version",
          "kind": "cooldown",
          "tenant_id": 2,
          "format": "pypi",
          "package": "left-pad",
          "version_pattern": "1.2.3",
          "action": "deny",
          "config": {"min_age_days": 36500}
        }' \
    http://localhost:8080/admin/rules

# Disable / re-enable
curl -s -X POST -H "Authorization: Bearer $TOKEN" \
    -d '{"enabled":false}' \
    http://localhost:8080/admin/rules/42/enabled

# Delete
curl -s -X DELETE -H "Authorization: Bearer $TOKEN" \
    http://localhost:8080/admin/rules/42
```

### Rule actions: what each one means

| `action` | On Ingest | On Read |
| --- | --- | --- |
| `deny` | 403 — artifact never enters storage | 403 |
| `quarantine` | 200; bytes stored, `quarantine_reason` set | omitted from `/simple/`, `/@v/list`; file downloads 403 |
| `warn` | proceed; audit row records it | proceed; audit row records it |

Pick **quarantine** when you want a human to look at the artifact before
it goes live. Pick **deny** for unrecoverable refusals (license
violations, known-malicious versions).

---

## Cooldown control

**Config:**

```yaml
config:
  min_age_days: 7
```

**Semantics:**

- Age is `now - package_versions.created_unix` (ingest time on this
  mirror).
- If `age < min_age_days`, the rule's action fires.
- `min_age_days: 0` is the explicit "no cooldown" form — useful as
  a more-specific exemption rule for a trusted package.
- A missing or unparseable `ingest_age_seconds` attribute returns
  Allow (with a Reason logged) — never block on a misconfiguration.

**Typical setup:**

```yaml
- name: org-cooldown
  kind: cooldown
  action: quarantine
  config: { min_age_days: 7 }

- name: prod-stricter
  kind: cooldown
  tenant: prod
  action: quarantine
  config: { min_age_days: 21 }
```

This gives the org 7 days globally and `prod` 21 days specifically.
A team can add a package-scoped override to relax it for known-trusted
deps.

---

## License allowlist control

**Config:**

```yaml
config:
  allow: [MIT, Apache-2.0, BSD-3-Clause]
  on_unknown: warn         # warn (default) | deny | allow
```

**Semantics:**

- License source: the SPDX identifier in
  `package_versions.license`. Populated at ingest by the per-format
  extractor (PyPI today; other formats will follow).
- Match is case-insensitive.
- **Most-specific rule REPLACES less-specific allowlist** (no merging).
  This lets a tenant-specific rule both *narrow* (e.g. drop GPL) and
  *broaden* (e.g. add a vendored license) the global default.
- `on_unknown` decides what happens when the license column is empty
  or NULL. Defaults to `warn`.

---

## Audit log

### What gets logged

- **Every ingest** (success or failure)
- **Every non-Allow read** (the interesting events)
- **Every successful read** — only if `tenants.audit_reads = 1` for
  that tenant. Off by default; enable per-tenant for compliance.
- **Every admin action** (rule_upsert, rule_set_enabled, rule_delete,
  promote_quarantined, reject_quarantined)

### What's in each row

- `actor_user_id`, `actor_token_id` — who
- `request_id`, `remote_addr`, `user_agent` — request context
- `tenant_id`, `format`, `package`, `version`, `filename` — what
- `decision`, `rule_id`, `reason` — verdict
- `extra_json` — evaluator-specific evidence

### Querying

```sh
# Recent denied ingests
curl -s -H "Authorization: Bearer $TOKEN" \
    'http://localhost:8080/admin/audit?action=ingest&decision=deny&limit=50' | jq

# Who downloaded a specific version (requires audit_reads enabled on the tenant)
curl -s -H "Authorization: Bearer $TOKEN" \
    'http://localhost:8080/admin/audit?format=pypi&package=ua-parser-js&version=0.7.29&action=read&decision=allow' | jq
```

The standard CVE incident query — "who pulled the bad version?" — is one
indexed `SELECT` against `audit_log` joining on actor_user_id.

### Retention

Default 90 days per tenant; configurable via the
`tenants.audit_retention_days` column. A pruner runs every 24 hours and
deletes rows older than the owning tenant's retention. System events
(tenant_id NULL) use the 90-day default.

---

## Quarantine workflow

When a rule fires with `action: quarantine`:

1. On **Ingest**: the artifact bytes are stored normally. The version
   row's `quarantine_reason` is set, and `quarantined_by_rule_id` points
   at the rule that fired. The upload returns 201 — the operator who
   pushed it is not blocked.
2. On **Read**: the version is omitted from `/simple/<name>/`, `/@v/list`,
   and `@latest`. File-download endpoints return 403 with the reason.
3. The package itself is hidden from the root `/simple/` index if every
   one of its versions is quarantined.
4. The version appears in `/admin/quarantine`.

### Promote (clear quarantine)

```sh
curl -s -X POST -H "Authorization: Bearer $TOKEN" \
    'http://localhost:8080/admin/quarantine/123/promote?reason=manual%20review%20by%20alice'
```

`quarantine_reason` is cleared; the version becomes visible again. The
action is logged as `promote_quarantined` with the supplied reason.

### Reject (keep in quarantine permanently)

```sh
curl -s -X POST -H "Authorization: Bearer $TOKEN" \
    'http://localhost:8080/admin/quarantine/123/reject?reason=CVE-2026-XXXX'
```

`quarantine_reason` is updated to `REJECTED: …`; the version stays
hidden, but the bytes remain in storage for forensics. Deletion is
intentionally not exposed via the API — it's destructive and we want
the audit trail.

---

## Defaults and fail modes

| Situation | Behavior |
| --- | --- |
| No rules configured | NoopEngine — everything Allow |
| Rule with no matching subject | Allow (that evaluator returns Allow) |
| Rule with malformed config_json | Allow with Reason logged — operator misconfig must never lock the registry |
| Evaluator panics | Caught by the engine; that evaluator yields Allow, the rest of the chain runs |
| DB unreachable during Evaluate | Per-evaluator: each falls back to Allow with a Reason — the audit row captures it |
| Audit channel full | Event dropped; counter incremented (`Logger.Drops()`); no impact on the request |

The engine is designed so a single rule typo or DB blip never produces a
hard outage of the registry. Strictness is opt-in via explicit rules.

---

## Configuration variables

| Env var | Default | Description |
| --- | --- | --- |
| `PKGMIRROR_POLICY_FILE` | _(unset)_ | Path to YAML rule file. Upserted into DB on boot. Absent → DB is the only source. |

Per-tenant settings (columns on the `tenants` table; manage via SQL or
future admin endpoint):

| Column | Default | Description |
| --- | --- | --- |
| `audit_reads` | 0 | When 1, audit every successful read for this tenant. |
| `audit_retention_days` | 90 | Audit log retention. Set higher for compliance-sensitive tenants. |

---

## See also

- [`auth.md`](auth.md) — the auth model the admin endpoints sit on top of.
- [`../plans/supply-chain-security-brainstorm.md`](../plans/supply-chain-security-brainstorm.md) — the broader
  brainstorm of controls and the roadmap.
- [`adding-a-format.md`](adding-a-format.md) — when adding a new format,
  remember to populate `Subject.Attrs["license"]` if the format carries
  license metadata so the allowlist evaluator works for it.
