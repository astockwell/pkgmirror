# Supply Chain Security

Supply chain security on a corporate mirror gives you a single chokepoint where you can layer defenses the public registries can't or won't enforce.

This file is currently a brainstorm.

## Quick framing: upload-only vs. pull-through

A lot of these controls depend on **when** the mirror sees a package. Right now pkgmirror is upload-only: a human or CI explicitly pushes things in. Most of the value of "supply chain mirror" features comes when you add a **pull-through cache** mode — your devs/CI set their `GOPROXY`/`PIP_INDEX_URL` to pkgmirror; on miss, pkgmirror fetches upstream, runs checks, and either ingests or quarantines. That's the ingest hook where policy enforcement lives.

We should assume pull-through is on the roadmap. Everything below assumes "at ingest time we get to inspect the artifact before serving it."

---

## Tier 1 — Temporal & velocity controls (cheap, high signal)

| Idea | What it does | Why it helps | Prior art |
| --- | --- | --- | --- |
| **Cooldown / `exclude-newer`** | A version is invisible until it's been on the upstream for ≥ N days. Per-format default + per-package overrides. | Most supply-chain compromises are caught by the ecosystem within hours-to-days (event-stream, ua-parser-js, colors.js, the recent xz-utils saga). A 7–14 day cooldown buys you that time for free. | `uv --exclude-newer`, Renovate's `minimumReleaseAge`, JFrog Artifactory "stable versions" |
| **Per-package velocity caps** | Reject if a new version comes < N hours after the previous one, or > M versions per week. | Anomaly signal for compromised packages flooding the registry with malicious releases (a known noisy-attack pattern). | Phylum, Socket.dev |
| **Version-skip detection** | `1.2.3` → `99.0.0` jump → quarantine for review. | `requests` going from `2.31` to `99.0.0` overnight is exactly what happened with several typo/dep-confusion attacks. | Renovate has heuristics for this |
| **Time-window freezes** | "No new versions enter prod-tier mirrors during release windows." | Reduces "we shipped a bad week-old version because nobody was watching." | Common at large orgs via ticketed approvals; rarely codified |

## Tier 2 — Provenance & integrity (highest leverage long-term)

| Idea | What it does | Why it helps | Prior art |
| --- | --- | --- | --- |
| **Sigstore / PEP 740 attestation verification** | Verify the package was signed by the upstream's published identity (e.g. via GitHub OIDC). | Strong cryptographic link between source repo and artifact. Defeats most "stolen maintainer credentials" attacks. | PyPI PEP 740 (live), npm provenance, sigstore policy-controller |
| **Hash pinning vs. upstream** | Cross-check the bytes we cached against the upstream registry's published hash and against a second source (e.g. a public mirror). | Catches replay/serve-different-bytes attacks against your mirror's upstream. | TUF (The Update Framework) |
| **SLSA level enforcement** | Require SLSA L2+ provenance to enter the "prod" tenant; allow lower into "experimental." | Tiered trust ladder. | SLSA spec, Google's binauthz |
| **Reproducible builds witness** | For formats that support it (Java, Rust, Go), verify the artifact matches a reproducible rebuild. | Detects build-server compromise (the SolarWinds class of attack). | reproducible-builds.org |
| **Immutable storage** | We already do this via content addressing — codify the policy: once `foo==1.2.3` exists, it cannot be replaced. | Defeats "we re-published with a malicious version" — the attacker needs a new version number, which trips your cooldown. | PyPI/npm both enforce; explicit in our code |

## Tier 3 — Vulnerability & policy gating

| Idea | What it does | Why it helps | Prior art |
| --- | --- | --- | --- |
| **OSV / GHSA integration on ingest** | Look up the package@version in OSV.dev; block or quarantine if a vulnerability is open. | Free, fast, well-maintained DB. Per-tenant policy: "no high/critical." | dependabot, trivy, grype, OSV-Scanner |
| **License allowlist** | Compute SPDX from the package metadata; reject GPL into the "proprietary-product" tenant. | Compliance lever your legal team will ask for within a quarter. | FOSSA, Snyk, scancode-toolkit |
| **SBOM generation + storage** | Run `syft` on the artifact, store the SBOM as a sibling file in the package_files table. | Mandatory for some regulated industries; useful for incident response ("who depends on log4j?"). | syft, cyclonedx, EU CRA emerging requirements |
| **Policy as code** | OPA/Rego or CEL: "every Python package on `prod` must (pass-OSV ∧ cooldown≥7d ∧ SLSA-L2) or be on the approved-overrides list." | One place to express the rules, auditable, version-controlled. | OPA, Kyverno, Conftest |
| **Approve-once-cache-forever override** | A reviewer can promote a single (name, version) past any failing check, with an audit trail and an expiring approval. | Critical escape hatch — without it people will route around the mirror. | Sonatype Firewall, Snyk Broker |

## Tier 4 — Heuristic / static analysis at ingest

| Idea | What it does | Why it helps | Prior art |
| --- | --- | --- | --- |
| **Typosquat detection** | Levenshtein + phonetic + popular-prefix detection against top-10k packages. Quarantine + email security team. | `python3-dateutil` vs `dateutil`, `reqests` vs `requests` — these attacks are constant. | guarddog, scorecard, PyPI's typosquat tooling |
| **Malicious-pattern scanning** | Run `guarddog` (python) / `npq` (npm) / similar — patterns like obfuscated install scripts, exfil to webhook.site, suspicious DNS. | Catches the bulk of automated malicious-publish attacks. | guarddog (Datadog OSS), npq, Phylum, Socket |
| **Post-install / pre-install script flagging** | npm `postinstall`, PyPI `setup.py` execution, `pyproject.toml` build hooks. Don't block — quarantine + require manual review. | These are the #1 vector for malicious npm packages. | Socket.dev surfaces this prominently |
| **Binary / native code detection** | Pure-Python package suddenly shipping a `.so`? Suspicious. | "ctx" PyPI attack 2022 added a hidden ELF. | Phylum, guarddog |
| **High-entropy string scan** | Detects obfuscated payloads, secrets baked in. | Same families of attacks. | trufflehog (for our secrets), entropy heuristics in many scanners |

