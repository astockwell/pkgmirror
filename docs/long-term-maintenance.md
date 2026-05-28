# Long-term maintenance friction points

This document enumerates the places where pkgmirror's correctness or
freshness will drift over time even if nobody is actively changing
the code. Each entry covers:

- **what** drifts
- **how it surfaces** if undetected
- **how (or whether) we can detect it** ahead of a user-facing break
- **who owns the fix** — automatable, LLM-runnable, or "wait for a
  user report"

It is a companion to [maintenance-skill.md](maintenance-skill.md),
which is a step-by-step checklist a future LLM (or human) can run
on a quarterly cadence. Anything detectable should be in the
checklist; anything not is called out here explicitly so future-us
isn't surprised.

The intended reader is the person (or LLM) sitting down to do a
maintenance pass three to six months after the last one.

---

## 1. Upstream spec drift (per format)

Every format we ship implements an external wire spec maintained
by a community we don't control. When that spec evolves, our
implementation is silently wrong until a client breaks.

| Format | Authoritative spec | Drift signal we have | Drift signal we don't have |
| --- | --- | --- | --- |
| `go` | [go.dev/ref/mod#goproxy-protocol](https://go.dev/ref/mod#goproxy-protocol) + [zip layout](https://go.dev/ref/mod#zip-files) | Real `go` toolchain blackbox catches wire breaks. The Go team is conservative; major proxy changes ship in `go` release notes. | New optional endpoints (no `go list` regression) |
| `pypi` | [PEP 503](https://peps.python.org/pep-0503/), [PEP 691](https://peps.python.org/pep-0691/), [PEP 740](https://peps.python.org/pep-0740/), [PEP 658](https://peps.python.org/pep-0658/) | PEPs have an active index at [peps.python.org](https://peps.python.org). Blackbox runs real `pip install`. | New PEPs that add optional capabilities (we won't fail, we just won't advertise) |
| `npm` | [npm/registry](https://github.com/npm/registry/) docs | Blackbox runs real `npm publish` + `npm install`. | Undocumented quirks shipped by the npm CLI |
| `rubygems` | [guides.rubygems.org/rubygems-org-api](https://guides.rubygems.org/rubygems-org-api/) | Blackbox runs real `gem push` + `bundle install`. Ruby `Marshal` format is frozen. | New compact-index features |
| `container` | [OCI Distribution Spec](https://github.com/opencontainers/distribution-spec) | **Most active spec.** OCI publishes versioned releases. Blackbox runs `crane`. | Spec **v1.2** when it lands |
| `generic` | Our own contract | Doesn't drift. | n/a |
| `alpine` | [APKINDEX format](https://wiki.alpinelinux.org/wiki/Apk_spec) | Blackbox runs real `apk add`. Format is convention-driven, evolves slowly. | New optional APKINDEX fields |
| `maven` | [Maven repository layout](https://maven.apache.org/repository/layout.html) | Blackbox runs real `mvn deploy`/`mvn dependency:get`. Layout is de-facto frozen. | Maven 4 deployment-time changes |
| `debian` | [DebianRepository/Format](https://wiki.debian.org/DebianRepository/Format) | Blackbox runs real `apt update`. Long-tail wiki page; check on `apt` major releases. | New optional Release fields |
| `rpm` | createrepo metadata + repomd.xml schema (not formally an RFC) | Blackbox runs real `dnf makecache`. dnf 5 is the modern client; dnf 6 may shift things. | Schema additions clients tolerate but we don't emit |
| `nuget` | [NuGet Server API v3](https://learn.microsoft.com/en-us/nuget/api/overview) | Blackbox runs real `dotnet add package`. Microsoft maintains a versioned doc. | NuGet "Symbol Server v2" protocol features (we skip symbols) |
| `cran` | Convention-driven; [tools::write_PACKAGES](https://stat.ethz.ch/R-manual/R-devel/library/tools/html/PACKAGES.html) is the reference impl | Blackbox runs real `install.packages()`. CRAN evolves on R minor-version cadence. | New optional PACKAGES fields |

### What we *can* automate

- Run each blackbox test on the latest published patch tag of its
  client image, weekly or monthly. A failure surfaces wire-breaks
  introduced by a client-side change.
- Subscribe a maintainer to release announcements for the most
  active specs (OCI Distribution, Go modules, dotnet SDK majors).
- Track NIST CVE / GitHub Security Advisories for the parser libs
  we depend on (`go-rpmutils`, `blakesmith/ar`, `ulikunitz/xz`,
  `ProtonMail/go-crypto`).

### What we *cannot* easily automate

- "Did the spec PDF / wiki page text change in a way that affects
  us?" There is no machine-readable changelog for `wiki.debian.org`,
  `createrepo.baseurl.org`, or `peps.python.org` per se. PEPs do
  have a fixed-ID convention so we can subscribe to ranges (`PEP-700+`).
- "Did Forgejo upstream fix a parser bug we ported verbatim?" See
  §2.

### When a drift fires

- **Wire break in blackbox:** highest signal. CI tells us
  immediately if we run blackbox on a recent enough cadence.
- **Wire break in production (user report):** lower signal, higher
  cost. We mitigate by making blackbox actually-pull-the-latest-tag
  one channel and pinned-tag the other (see §3).

---

## 2. Forgejo upstream port drift

Every format's parser + handler is a transliteration of a Forgejo
file (see [`ATTRIBUTIONS.md`](../ATTRIBUTIONS.md)). When Forgejo
patches a parser bug or adds a metadata field, our copy diverges
silently. The risk is not breakage (Forgejo is the reference impl;
their fixes are correct) — it's that we miss security or
correctness fixes that should land here too.

### Tracking signal we have

- `forgejo/` sibling clone alongside this repo (not committed; the
  contributor's local working copy).
- `ATTRIBUTIONS.md` lists the specific Forgejo path each file was
  ported from.

### Tracking signal we lack

- No automated diff between our ported files and the upstream
  source. A bug fix Forgejo lands on, say,
  `modules/packages/debian/metadata.go` is invisible to us.
- No commit-pinning. We don't record "we ported from Forgejo @
  commit `abc1234`" so a "what changed upstream" query has nothing
  to anchor on.

### What we *can* do

- **Pin Forgejo at port time.** When we port a file, record the
  upstream HEAD commit in an `ATTRIBUTIONS.md` footer or alongside
  the SPDX header. A future maintenance pass can `git -C ../forgejo
  log <commit>..HEAD -- modules/packages/<fmt>/` to enumerate
  changes.
- **Quarterly diff pass.** For each format, walk the upstream files
  named in `ATTRIBUTIONS.md` and diff them against the last-known-
  ported version. Surface any non-cosmetic change for human review.

### What we *can't* automate

- "Forgejo refactored their orchestration layer (`services/packages/...`)"
  vs "Forgejo changed wire format" — same git diff, very different
  significance. Needs a read-the-changes review.

### Embedded fixtures borrowed from upstream

Today only **one** file embeds a Forgejo-sourced binary blob: the
RPM grey-box and blackbox tests embed a base64+gzipped
`gitea-test 1.0.2-1.x86_64.rpm` fixture. If Forgejo refreshes
that fixture, our tests are testing against a frozen-in-time copy.
Not a correctness issue — the bytes are unchanging — but worth
noting in the friction inventory.

Every other format builds its blackbox fixtures programmatically
(see `tests/blackbox/<fmt>/conformance_test.go` `makeXxx` helpers),
so this category is shallow.

---

## 3. Pinned client image tags in blackbox tests

`tests/blackbox/<fmt>/conformance_test.go` and
`.github/workflows/ci.yml` pin every client to a specific image
tag. Today (May 2026) those pins are:

| Format | Image | Concern |
| --- | --- | --- |
| `goproxy` | `golang:1.22-bookworm` | Go 1.26 is current; 1.22 is N-2 |
| `pypi` | `python:3.12-slim` | Python 3.13 is current |
| `npm` | `node:22-bookworm` | Node 22 LTS — fine |
| `rubygems` | `ruby:3.3-slim` | Ruby 3.3 — fine |
| `container` | `gcr.io/go-containerregistry/crane` (latest) | Drifts on rebuild |
| `generic` | `curlimages/curl:8.10.1` | curl 8.11+ available |
| `alpine` | `alpine:3.20` | 3.22 is current |
| `maven` | `maven:3.9-eclipse-temurin-21` | Maven 4.x is in early-access |
| `debian` | `debian:bookworm-slim` | Trixie (Debian 13) released |
| `rpm` | `fedora:41` | Fedora 42 released |
| `nuget` | `mcr.microsoft.com/dotnet/sdk:8.0` | .NET 9 LTS released |
| `cran` | `r-base:4.4.3` | R 4.5.x released |

### What we *can* automate

- A scheduled GitHub Actions workflow (e.g. weekly) that runs the
  blackbox suite with the **latest** tag of each image (`alpine:latest`,
  `python:3-slim`, etc.). When a future client release breaks our
  wire format, we find out from a green-on-pinned, red-on-latest
  signal.
- Dependabot / Renovate watching the pinned tags and opening PRs
  with version bumps. Renovate has good Docker tag support.

### What we *should not* automate fully

- Auto-merge of image bumps. Each bump should run the blackbox
  test for that format and surface the result. The bump itself is
  mechanical; the review is "did the new image's client change its
  output format in a way that breaks our assertions?"

### Where the pin lives (every format has up to three)

1. The image tag in `tests/blackbox/<fmt>/conformance_test.go`
2. The pre-pull entry in `.github/workflows/ci.yml`'s `images=()` array
3. A reference in `docs/adding-a-format.md`'s per-format recipe + matrix
4. A reference in `README.md`'s "Using the X registry" section (where
   examples cite the image)

Bumping requires updating all four. Easy to miss one. The
maintenance checklist accounts for it.

---

## 4. Go module dependencies

`go.mod` pins direct deps with semver. Indirect deps come along
for the ride. Both rot in two directions:

- **Patch / minor releases** containing bug or security fixes
- **Major releases** containing breaking changes (Go conventionally
  re-imports as `/v2`, so this is usually obvious)

### What we *can* automate

- `go list -m -u all` enumerates available upgrades. Trivial to run.
- `govulncheck ./...` enumerates known CVEs that actually reach
  the call graph (not just "this version of foo has a CVE
  somewhere"). The skill checklist runs both.
- Renovate or Dependabot to open the PRs. Today this repo has
  **neither** wired up.

### What we *can't* fully automate

- "Did this upgrade change the behavior of a public function we
  depend on?" govulncheck doesn't surface behavior changes, only
  vulnerabilities. The stress loop + blackbox suite is our
  backstop.

### Critical deps to watch (formats break if these regress)

| Dep | Used by | Why critical |
| --- | --- | --- |
| `github.com/gin-gonic/gin` | every handler | HTTP router; behavior change ripples |
| `modernc.org/sqlite` | every test, every prod read | Pure-Go SQLite; the WAL race we hit was here |
| `github.com/sassoftware/go-rpmutils` | RPM parser | Binary RPM header format |
| `github.com/blakesmith/ar` + `ulikunitz/xz` + `klauspost/compress/zstd` | Debian parser | `.deb` decompression |
| `github.com/ProtonMail/go-crypto` | Debian + RPM signing | OpenPGP signing; the maintained successor to `golang.org/x/crypto/openpgp` |
| `github.com/hashicorp/go-version` | NuGet + others | SemVer normalization |
| `github.com/testcontainers/testcontainers-go` | every blackbox | Docker orchestration |

---

## 5. Go toolchain + tooling versions

| Pin | Lives in | Owner |
| --- | --- | --- |
| `go 1.26` directive | `go.mod` | bump on Go major release (Feb / Aug cadence) |
| `go-version: "1.26"` | `.github/workflows/ci.yml` | mirror of above |
| `GO ?= go` | `Makefile` | inherits whatever's on PATH; no pin |
| README quickstart | `README.md` | mentions "Go 1.26+" as an intro |
| Dockerfile | `Dockerfile` + `Dockerfile.rootless` | bump on Go bump |

When we bump Go we should bump all five. The skill checklist has
a single step for this.

---

## 6. Documented client gotchas that may stop being gotchas

Each format's recipe in `docs/adding-a-format.md` has a "Client
gotchas" subsection. Several document specific client bugs or
quirks that may get fixed upstream:

- **Maven 3.8.1+ HTTP blocker.** If Maven publishes a saner default
  or a config-free workaround, our `<mirror>` snippet becomes
  outdated.
- **apt 2.x `http://` prefix on `auth.conf`.** Could relax in apt 3.x.
- **bookworm-slim missing gpg.** Could ship by default in a future
  slim variant.
- **dnf 5 credential helper fussiness.** Active development;
  dnf 5.x may grow native PAT support.
- **`dotnet add package --source <name>` treats name as path.**
  Could be fixed in a future dotnet SDK.

### What we *can* automate

- Periodically re-run the blackbox without the workaround applied
  (e.g. `apt` test without the `http://` prefix in `auth.conf`).
  If it suddenly passes, the gotcha is gone and the docs can be
  trimmed.

### What we *can't* fully automate

- Knowing *which* gotchas to retest. The maintenance checklist
  suggests a quarterly rotation: one format per quarter, retest
  the gotcha block.

---

## 7. "Hard problems" we're choosing to accept

These items have no good preventive solution; we accept the
risk of finding out reactively.

### Real-world client implementations that don't follow spec

Most blackbox tests drive **one** canonical client. There are
many other clients in the wild (e.g. `pnpm`, `yarn`, `bun`,
`packagecloud-cli`, `paket`, `cabal`, …) that may have their
own quirks. We'll discover those when a user reports a bug.

**Mitigation:** when a non-blackbox client is reported broken,
add a blackbox test for it before fixing. Subsequent maintenance
runs then catch regressions.

### Bug-compatible behavior we copied from Forgejo

The `cran.rvserion` typo in CRAN's property key is an example.
If Forgejo fixes it, we'd notice on the §2 diff pass — but only
if we're looking at the right file. There may be similar
typos / undocumented choices we copied without noticing.

**Mitigation:** the §2 diff pass surfaces these eventually.

### Performance regressions under sustained load

We have no load test. Per-request latencies are sub-100ms for
all on-demand-index formats today, but a future schema change or
a deeply-paginated registration index could regress that without
any test catching it.

**Mitigation:** none currently. If a user reports it, write a
benchmark + add a baseline.

### Storage growth + GC

The content-addressed blob store deduplicates by hash but never
collects unreferenced blobs. Long-running deployments will leak
disk space when versions are deleted via `DELETE /…/<version>`.

**Mitigation:** this is a real product gap, not a maintenance
gap. Tracked separately as a roadmap item.

---

## 8. What's *not* on this list (because it's fine)

- **The `tools/dedup-package/` guard** — self-tests, no upstream.
- **The `internal/syncutil/ExclusivePool`** — ported once, no
  upstream churn worth tracking.
- **Our own protocol (the `/admin`, `/v2`, generic format)** — we
  define it, so it can't drift.
- **JS / Tailwind toolchain** — we don't currently use any.
- **Database schema** — versioned; migrations go through a
  controlled path. Drift is impossible by construction.

---

## How to run a maintenance pass

See [maintenance-skill.md](maintenance-skill.md) for the
step-by-step checklist.

Recommended cadence:

- **Quarterly** (suggested calendar dates: 1st Mon of Mar / Jun /
  Sep / Dec): full pass per the skill doc
- **On security advisory** for any direct dep listed in §4: bump
  immediately, don't wait for the quarterly
- **On Go release** (Feb / Aug): bump §5
- **On a user-reported wire break**: file an issue with the
  blackbox repro, then add a permanent blackbox test before fixing
