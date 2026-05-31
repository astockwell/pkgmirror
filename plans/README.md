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

## Implemented

| Plan | Shipped | Post-facto notes |
| --- | --- | --- |
| [supply-chain-policy-engine.md](implemented/supply-chain-policy-engine.md) | 2026-05-27 | Engine + audit log + cooldown + license allowlist + admin endpoints. 8 step-aligned commits. See the file's "Post-implementation notes" section. |
| [created-via-package-ownership.md](implemented/created-via-package-ownership.md) | 2026-05-31 | `packages.created_via` per-package marker; PyPI handler gates merge + 409s upload-against-pull-through; console badge + admin flip form; 14 new tests across 4 PRs. |
