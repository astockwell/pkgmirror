# Integration suite (real network)

This suite exercises pkgmirror's pull-through subsystem against the
**real** public package registries (pypi.org today; npm/RubyGems/etc.
as their per-format adapters ship).

## What it answers

> Did our pull-through still work end-to-end **against the real
> internet** since the last time we checked?

This is the only thing the integration suite is for. Everything else
(format-protocol conformance, internal correctness, security gating,
UI behavior) is covered by the unit + black-box suites. Don't expand
this suite to cover things those suites can answer.

## When it runs

| Trigger | Frequency | Where |
|---|---|---|
| `schedule:` cron | Weekly, Sunday 06:00 UTC | `.github/workflows/integration.yml` |
| `workflow_dispatch:` | On demand, per format | same |

**Never on push/PR.** PRs land too frequently to be a polite consumer
of public registries, and PR contributors can't fix upstream outages.

## When it fails

The workflow's `notify` job auto-files a tracking GitHub issue with the
run link (or comments on the existing open issue, to avoid dupes).

The first triage step is **"did tests SKIP with 'upstream unreachable'
or did they FAIL?"**. The suite distinguishes the two on purpose:

- **SKIP** = upstream is down. Close the issue; not our problem.
- **FAIL** = upstream is up + our code broke (or upstream drifted
  in a way we need to handle).

## Etiquette toward public registries

We are guests on pypi.org / registry.npmjs.org / etc. The suite is
designed to be a vanishingly small fraction of those services'
traffic; please keep it that way:

1. **Tiny canary set.** Three or fewer well-known stable packages per
   format. Resist the urge to "test more coverage" here — that's what
   the unit suite is for. Each new canary is a recurring tax on the
   upstream operator.

2. **Weekly, not daily.** A weekly cadence is plenty to catch upstream
   schema drift. Daily would be ~7× more traffic for ~0× more signal.

3. **Distinctive User-Agent.** The suite sends
   `pkgmirror-integration-tests/0.1 (+https://github.com/astockwell/pkgmirror; ci weekly)`
   so registry operators can find us if we misbehave. **Do not** strip
   this — anonymous bot traffic gets blocked, and rightly so.

4. **Conservative rate limit.** The pkgmirror process under test is
   configured for `FETCH_RPM_PER_TENANT=30`, well below what any test
   needs. This is a circuit breaker against a future runaway loop.

5. **No content-byte assertions.** Wheels and tarballs get rebuilt
   upstream over time. Assert "install succeeded" and "module is
   importable", **not** "wheel has exactly N bytes" or "first 8 bytes
   are `PK\x03\x04`".

## Running locally

```sh
# Full PyPI suite (3 tests, ~2-5 min, contacts pypi.org)
go test -tags=integration -timeout=15m -v ./tests/integration/pypi/...

# Single test
go test -tags=integration -timeout=15m -v -run TestPyPIPullThrough_InstallSix \
    ./tests/integration/pypi/...
```

You need a working Docker daemon (the suite uses
[testcontainers-go](https://golang.testcontainers.org/)) and outbound
HTTPS to pypi.org.

If pypi.org is unreachable from your network, the preflight will skip:

```
--- SKIP: TestPyPIPullThrough_InstallSix (0.05s)
    preflight.go:54: integration: pypi.org unreachable (...); skipping - cannot tell whether our code regressed
```

That's the **correct** behavior — the suite cannot distinguish "our code
broke" from "upstream is unreachable" without a working network. Skip is
honest; fail would be a false positive.

## Architecture

```
tests/blackbox/harness/    # shared (build tag: blackbox || integration)
    pkgmirror.go             Stack, Start, StartWithOptions
    client.go                ClientSpec, Client.Exec / MustExec
    upload.go                helpers for tests that POST artifacts

tests/integration/         # this suite (build tag: integration only)
    doc.go                   package overview + etiquette policy
    stack.go                 StartStack: harness.StartWithOptions with
                             cache_and_serve + polite User-Agent +
                             conservative rate cap
    preflight.go             RequireReachable: t.Skip on upstream down
    pypi/canary_test.go      three install tests (pip / six / urllib3)
    go/canary_test.go        three install tests (rsc.io/quote / google/uuid)
    rubygems/canary_test.go  three install tests (rake / thor / cache reuse)
```

Future per-format additions go under `tests/integration/<format>/`. Each
should:

1. Define a `<format>ProbeURL` and call `integration.RequireReachable`
   at the top of every test.
2. Bring up the stack via `integration.StartStack`.
3. Add itself to `.github/workflows/integration.yml`'s job matrix (and
   to the `workflow_dispatch.inputs.format` enum so contributors can
   target a single format manually).

## Adding a canary package

Before adding a fourth package to any format's canary list, ask:

- Does this catch a class of regression the existing canaries miss?
- Is this package as stable + boring as `six` / `pip` / `urllib3`?
- Will it still exist in 5 years?

If yes to all three, add it. If unsure, the answer is probably "use a
black-box test with an in-process fixture instead."

## Secrets

The scheduled workflow uses **no secrets** beyond the default
`GITHUB_TOKEN` (for the `notify` job's `issues: write` permission).

Anything that needs a private upstream credential (e.g. a private
GHEC PyPI mirror in PR Q+ territory) lives in a separate workflow,
runs only on `workflow_dispatch` with explicit reviewer-approved
secrets, and is **never** on the schedule. Public registries get
zero credentials from us.
