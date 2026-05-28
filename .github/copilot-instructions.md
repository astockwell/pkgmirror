# pkgmirror — Copilot instructions

Standalone multi-format package mirror with supply-chain controls.
Twelve formats shipped (go, pypi, npm, rubygems, container, generic,
alpine, maven, debian, rpm, nuget, cran), plus a working policy +
audit engine. Built in Go; SQLite + filesystem storage; no external
runtime dependencies.

## Stack and conventions

- **Go 1.26** at `/usr/local/go/bin/go`. Module path
  `github.com/astockwell/pkgmirror`. Gin v1.10 HTTP router,
  `modernc.org/sqlite` pure-Go SQLite (PRAGMA user_version v3),
  `testcontainers-go` for blackbox.
- **gofmt -s -w**. No bespoke linter beyond `go vet`. The Makefile
  `vet` target depends on `check-package-dupes` (see Pitfalls).
- Reach for `make` targets before raw `go` commands. `make help`
  lists everything.

## Read these before editing

When the task touches a format's wire layer or a new format port:

- [docs/adding-a-format.md](docs/adding-a-format.md) — the
  playbook. **Read end-to-end** before adding a format. Codifies
  the cross-cutting patterns and per-format recipes.
- [docs/blackbox-testing.md](docs/blackbox-testing.md) — harness
  contract.
- [docs/auth.md](docs/auth.md) — per-format credential mechanics.
- [docs/storage.md](docs/storage.md) — content-addressed blob
  store contract.
- [docs/supply-chain.md](docs/supply-chain.md) — policy engine
  architecture (cooldown, license allowlist, quarantine, audit).
- [docs/known-deviations-from-spec.md](docs/known-deviations-from-spec.md)
  — places we intentionally diverge from a canonical spec.
- [DECISIONS.md](DECISIONS.md) — running log. Append a dated entry
  for any non-obvious choice.
- [ATTRIBUTIONS.md](ATTRIBUTIONS.md) — every Forgejo-derived file
  has an entry here. Adding a derived file? Add an entry.

For long-term hygiene work see
[docs/long-term-maintenance.md](docs/long-term-maintenance.md) and
[docs/maintenance-skill.md](docs/maintenance-skill.md).

## Load-bearing patterns (already codified — reach for these)

- **`internal/syncutil.ExclusivePool`** — refcount-driven per-key
  mutex pool. Use whenever a format publishes multiple files per
  coordinate (Maven, Debian, NuGet) or lazily generates a per-
  tenant signing key (Alpine, Debian, RPM). Key shape:
  `fmt.Sprintf("%d|%s", tenant.ID, scope)`.
- **On-demand index generation.** Never persist index files
  (PACKAGES, APKINDEX, repomd, Release, etc.) — build per-GET
  from the live file list. Forgejo persists; we don't. Tradeoffs
  documented in the playbook.
- **Stable-Bytes for signed-on-demand content.** When a format
  signs a generated index (Debian Release.gpg, RPM repomd.xml.asc),
  derive every non-constant field (`Date:`, `<timestamp>`) from
  `max(file.created_unix)` rather than `time.Now()`. The signature
  has to validate across two separate requests for body + sig. The
  resulting wire deviation goes in
  [known-deviations-from-spec.md](docs/known-deviations-from-spec.md).
- **Composite-key file naming** for multi-arch / multi-component
  formats. Encode the coordinate into the file row's `Name` as
  `<dim1>|<dim2>|...|<basename>` so `UNIQUE(version_id, name)`
  holds without a schema change. See Alpine, Debian, RPM, CRAN.
- **HEAD support** alongside every GET that may be cache-validated
  (Gradle, OCI, NuGet, dnf, apt). Gin does not auto-derive HEAD.
- **License extraction → policy engine.** Every parser that can
  read an SPDX expression surfaces it on the parsed metadata.
  Handler hands it to `policy.Subject.Attrs["license"]` AND
  `models.SetLicense(ver.ID, lic)`.
- **Catch-all `*path`** for hierarchical URL shapes (Maven,
  Container, Generic, NuGet `/registration/*tail`, CRAN
  `/src/contrib/*tail`). Strip the leading `/` gin includes.

## Critical pitfalls (cost real debugging time before)

