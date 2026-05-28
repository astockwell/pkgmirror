# Supply-chain controls

This document is the **operator runbook** for the supply-chain policy
engine. It covers what's enforced today, how to configure it, where
audit trails live, and what to do when a version gets quarantined.

For the architectural design see
[`plans/implemented/supply-chain-policy-engine.md`](../plans/implemented/supply-chain-policy-engine.md).
For the broader brainstorm of future controls see
[`supply-chain-security.md`](supply-chain-security.md).

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

```yaml
# /etc/pkgmirror/policy.yaml
rules:

  - name: org-default-cooldown
    kind: cooldown
    enabled: true
    action: quarantine        # on ingest: store-but-hide; on read: filtered
    config:
      min_age_days: 7

  - name: prod-tenant-strict-cooldown
    kind: cooldown
    tenant: acme-prod
    action: quarantine
    config:
      min_age_days: 21

  - name: prod-tenant-pypi-requests-fastpath
    kind: cooldown
    tenant: acme-prod
    format: pypi
    package: requests
    priority: 50              # lower wins on tiebreak
    action: warn
    config:
      min_age_days: 0         # exemption

  - name: org-license-allowlist
    kind: license_allow
    action: deny
    config:
      allow: [MIT, Apache-2.0, BSD-3-Clause, ISC, MPL-2.0, BSD-2-Clause, Unlicense]
      on_unknown: warn
```

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
- [`supply-chain-security.md`](supply-chain-security.md) — the broader
  brainstorm of controls and the roadmap.
- [`adding-a-format.md`](adding-a-format.md) — when adding a new format,
  remember to populate `Subject.Attrs["license"]` if the format carries
  license metadata so the allowlist evaluator works for it.
