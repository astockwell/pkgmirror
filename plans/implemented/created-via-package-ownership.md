# `packages.created_via` — first-touch ownership for tenant packages

**Status:** shipped 2026-05-31 across 4 commits.

- PR A (`919b2a0`): migration v6 + models layer + signature change.
- PR B (`2d256b6`): PyPI handler wiring + 5 provenance tests.
- PR C (`7e7b451`): console badge + admin flip form + 4 console tests.
- PR D: docs/supply-chain.md + README + demo talking point.

**One-line:** Add `packages.created_via TEXT` so the `/simple/<name>/`
merge knows whether a package was tenant-uploaded (don't pull through)
or first arrived via upstream (merge upstream into the local view).

Post-implementation notes
-------------------------

- The plan recommended 4 PRs at ~1.75 days; actual was 4 commits in
  one focused session. No surprises in the data model; the typosquat
  + insider-shadow tests pass exactly as drafted in the plan's §6.
- The 409 body text from the plan worked verbatim; tests assert on
  the package name, "mirrored from upstream", and the console URL
  fragment - which matches the plan's intent.
- One minor deviation: the admin flip's confirm dialog uses two
  distinct messages (uploaded→pull_through vs pull_through→uploaded)
  to spell out the security implication in BOTH directions, rather
  than one generic confirm. Plan §5.3 anticipated this as an
  engineer-side decision.
- Cross-format wiring still pending. The column is universal but only
  the PyPI handler consumes it today. npm/rubygems/etc. handlers will
  set CreatedViaUploaded on upload + CreatedViaPullThrough on
  pull-through ingest as their pull-through PRs land. See the plan's
  §5.2 for the per-format pattern.
- The Go pull-through (which shipped before this plan) currently has
  no upload endpoint of its own, so the row provenance is always
  `pull_through` for go modules - the gate behaves correctly but the
  insider-upload defense isn't applicable. Follow-up if the go module
  upload endpoint grows.

--- ORIGINAL PLAN BELOW ---

# `packages.created_via` — first-touch ownership for tenant packages

**Status:** plan / ready for review. Not yet implemented.

**One-line:** Add `packages.created_via TEXT` so the `/simple/<name>/`
merge knows whether a package was tenant-uploaded (don't pull through)
or first arrived via upstream (merge upstream into the local view).

---

## 1. The problem this solves

The recent merge in [internal/packages/pypi/handler.go `packageIndex`](../internal/packages/pypi/handler.go)
makes `/simple/<name>/` return *both* local-persisted files and any
freshly-allowed upstream files for a known-local package. That's the
right behavior for packages we pulled through — turn a cooldown rule
off, resync, get the newly-available version — but it's the wrong
behavior for tenant-uploaded packages, because it opens a typosquat
hole:

> A tenant uploads `acme-internal-lib v1.0.0`. An attacker registers
> `acme-internal-lib v0.9.0` on pypi.org. `pip install
> acme-internal-lib<1.0` against pkgmirror now merges the attacker's
> upstream into the local index. uv resolves the constraint to the
> attacker's `v0.9.0` and installs it. The org's private name has been
> shadowed without anyone noticing.

We need a per-package marker that says "this name belongs to the
tenant; never reach upstream for it."

## 2. Why per-package, not per-version

The earlier conversation considered `package_versions.created_via`
instead of `packages.created_via`. Per-package is meaningfully better
on both attack and operational axes:

| Concern | Per-package (`packages.created_via`) | Per-version (`package_versions.created_via`) |
| --- | --- | --- |
| **Typosquat attack (above)** | Package is `uploaded` → merge disabled → attacker invisible. ✅ | Both versions in merged index; uv resolves attacker's version because it satisfies the pin. ❌ |
| **Insider "shadow upload"**: pull-through has been serving `requests v2.32.x`; insider uploads `requests v999.99.99` to the tenant | Package is `pull_through` → upload is refused with a clear "delete to take ownership" error (see §5). ✅ | Local "newer" version shows in merged index; uv installs it. ❌ |
| **Round-trip cost** for `N` tenant-uploaded packages | Zero upstream contact on their `/simple/` requests. | Every `/simple/<acme-internal-thing>/` fans out to pypi.org, hoping for 404. Cached, but still upstream contact + potential leakage of private names via "similar names" responses. |
| **Mental model** | "We own this name." Matches how DNS / npm scopes / GitHub Packages all work. | "We own this version." No registry on Earth uses this granularity. |
| **Per-version `upstream_published_unix`** still needed? | Yes, for cooldown — independent column. | Yes, same column. |

The one workflow per-version *would* enable — "tenant uploads
`requests v999.x` and pkgmirror serves a merged view of `that + pypi
v2.32.x`" — isn't a workflow any private mirror or public registry
supports, and we couldn't construct a healthy use case for it. The
right answer for "I want my own version of an upstream package" is
"give it a distinct name" (Cargo, Maven, npm scopes, pip extras all
support this idiomatically).

## 3. First-touch semantics

The package's `created_via` is set on the row's initial INSERT,
based on which code path created it:

| Code path that creates the package row | `created_via` |
| --- | --- |
| `PUT /api/packages/<tenant>/<format>/upload` (manual upload) | `uploaded` |
| `POST /api/packages/<tenant>/<format>/` multipart twine upload (PyPI) | `uploaded` |
| `pullThroughDownload` / `pullThroughIngest` (any format) | `pull_through` |
| Future YAML bulk-import tooling | `uploaded` (operator intent is explicit) |

Once set, the value is sticky and never auto-mutates. The only ways
it changes:

1. **Admin deletes the package + reuploads / re-pulls** — new row, new
   first-touch wins.
2. **Admin edits `created_via` directly** via the console package
   detail page (see §5).

### Why both directions are sticky

Asymmetric stickiness was tempting ("first-uploaded stays uploaded;
first-pulled-through can be upgraded by an upload"), but the second
case is exactly the insider-shadow attack — silently allowing the
upgrade is dangerous, refusing it with a clear error is safe and
admin-recoverable.

### What "first declared intent wins" means in practice

- First action determines ownership.
- Second action of the opposite type fails loudly with an actionable
  error message.
- The legitimate exception — "we want to fork this upstream package
  privately" — requires deliberate admin action (delete + reupload).

Same model as DNS, npm package names, GitHub repo names. Whoever
registers first owns the name; the second person can't silently
shadow you.

## 4. Schema + storage

### 4.1. Migration v6

```sql
-- migration v6: packages.created_via
ALTER TABLE packages ADD COLUMN created_via TEXT NOT NULL DEFAULT 'uploaded';
```

Backfill semantics:

- Every existing row gets `'uploaded'`. This is the *safer* default
  for the migration: it disables upstream merge for pre-existing
  packages. If an operator wants to flip a row to `'pull_through'`
  (because they know it was originally pull-through-ingested), they
  can do so via the admin UI or a one-liner `UPDATE`.
- Choosing `'pull_through'` as the default would silently re-enable
  the typosquat hole for every pre-migration package, which is the
  opposite of what a security migration should do.

The migration is additive (column add only, no table rebuild); fast
on production-size DBs.

### 4.2. Constants in `internal/models`

```go
// CreatedVia identifies how a packages row first came into existence.
// Set on the initial INSERT and immutable thereafter unless an admin
// explicitly changes it via the console (or the package is deleted +
// re-created). Controls whether the PyPI handler's /simple/ merge
// reaches upstream for this (tenant, package).
type CreatedVia string

const (
    // CreatedViaUploaded means a human (or a CI publish step) put
    // this package into pkgmirror first. Upstream merge is disabled;
    // upstream is never reached for this (tenant, package).
    CreatedViaUploaded   CreatedVia = "uploaded"
    // CreatedViaPullThrough means pkgmirror first ingested this
    // package on a cold pull-through miss. Upstream merge is on.
    CreatedViaPullThrough CreatedVia = "pull_through"
)
```

### 4.3. Models changes

- `models.Package` struct: add `CreatedVia CreatedVia` field.
- `models.GetOrCreatePackage(...)` grows a `createdVia CreatedVia`
  parameter, passed at INSERT time. Existing callers in the service
  layer pass through their context.
- New `models.SetPackageCreatedVia(ctx, packageID, v CreatedVia) error`
  for the admin override path.
- `ListPackages` etc. return the field.

## 5. Behavior changes

### 5.1. PyPI handler

**`packageIndex` (the merge):**

```go
pkg, err := h.Models.GetPackageByLookup(...)
// ... existing handling ...

// NEW: only merge upstream into the local view when this package
// was originally pulled through. Tenant-uploaded packages are
// sealed - upstream is never reached for them.
if pkg.CreatedVia == models.CreatedViaPullThrough {
    upEntries, upVersions := h.mergeUpstreamIntoLocalIndex(...)
    // ... existing merge logic ...
}
```

**`upload` (twine + raw):**

```go
// In checkIngest, before persist:
if existing, err := h.Models.GetPackageByLookup(...); err == nil {
    if existing.CreatedVia == models.CreatedViaPullThrough {
        c.String(http.StatusConflict,
            "package %q is currently mirrored from upstream; " +
            "delete it via /console/tenants/%s/packages/pypi/%s " +
            "first if you want to take ownership",
            existing.Name, tenant.Name, existing.LowerName)
        return false
    }
}
```

This is the second-touch-fails-loudly rule. Without it, an insider
can shadow an upstream package by simply uploading a "newer" version.

**`pullThroughDownload`:**

No behavior change for the happy path. On the rare race where someone
uploads to the same name *during* a pull-through fetch, the upload's
own check (above) protects us — the upload races and either wins
(setting `uploaded`) or loses (the package now exists, but as
`pull_through`, and the upload's check refuses to overwrite).

### 5.2. Other formats

Apply the same two-rule pattern to npm, rubygems, etc. as their
pull-through PRs ship:

1. Upload sets `CreatedVia=uploaded`.
2. `/index/<name>/` or equivalent merge gated by `CreatedVia==pull_through`.
3. Upload-against-pull-through-owned returns 409 with the same shape
   of message.

Go modules need extra thought (the proxy protocol has no "upload"
endpoint in v1; today's PUT `/upload` is non-standard mirror tooling)
— for Go, every locally-uploaded module is by definition first-touch,
so the rule is trivially correct.

### 5.3. Admin console

[`/console/tenants/:name/packages/:type/:pkgname`](../internal/console/packages.go)
detail page:

- Show a "Provenance" pill near the title:
  - `uploaded` → blue badge "uploaded"
  - `pull_through` → grey badge "mirrored from upstream"
- Add a "Change provenance" form (system-admin only) — POST that
  toggles between the two values, with a confirm dialog explaining
  the security implication ("flipping to `pull_through` will allow
  pkgmirror to merge upstream versions into this package's index").
- Audit row `tenants.package.set_provenance` with old + new values.

### 5.4. Audit + observability

Every state-changing path emits an audit row:

| Event | Action | Notes |
| --- | --- | --- |
| Package created via upload | `tenants.package.upload` | Existing event; add `created_via=uploaded` to Extra. |
| Package created via pull-through | `tenants.package.pull_through` | Existing event; add `created_via=pull_through`. |
| Admin flips provenance | `tenants.package.set_provenance` | New event. Extra: `{"old":"...","new":"..."}`. |
| Upload refused because package is `pull_through` | `tenants.package.upload_refused_provenance` | New event, `decision=deny`. Useful for spotting insider attacks. |

## 6. Test plan

### Unit tests

- `TestPackageCreatedVia_DefaultsToUploadedOnUpload`: PyPI twine
  upload of new package → row's `CreatedVia=uploaded`.
- `TestPackageCreatedVia_PullThroughIngestSetsPullThrough`: cold
  pull-through fetch → row's `CreatedVia=pull_through`.
- `TestPackageCreatedVia_Migration`: open a v5 DB with one existing
  package row, run migration, confirm the row is `uploaded`.

### Integration tests (in-process httptest)

- `TestPackageIndex_UploadedPackageDoesNotMerge`: upload a package
  to a tenant; have upstream advertise an additional version; assert
  the upstream version is NOT in `/simple/`.
- `TestPackageIndex_UploadedPackageNeverContactsUpstream`: same setup
  but assert the upstream httptest server's index hit counter is `0`
  after the `/simple/` request.
- `TestUploadAgainstPullThroughOwnedReturns409`: cold-pull a package,
  then attempt a twine upload of a different version → 409 with the
  documented body text.
- `TestUploadAgainstUploadedOwnedSucceeds`: upload v1, then upload v2
  → 201 (no provenance check fails, since we're not changing the
  ownership type).
- `TestPackageIndex_MergeStillWorksWhenPullThrough`: regression test
  that the existing merge tests still pass when `CreatedVia=pull_through`.
- `TestAdminSetProvenance_FlipsBehavior`: cold-pull → `/simple/` shows
  merge; admin flips to `uploaded`; `/simple/` no longer merges.

### Manual / docs

- Update [docs/demos/python.md](../docs/demos/python.md) demo recipe
  to mention the provenance behavior in the talking-points section.
- Update [docs/supply-chain.md](../docs/supply-chain.md) with a new
  "Package provenance" section after "Cooldown control."

## 7. Migration concerns

### 7.1. Backfilling existing pull-through rows

A pkgmirror instance that has been pulling through packages will, on
migration, mark all of them `uploaded`. That's the safe default but
might surprise operators ("why did my mirror stop serving fresh
versions of `requests`?").

Options:

a. **Default to `uploaded`, document the migration loudly.** Operators
   re-flip via console as needed. Safest; one-time pain.
b. **Default to `pull_through` if no upload has ever happened against
   this package**, else `uploaded`. Requires schema introspection
   (joining against `audit_log` for upload events) — fragile and
   slow on large DBs.
c. **Default to `pull_through` always.** Re-opens the typosquat hole
   for any pre-existing tenant-uploaded package; unacceptable for a
   security migration.

→ **Go with (a).** Document it in the release notes. Add a one-liner
to `docs/supply-chain.md` showing the SQL to bulk-flip if an
operator knows their tenant is all pull-through.

### 7.2. Format-by-format rollout

The PyPI handler is the first beneficiary. Other formats keep the
column but their handlers ignore it until their pull-through PRs
land. No coupling forced; per-format adapters opt into the
provenance gate when they grow pull-through.

## 8. Out of scope

These are *not* part of this plan:

- Per-tenant provenance default ("acme-corp packages default to
  `uploaded`"). Tenant-wide policy is already the cooldown / blocklist
  story; provenance is per-package.
- Time-based provenance auto-flip ("after 90 days, pull-through
  packages become uploaded"). Adds complexity without solving a real
  problem.
- Per-version provenance. Explicitly rejected in §2.
- A separate "this package is paused / archived" flag. Use the
  existing delete-then-reupload workflow; or, if that proves
  insufficient, file a separate plan.

## 9. Open questions

1. **Should we surface provenance in the package's `/simple/`
   response somehow?** PEP 691 has no field for it; the rendered HTML
   could include an HTML comment for human debugging but no machine-
   readable consumer expects it. Probably no — the console UI is the
   right surface.
2. **What happens to the existing `mergeUpstreamIntoLocalIndex` call
   site after this lands?** It stays exactly as-is — only the
   condition under which it's called changes (now gated on
   `pkg.CreatedVia == CreatedViaPullThrough`).
3. **Should the 409 body include a JSON variant for API consumers?**
   Twine doesn't render the body; pip/uv users see a `pkg.upload`
   error from the client. Probably fine to keep it as plain text in
   v1; if operators complain, add JSON content-negotiation later.

## 10. PR sequence

| # | PR | Estimate |
| --- | --- | --- |
| **A** | Migration v6 + `models.CreatedVia` constants + `Package` struct field + `GetOrCreatePackage` signature change + migration test | 0.5 day |
| **B** | PyPI handler: set on upload, set on pull-through, gate merge on `pull_through`, refuse upload-against-`pull_through` with 409 + tests | 0.5 day |
| **C** | Console package detail page: provenance badge + admin "change provenance" form + audit events | 0.5 day |
| **D** | Docs: supply-chain.md section, demo talking points, README footnote | 0.25 day |

**~1.75 days total**, all PRs are independently shippable but PR A
must land first.

## 11. What this plan deliberately does NOT specify

These are decisions for the engineer implementing:

- The exact 409 body text (within the documented shape).
- The badge colors / icons in the console UI.
- Whether the "change provenance" confirm dialog uses HTML
  `confirm()` or a more elaborate modal.
- Whether `SetPackageCreatedVia` lives on `*Store` directly or in a
  dedicated admin service.

## 12. Risks

- **Operator confusion at migration time.** Mitigated by docs + a
  bulk-flip SQL snippet. The conservative default is correct;
  surprising-but-correct beats silently-insecure.
- **A format adapter forgets to set `CreatedVia` on a code path we
  don't think about.** Mitigated by the `NOT NULL DEFAULT 'uploaded'`
  schema constraint: forgetting to set it means the row defaults to
  the safer value.
- **Race between pull-through and upload of the same name.** SQLite
  serializes the writes; whichever transaction commits first wins
  the `CreatedVia` value. The loser's check in `upload` sees the
  winner's row and either succeeds (if the upload also wrote
  `uploaded`) or 409s (if pull-through won). Acceptable.

## See also

- [upstream-pull-through.md](upstream-pull-through.md) — the parent
  feature; this plan is a defense against one of the threats that
  feature introduces.
- [supply-chain.md](../docs/supply-chain.md) — operator runbook for
  cooldown / blocklist / quarantine; provenance is the fourth control.
- [internal/packages/pypi/handler.go `packageIndex`](../internal/packages/pypi/handler.go) —
  the merge that this plan gates.
- [internal/packages/pypi/upstream.go `mergeUpstreamIntoLocalIndex`](../internal/packages/pypi/upstream.go) —
  the call site that the new `CreatedVia` check guards.
