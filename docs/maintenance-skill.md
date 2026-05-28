# Maintenance skill: quarterly health pass

**For an LLM (or human) sitting down to do a scheduled pkgmirror
maintenance pass.** This is a runbook, not a tutorial — read
[long-term-maintenance.md](long-term-maintenance.md) first if you
want context on *why* each step exists.

The pass takes ~1–2 hours wall time, most of which is image pulls
and stress-loop runs. The actual decisions are quick.

---

## Inputs the operator must supply

Before starting, confirm with the user:

1. **Scope:** "full" (every step) or "fast" (skip §F + §G)
2. **Authorization to bump deps:** minor + patch only, or major too?
3. **Authorization to bump Docker tags:** same question, per
   image's major
4. **Branch to work on:** new branch `maintenance/<YYYY-Q>` is the
   default; ask if uncertain

Default to **fast scope, minor+patch only, dedicated branch** if
you can't reach the user.

---

## §A. Snapshot the starting state

```sh
git status --porcelain   # must be empty
git checkout -b maintenance/$(date +%Y-Q$(( ($(date +%-m) - 1) / 3 + 1 )))
```

Run the baseline test loop **before** any changes so you can
distinguish "broken by my change" from "already flaky":

```sh
make test         # unit + grey-box (~30s)
```

If this fails on a clean checkout, **stop and report.** The
maintenance pass is not the place to debug pre-existing failures.

---

## §B. Go module + toolchain hygiene