## Tier 5 — Operational & incident response

| Idea | What it does | Why it helps | Prior art |
| --- | --- | --- | --- |
| **Full audit log** | Append-only log of: who pulled what, when, from which build, which token; ingest decisions; policy override actions. | Foundation for forensics. "Did anyone in prod pull `colors@1.4.4` between 09:30 and 11:00?" | Standard SIEM integration; we'd emit JSON to stderr or a webhook |
| **Quarantine queue** | Failed-policy packages land in `/quarantine/<format>/...`; reviewers see them in the UI; promote / reject / annotate. | Concrete workflow, not just "blocked." | Sonatype IQ, JFrog Xray |
| **"Used by" reverse index** | For any (name, version), list the builds (or CI runs) that pulled it. | Powers incident response: when a CVE drops, "what do I need to rebuild?" is one query. | dependabot, our existing audit if we tag pulls with build IDs |
| **Vulnerability re-scan on schedule** | Already-mirrored packages get re-checked daily; new CVEs trigger alerts. | A package is safe at ingest, then a CVE drops next week. | trivy DB updates, OSV mirror, Dependabot's daily scan |
| **Mirror-side egress allowlist** | The mirror itself only fetches upstream from a small list of registries over verified TLS, optionally with cert pinning. | Mitigates DNS hijack of `pypi.org` etc. | Sonatype "trust on first use" + pinning |

## Tier 6 — pkgmirror-architecture-specific wins

| Idea | What it does | Why it helps | Notes |
| --- | --- | --- | --- |
| **Dependency-confusion / namespace reservation** | Per tenant, declare "these names are internal and must NEVER be fetched from upstream." If upstream has the same name, refuse. | The 2021 Birsan attack chain. Critical for any org with internal packages. | Easy with our tenant model — add a `reserved_names` table per tenant |
| **Authoritative-source-per-package** | Per package, declare which upstream source is canonical. "We pull `requests` from PyPI, never from GitHub mirrors." | Stops mirror-of-mirror chain attacks. | Small per-package metadata addition |
| **Tier promotion** | Three tenants per format: `incoming` (anyone can pull, no gates), `staging` (cooldown + scan), `prod` (signed + approved). Promotion is a pipeline. | Concrete way to use our existing tenancy as a security artifact. | Heavy reuse of what we already built |
| **Per-token per-tenant rate limits + WAF-style anomaly detection on pulls** | "Build bot X normally pulls 50 deps; today it tried 500" → 429 + alert. | Catches build-machine compromise. | New code; uses our existing token model |
| **Token-based pull attribution** | Every pull is tagged with the token's owner (user_id) — we get this almost for free from our auth model. | Powers the "who used the bad version" query. | Audit-log addition |

## What to build first (rough order)

A pragmatic v1 corporate posture, ordered by **value × ease**:

1. **Audit log on every read + write** — foundation for everything else, ~1 day of work. Stream JSON to stderr; let SIEM tools take it from there.
2. **Cooldown** (per-tenant, per-format default + per-package overrides) — exactly your idea, fits cleanly because we already store `created_unix` on every version. Probably 1-2 days.
3. **Dependency-confusion namespace reservation** — minimal schema (`tenant_reserved_names`), enforcement is one check in the pull-through path. Cheap, prevents a real, repeated attack class.
4. **OSV integration on ingest** — block-or-quarantine on CVE match. Async (don't slow the pull); 2-3 days incl. test.
5. **Quarantine queue + admin promote UI** — once you have any "block" decision, you need a workflow. This is the spine for everything in Tier 4.
6. **Tier promotion model** (incoming → staging → prod tenants) — re-uses our tenancy directly; no new primitives.
7. **Sigstore / PEP 740 verification** for PyPI — the foundation has to land first for the rest of the provenance ladder to pay off.

Everything else (license, SBOM, typosquat detection, scheduled re-scan, velocity caps, policy-as-code) sits on top of these.

---

## A few worth surfacing because they're easy to miss

- **Pkgmirror itself becomes a high-value target.** Anything we ingest can be malicious *to the mirror*; multipart parsers, zip parsers, OCI manifest parsers all need to be sandboxed or memory-capped. We already cap go.mod at 16 MiB inside zips. Worth a separate threat-model pass before we add pull-through.
- **Don't ship a "skip-checks" flag without expiry + audit.** Every commercial product that has one regrets it.
- **The mirror's outbound HTTP client is part of the attack surface.** Pin upstream TLS roots; use a small allowlist of upstream registries; consider mTLS to those.
- **Reproducibility is a security feature.** Refusing re-uploads of an existing `(name, version)` (which we already do) is what makes a 7-day cooldown meaningful — otherwise the attacker just replaces the bytes.
- **"Pull-through cache vs. mirror" is a UX decision with security implications.** A "cache only what's been explicitly approved" mode (vs. "fetch on miss") is the strongest posture but the most operational burden. Both should be tenant-level options.

---

Want me to:
1. **Pick one and implement it** — my recommendation would be cooldowns (your idea, high impact, fits cleanly with what's already there), OR
2. **Sketch a "supply-chain" design doc** in `docs/` that picks a coherent v1 set and shows how they compose, OR
3. **Add the audit-log foundation first** — least exciting but everything else needs it.

Or something else from the list above.