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

## Implemented

| Plan | Shipped | Post-facto notes |
| --- | --- | --- |
| [supply-chain-policy-engine.md](implemented/supply-chain-policy-engine.md) | 2026-05-27 | Engine + audit log + cooldown + license allowlist + admin endpoints. 8 step-aligned commits. See the file's "Post-implementation notes" section. |