Per [long-term-maintenance.md §4 + §5](long-term-maintenance.md#4-go-module-dependencies).

```sh
# What's available?
go list -m -u all 2>&1 | grep -v "indirect" | grep -E "\[" || echo "all up to date"

# Known vulns reaching our call graph?
go install golang.org/x/vuln/cmd/govulncheck@latest
govulncheck ./...
```

### Decisions

- **Patch + minor bumps:** apply with `go get foo@latest` for each
  dep, then `go mod tidy`. Re-run `make test`. If green, commit
  with a message naming each bumped package.
- **Major bumps:** ask the user before applying. Major bumps for
  `gin`, `gin-gonic`, `sqlite`, `testcontainers-go`,
  `ProtonMail/go-crypto`, or `go-rpmutils` are high-risk and
  deserve their own PR.
- **govulncheck findings:** apply the minimum-version bump that
  closes each finding. If the fix is in a major version we haven't
  authorized, surface to user.

### Go toolchain

Check if a new Go minor was released since `go.mod`'s `go` line:

```sh
GO_DECLARED=$(awk '/^go / {print $2}' go.mod)
echo "Current go.mod toolchain: $GO_DECLARED"
echo "Visit https://go.dev/dl/ to see the latest stable release"
```

If a bump is warranted (Feb / Aug release cadence), update
**all four** locations:

1. `go.mod` — the `go` directive
2. `.github/workflows/ci.yml` — `go-version: "X.Y"` (two jobs)
3. `Dockerfile` + `Dockerfile.rootless` — `FROM golang:X.Y-...`
4. `README.md` — the "Go X.Y+" intro line

Run `make test` after.

---

## §C. Docker image tags in blackbox tests

Per [long-term-maintenance.md §3](long-term-maintenance.md#3-pinned-client-image-tags-in-blackbox-tests).

For each format, the pinned image lives in **up to four** places:

1. `tests/blackbox/<fmt>/conformance_test.go` — the `Image:` field
2. `.github/workflows/ci.yml` — the `images=(...)` array
3. `docs/adding-a-format.md` — the per-format recipe + the
   quick-reference matrix
4. `README.md` — examples in the "Using the X registry" section
   (only if the image is cited)

### Discovery loop

For each pinned tag, check whether the upstream image has a newer
patch tag in the same major:

```sh
# Example for one image; loop in practice
docker pull alpine:3.20 >/dev/null
docker pull alpine:latest >/dev/null
docker image inspect alpine:3.20 --format '{{.Id}}'
docker image inspect alpine:latest --format '{{.Id}}'
# Different digests → newer patch likely available; check Docker Hub
# tags page for the next 3.x or new major
```

A faster shortcut is the GitHub `crane ls` flow:

```sh
docker run --rm gcr.io/go-containerregistry/crane:latest \
    ls library/alpine | grep '^3\.' | sort -V | tail -5
```

### Apply bumps one image at a time

Bumping multiple images in one commit obscures which one broke
something. For each image you decide to bump:

1. Update the tag in all four locations above.
2. Run *that format's* blackbox: `make test-blackbox-<fmt>`.
3. If green, commit with a message naming the format + new tag.
   If red, revert the change and surface to user — likely the
   new image's client changed something that breaks our
   blackbox.

### Don't auto-bump majors without user OK

A new major often ships a behavior change (e.g. `dnf 5` →
`dnf 6`). Surface and ask.

---

## §D. Forgejo upstream port diff

Per [long-term-maintenance.md §2](long-term-maintenance.md#2-forgejo-upstream-port-drift).

```sh
cd ../forgejo
git fetch origin
git log --oneline HEAD..origin/forgejo
```

If there are commits, for each format we ship, see what changed
in its upstream sources:

```sh
# Replace <fmt> per format
git log --oneline --since="3 months ago" -- \
    routers/api/packages/<fmt>/ \
    modules/packages/<fmt>/ \
    services/packages/<fmt>/
```

For each non-trivial commit:

- **Wire-format change** → port it. Update our parser/handler.
  Add a grey-box test if upstream added one.
- **Bug fix** → port it.
- **Refactor / orchestration / tests** → ignore (we don't share
  that layer).
- **New optional capability** → file as a roadmap item, don't
  port reactively.

After porting, update the relevant entry in `ATTRIBUTIONS.md` if
the file path moved upstream.

If the diff is large, surface to user before going through it
mechanically — they may want to scope this to one format per
pass.

---

## §E. Stress loop (always)

Per the playbook: a single test pass papers over flakes that load
exposes.

```sh
for i in $(seq 1 10); do
    /usr/local/go/bin/go test ./... -count=1 -timeout 300s 2>&1 >/tmp/m-$i.log
    if tail -3 /tmp/m-$i.log | grep -q FAIL; then
        echo "run $i FAIL"
        grep "FAIL: Test" /tmp/m-$i.log
    else
        echo "run $i ok"
    fi
done
```

10/10 green is the bar. Anything less is a flake worth
investigating *before* any of the changes above land.

---

## §F. Blackbox suite against latest tags (full scope only)

This is the "discover wire-breaks before users do" step.

For each format, run the blackbox with `latest` instead of the
pinned tag. The easiest way is to set an env override the test
honors, but since the harness doesn't currently expose that:

```sh
# Per format, temporarily edit conformance_test.go's Image field
# to use `latest`, then run:
make test-blackbox-<fmt>
# Revert the edit.
```

If a `latest`-tag run **fails** while the pinned-tag run passes:
the client has changed its behavior in a way that breaks our
wire format. Open an issue with the failure + the diff between
old and new tag's behavior.

If `latest` passes too, no action needed.

This is tedious — automate by adding a separate
`.github/workflows/blackbox-latest.yml` that does this once a
week. Tracked as a roadmap item; not part of the quarterly pass
yet.

---

## §G. Client-gotcha rotation (full scope only)

Per [long-term-maintenance.md §6](long-term-maintenance.md#6-documented-client-gotchas-that-may-stop-being-gotchas).

Pick **one** format whose blackbox test has a "client gotcha"
workaround. Temporarily disable the workaround in the test, run
the blackbox, and:

- **Still fails** → gotcha is still real, revert the change, no
  doc update.
- **Now passes** → upstream fixed it. Remove the workaround from
  the test + the docs (`adding-a-format.md` recipe + `README.md`
  section).

Rotate through formats so each is rechecked roughly yearly
(one per quarter ≈ 4 formats × 3 years to cover all 12).

Suggested order: maven (HTTP blocker), debian (auth.conf prefix +
gnupg-not-shipped), rpm (dnf 5 credentials), nuget (dotnet add
--source).

---

## §H. Update DECISIONS.md

Append a single dated entry capturing:

- What was bumped (deps, images, toolchain)
- What was ported from Forgejo
- What gotchas were re-tested + their outcome
- Any items surfaced to user

Template:

```markdown
## YYYY-MM-DD — Quarterly maintenance pass

**Deps bumped:** foo v1.2.3 → v1.3.0 (CVE fix), bar v0.5.0 → v0.6.0.
**Images bumped:** alpine:3.20 → 3.22, node:22-bookworm unchanged.
**Forgejo diff:** debian metadata.go patched on upstream `abc1234`;
ported to internal/packages/debian/parser.go.
**Gotcha rotation:** retested maven HTTP blocker — still real.
**Surfaced to user:** govulncheck flagged GHSA-xxx in transitive
modernc.org/sqlite; deferred to a dedicated PR per scope agreement.

**Net result:** stress loop 10/10 green; all blackbox green.
```

---

## §I. Commit + report

```sh
git log --oneline maintenance/$(date +%Y-Q*)..HEAD
```

Group commits into one PR per logical change (deps bump, image
bumps, forgejo ports). Don't bundle.

Final report to user:

- Bullet list of what changed
- Bullet list of what was deferred (and why)
- Bullet list of anything *strange* the pass surfaced (test flakes
  that recovered, deps that bumped but didn't show in
  `list -m -u`, etc.)

---

## What this skill **does not** do

These need explicit user direction:

- **Major version bumps** of any direct dep
- **Major version bumps** of any Docker image (e.g. `fedora:41 →
  fedora:42`)
- **Schema / migration changes**
- **Adding a new format** (use [adding-a-format.md](adding-a-format.md))
- **Closing roadmap items** (cargo, composer, conan, conda, helm,
  pub, swift, alt, arch, vagrant, chef)
- **Anything that touches `policy/`, `auth/`, `audit/`, or
  `admin/`** — these are the supply-chain spine and warrant a
  human reviewer

If a maintenance step seems to need one of these, **stop and
surface.** The skill's job is hygiene, not architecture.