- **Always `buf.Seek(0, io.SeekStart)` before handing a HashedBuffer
  to a streaming parser.** `Service.NewHashedBuffer` drains the body
  to compute hashes; the underlying file is at EOF. Bit CRAN on
  first run; rediscoverable on any new format.
- **Every grey-box fixture's `t.Cleanup` needs an explicit
  `_ = os.RemoveAll(dir)` after `db.Close()`.** macOS APFS's
  `t.TempDir` auto-cleanup races SQLite WAL teardown. Load-bearing.
- **Drain `rows.Next()` into a slice BEFORE issuing per-row
  sub-queries.** Sibling test writers cause `SQLITE_BUSY (5)` and
  a 5-second timeout. Pattern in `alpine/handler.go`,
  `debian/handler.go`, `rpm/handler.go`, `cran/handler.go`.
- **Auto-formatter sometimes duplicates the `package X` line.**
  Guarded three ways: `tools/dedup-package/` Go tool,
  `.githooks/pre-commit`, CI vet prerequisite. If `make check-
  package-dupes` fails: run `make fix-package-dupes`. The hook is
  active after `make install-hooks`.
- **`tar.Writer` body padding** is only written on the next
  `WriteHeader` or `Close`. If you finalize a gzip stream around a
  single-entry tar without calling `tw.Flush()`/`tw.Close()`, the
  bytes are short. Cost: Alpine 2048-bit signatures.
- **Stress loop, always.** Before declaring any change done:
  `for i in $(seq 1 10); do go test ./... -count=1 -timeout 300s || break; done`.
  A single pass papers over WAL races, RSA-keygen timeouts, and
  test-ordering bugs.

## When to run what

| Goal | Command |
| --- | --- |
| Fast feedback (no docker) | `make test` |
| Race detector | `make test-race` |
| Full per-format blackbox | `make test-blackbox-<fmt>` (need docker) |
| Full blackbox sweep | `make test-blackbox` |
| Update deps (patch only) | `make deps-update-patch` |
| Pre-commit baseline | `make vet && make test` |

`go test` without `-count=1` will use cached results; for "did I
break anything?" runs use `-count=1`.

## Working flow for new code

1. **Read the playbook** if touching a format or adding one.
2. **Port from Forgejo** when possible; transliteration beats
   invention. The `forgejo/` sibling clone is the reference.
   Add SPDX + attribution headers on derived files (template in
   `internal/packages/goproxy/parser.go`).
3. **Implement, then grey-box** with `httptest.NewServer`. Cover
   401 anon, 201 happy path, 409 dup, 404 missing.
4. **Wire the route** in `internal/server/server.go`
   (one `r.Group(...)` + `Register` call).
5. **Add the blackbox** in `tests/blackbox/<fmt>/conformance_test.go`
   driving the real client. Pre-pull the image in
   `.github/workflows/ci.yml`'s `images=(...)`.
6. **Stress loop** before declaring done.
7. **DECISIONS.md** entry for any non-obvious choice.
8. **ATTRIBUTIONS.md** entry for any Forgejo-derived file.
9. Update README format count + the playbook's quick-reference
   matrix.

## What to surface vs do silently

**Do without asking:**

- Add tests, fix bugs you find, port from Forgejo, follow the
  playbook end-to-end, fix typos, update DECISIONS.md +
  ATTRIBUTIONS.md, run the stress loop.

**Stop and surface:**

- Schema or migration changes (the SQLite v3 schema is a deliberate
  contract).
- Anything in `internal/policy/`, `internal/auth/`,
  `internal/audit/`, or `internal/admin/` beyond the format-level
  hooks the playbook describes — supply-chain spine, deserves a
  human reviewer.
- Major version bumps of any direct dep (`gin`, `sqlite`,
  `testcontainers-go`, `go-rpmutils`, `ProtonMail/go-crypto`).
- Removing a documented client gotcha or known-deviation entry
  without first re-running the relevant blackbox to confirm it's
  obsolete.
- Bypassing `Service.NewHashedBuffer` / `CreatePackageOrAddFileToExisting`
  — they own the hash pipeline that the policy engine depends on.

## Conventions for chat answers

- Linkify file references with workspace-relative paths. Don't
  wrap filenames in backticks.
- Be concise. The user is a staff engineer; skip the recap.
- After a multi-commit session, summarize what landed.
- Don't add docstrings, comments, or refactors to code you didn't
  modify.
