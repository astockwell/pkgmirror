# Documentation index

| Doc | Purpose |
| --- | --- |
| [adding-a-format.md](adding-a-format.md) | Implementation playbook for every package format — anatomy, Forgejo references, per-format recipes (parser links, black-box client image + commands). |
| [adding-jit-pull-through.md](adding-jit-pull-through.md) | Companion to adding-a-format.md: how to add upstream pull-through (cold-miss fetch + policy gating + integration tests) to a format that already has its upload + serve side. |
| [auth.md](auth.md) | Multi-tenancy + auth model, transport per format, threat-model notes, future identity-source contract. |
| [blackbox-testing.md](blackbox-testing.md) | How we conformance-test each format against its real client, using docker containers. |
| [integration-testing.md](integration-testing.md) | Internet-connected integration suite that pulls from real public registries (pypi.org, etc.) on a weekly cron via `.github/workflows/integration.yml`; built atop the blackbox harness, gated behind the `integration` build tag. |
| [multi-tenant.md](multi-tenant.md) | URL routing model for multi-tenancy, per-format compatibility matrix, and the OCI nuance (path-prefix everywhere; image-name-prefix for OCI because `/v2/` is host-rooted). |
| [storage.md](storage.md) | The blob-store interface (a port of Forgejo's `ObjectStorage`), the SQLite/blob split, the `LocalStorage` implementation, and the path to additional backends (S3 / MinIO / GCS). |
| [supply-chain.md](supply-chain.md) | Operator runbook for the supply-chain controls: cooldown, license allowlist, audit log, quarantine workflow. |

For the broader brainstorm of future supply-chain controls see
[`plans/supply-chain-security-brainstorm.md`](../plans/supply-chain-security-brainstorm.md).

See also the top-level [`README.md`](../README.md) for usage and
[`DECISIONS.md`](../DECISIONS.md) for the running architecture log.
