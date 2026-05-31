# `plans/`

Implementation plans for features in flight or queued. Each file is the
record of decisions for a single feature; once the feature ships the
plan moves to [`implemented/`](implemented/) with post-facto notes,
and the plan stays as the canonical answer to
"why was this built this way?".

Distinct from [`docs/`](../docs/), which is the user-/operator-facing
documentation of features that **already exist**.

## In-flight

_(none right now — submit one as `plans/<name>.md`)_

## Brainstorms / backlogs

Loose notes that aren't yet a concrete plan but inform what to build
next. Promote one to a real `plans/<name>.md` once it's ready to be
scoped.

| Doc | What it covers |
| --- | --- |
| [supply-chain-security-brainstorm.md](supply-chain-security-brainstorm.md) | Broader survey of supply-chain controls beyond the engine that already shipped — cooldowns, provenance, OSV, typosquat detection, tier promotion, etc. The shipped engine is the first slice of this list. |
| [multi-tenant.md](multi-tenant.md) | Decision note on whether path-prefix multi-tenancy is viable for a SaaS shape (yes, except OCI), and where the real operational costs of multi-tenancy live. Not a plan to change anything; informs future "lean in" or "scale back" plans. |

## Implemented

| Plan | Shipped | Post-facto notes |
| --- | --- | --- |
| [supply-chain-policy-engine.md](implemented/supply-chain-policy-engine.md) | 2026-05-27 | Engine + audit log + cooldown + license allowlist + admin endpoints. 8 step-aligned commits. See the file's "Post-implementation notes" section. |
