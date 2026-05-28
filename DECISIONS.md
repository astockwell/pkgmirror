# Decision Log

A running log of assumptions and architectural decisions made while building `pkgmirror`.
Each entry: date, decision, rationale, and (when relevant) what I'd revisit later.

---

## 2026-05-28 — RPM format (tenth format landed)

**Decision:** Implement Forgejo's RPM registry as the tenth package
format. Mounts at `/api/packages/:tenant/rpm/:group/` with a
free-form `:group` segment (e.g. `el9`, `fedora41`, `stable`) that
acts as an independent repository scope. Routes:
`/repository.key` + `/repository.repo` + `/repodata/:filename` +
`/package/:name/:version/:architecture/:filename` + `PUT /upload` +
`DELETE /package/:name/:version/:architecture`. Ported from
`forgejo/routers/api/packages/rpm/rpm.go`,
`forgejo/modules/packages/rpm/metadata.go`, and
`forgejo/services/packages/rpm/repository.go`.

**Library reuse — go-rpmutils.** The upstream parser is a thin
wrapper around `github.com/sassoftware/go-rpmutils`. We pull in the
same dep (Apache-2.0) rather than rolling our own RPM header parser
— the binary RPM header format is non-trivial and the upstream
library has been hardened against malformed input in production.
This is the same posture we took with `blakesmith/ar` + `ulikunitz/xz`
for Debian and `klauspost/compress/zstd` for OCI.

**On-demand index generation + Stable-Bytes-for-signed-on-demand-content.**
Same on-demand-not-cached choice as Debian, Maven, and Alpine. The
`<timestamp>` field in `repomd.xml` is derived from
`max(file.created_unix)` across the group rather than `time.Now()`,
so two separate GETs for `/repodata/repomd.xml` and
`/repodata/repomd.xml.asc` produce byte-identical repomd bytes that
the detached signature verifies against. This is the canonical
application of the "Stable bytes for signed-on-demand content"
playbook pattern first codified during the Debian work. Recorded in
[docs/known-deviations-from-spec.md](docs/known-deviations-from-spec.md).

**Per-tenant OpenPGP keypair on `_rpm`.** Matches the Debian/Alpine
key-storage pattern: a synthetic `_rpm` package row scoped to the
tenant holds the keypair (private in `SETTING.value`, public in
`SETTING.value`, both `KEY_VERSION` rows). First read of
`/repository.key` lazily generates it (gated by `ExclusivePool` keyed
on `<tenant>|rpm-key` to avoid the read-with-update fast-paths racing
against each other).

**Composite-key file naming.** Same Debian/Alpine pattern: a file's
`name` column encodes `<group>|<arch>|<basename>` so the existing
`UNIQUE(version_id, name)` constraint holds across multi-arch
publishes without a schema change. `loadEntriesForGroup` drains the
cursor before issuing the inner query that pulls each file's bytes,
keeping us safely off SQLITE_BUSY territory (same fix as Alpine).

**Single-segment group scope for the MVP.** Forgejo accepts arbitrary
multi-segment groups (`el9/extras/x86_64`-style). We accept only one
segment in this first cut: it covers the common dnf use case (one
group per OS release or channel) and keeps the route shape and the
composite-key encoder simple. Easy to relax later — bump
`storedFileName` to encode the full group and adjust the
`:group` route binding to a wildcard. No data migration required;
existing single-segment names are a strict prefix of the more
general form.

**Black-box stack: fedora:41.** Real `rpm --import` of the
`/repository.key` payload + a `/etc/yum.repos.d/pkgmirror.repo`
written with `gpgcheck=1` + `repo_gpgcheck=1` + `gpgkey=file://...`
+ `dnf makecache` + `dnf info gitea-test`. dnf 5 honors the chain
end-to-end. Two non-obvious client gotchas worth surfacing:

  1. dnf 5 ships in fedora:41; its credential plumbing is fussier
     than dnf 4's, so the simplest portable form is an authed
     `baseurl=http://x:$TOKEN@host/...` rather than an external
     credential helper. Documented in the README.
  2. fedora:41 ships its own default repos that try to fetch from
     `mirrors.fedoraproject.org` even when `--repo=pkgmirror-test`
     is passed; remove `/etc/yum.repos.d/*fedora*.repo` before the
     test exercises `dnf makecache`.

**Trade-offs / what I'd revisit:**

- Multi-segment groups (above) — would unblock the Forgejo
  `el9/extras/x86_64` layout for users migrating an existing tree.
- No SQLite-stored repodata cache. With ~thousands of packages per
  group the on-demand path is comfortable; past 50k it'd be worth
  measuring before committing to a cache.
- The `_rpm` synthetic key-holder is per-tenant; key rotation
  requires deleting the synthetic package and re-issuing the
  `.repo`/`rpm --import` pair. Fine for now.
- Source RPMs (.src.rpm) round-trip but their architecture is
  reported as `src`. We don't currently special-case the
  `repodata`'s `<arch>` field. Forgejo doesn't either.

---

## 2026-05-28 — Automated guard against the duplicate-`package` artifact

**Symptom.** Several times during the multi-month build, a Go file
in this repo arrived at the next test run with a duplicate
`package X` declaration and a spurious copyright-header fragment
inserted right after the legitimate one. Result: `go build` fails
with `expected declaration, found 'package'`. The most recent
instance bit `internal/syncutil/exclusive_pool_test.go` between the
ExclusivePool ship and the Debian ship, dropping the stress loop.

**What's triggering it.** Unknown. It's almost certainly a VS Code
Go-extension format-on-save or paste-handler bug in my local dev
environment — not file-specific, not consistently reproducible, not
something I can chase in pkgmirror itself.

**Decision.** Stop chasing the trigger. Guard the repository so a
bad commit can't land regardless of which editor or tool caused it.
Three layers:

1. `tools/dedup-package/` — a small Go tool that detects (and with
   `-fix` repairs) the duplicate. Uses `go/scanner` so it doesn't
   false-positive on `package X` strings inside test-file string
   literals (which an earlier regex-based prototype did). Refuses to
   auto-fix any span that contains real code rather than just
   comments + blanks — better to surface for human review than
   silently delete a function. Test coverage: clean / detected /
   fixed / refused-on-ambiguous.

2. `make check-package-dupes` runs the tool in check-only mode and
   is wired as a `vet:` prerequisite; `make fix-package-dupes`
   repairs in place. CI workflow runs the same check before
   `go vet`, so a bad state can't even land in main via PR.

3. `.githooks/pre-commit` runs the tool on every local commit.
   `make install-hooks` symlinks it. The hook also documents
   `--no-verify` as the escape hatch (for when investigating the
   trigger itself).

Net effect: regardless of which contributor's editor causes the
artifact, the bad bytes don't make it into the working tree.

**Verified end-to-end:** I deliberately poisoned a file matching the
observed pattern, confirmed `git commit` was blocked by the hook
with a clear error message, confirmed `make fix-package-dupes`
repaired it byte-correctly, and confirmed the rebuilt file compiles
+ `go vet`s clean.

**What I'd revisit later:** investigating the actual source of the
duplication. The most likely culprit is a VS Code Go-extension
hook firing on paste or on save, or possibly an aggressive
`goimports` config — but figuring that out is editor-specific and
the guard above is the right defense even if I do eventually pin
down the trigger.

---

## 2026-05-28 — Debian format (ninth format landed)

**Decision:** Implement Forgejo's Debian (apt) registry as the ninth
package format. Mounts at `/api/packages/:tenant/debian` with a
mix of named-param routes (the `/dists/...` index family) and
catch-all-style coordinates (the `/pool/.../upload` write path).
Ported from `forgejo/routers/api/packages/debian/debian.go`,
`forgejo/modules/packages/debian/metadata.go`, and
`forgejo/services/packages/debian/repository.go`.

**On-demand index generation + load-bearing `Date:` derivation.** Same
on-demand-not-cached choice as Maven and Alpine. But Debian is the
first format where index generation involves *both* signing AND a
client that fetches the signed-thing and the signature in
*separate* requests. The detached `Release.gpg` is computed over
the exact bytes of `Release`. If two GET requests for `/Release`
and `/Release.gpg` produce Release bytes with different `Date:`
fields (because `time.Now()` ticks between them), the signature
no longer validates against the second Release body — apt fails
with "GPG error".

The fix is to make `Release` deterministic from the data state.
We derive `Date:` from `max(file.created_unix)` across the
distribution. The byte sequence is now identical across requests
without any caching coordination. This is a new cross-cutting
pattern future signed-on-demand formats (RPM's `repomd.xml.asc`,
NuGet symbol packages with signature, etc.) will need too —
captured in docs/adding-a-format.md as the "Stable bytes for
signed-on-demand content" pattern.

**Per-tenant OpenPGP keypair on `_debian`.** Matches the Alpine
RSA-key pattern: a synthetic `_debian` package row scoped to the
tenant stores the armored private + public PEM as properties.
First-time generation is gated behind `internal/syncutil.ExclusivePool`
keyed on `<tenant>|debian-key` to avoid the
UNIQUE-property-constraint races Forgejo had to add `RetryTx` for.
We don't need the retry because the lock prevents the race entirely.

**Composite-key encoding follows Alpine.** Each .deb file row's
name is `<dist>|<comp>|<arch>|<basename>.deb`. The Packages index
emission strips the composite prefix and emits the basename in the
`Filename:` field that apt actually requests. Avoids the
`composite_key` schema column Forgejo adds.

**Out of scope:** by-hash lookups (`/dists/.../by-hash/<algo>/<hash>`)
are advertised in `Release: Acquire-By-Hash: yes` but the routes
aren't implemented; apt falls back to direct path lookups when
those 404. Source packages (`.dsc` + `.diff.gz` + `.tar.gz`) and
the `Sources` index are also unimplemented — most operators only
need binary `.deb` distribution. Both are purely additive to the
current shape.

**Validated:** `apt update` succeeds against our InRelease + Release.gpg
signature pair (the load-bearing assertion); `apt-cache show
fixture-pkg` parses the on-demand Packages index and surfaces our
metadata. We deliberately don't drive `apt install` because that
requires a fully-extractable `data.tar` inside the `.deb` (rootfs
payload, scripts, dependency closure) — well beyond what we need
to prove the registry format.

**Notable scope decisions worth revisiting:**
- The `_debian` synthetic key-holder is per-tenant; key rotation
  is a manual SQL exercise. Same posture as Alpine. A `/key/rotate`
  endpoint that re-publishes the public key under a new name is
  worth designing once we have a real operational need.
- Origin in the Release file is the constant string `"pkgmirror"`.
  Forgejo uses the application name; we use a fixed value so
  renaming a tenant doesn't break apt's repo fingerprint cache.

---

## 2026-05-28 — Adopt Forgejo's ExclusivePool + add built-in TLS

Two follow-ups from the Maven implementation review.

**ExclusivePool replaces the naïve sync.Map of mutexes.** The original
Maven handler used a `sync.Map[string]*sync.Mutex` to serialize
per-package upload races. That works but leaks the mutex map entry
forever — fine for a few hundred packages, ~80 bytes / coordinate of
slow growth for a busy registry. Forgejo's `modules/sync.ExclusivePool`
(originally from Gogs) refcounts per-key holders and deletes the map
entry when the count hits zero, bounding memory by *concurrent*
uploads rather than *unique coordinates ever seen*. Ported into
`internal/syncutil` as a shared primitive — Maven uses it today,
future Debian / NuGet / RPM formats with multi-file uploads will too.
The implementation is ~40 lines + 4 tests covering the four invariants:
serialization, parallel non-conflicting keys, post-checkout map
cleanup, and the nested-count edge case.

**Built-in TLS via `PKGMIRROR_TLS_CERT` + `PKGMIRROR_TLS_KEY`.** The
canonical recommended deployment is "pkgmirror behind nginx/Caddy/
Traefik" — those proxies handle ACME, cert renewal, OCSP stapling
better than we ever would. But for single-binary deployments and
dev environments where a reverse proxy is overkill, paying the
proxy tax isn't reasonable. Added the two env vars; when both
present, `cmd/pkgmirror/main.go` calls `ListenAndServeTLS` instead
of `ListenAndServe`. Setting only one is a misconfiguration the
process catches at boot. Hot cert reload is deferred — operators
restart pkgmirror after rotating certs. ACME is out of scope (state
management, challenge handlers, account keys; not a thing we should
own).

Specific consequence: the Maven blackbox test no longer needs the
`maven-default-http-blocker` workaround if the test stack is ever
upgraded to HTTPS. We're leaving the workaround in place for now
because the blackbox uses internal-container HTTP and adding TLS
between sibling containers is a separate plumbing exercise the
harness doesn't need yet. The README documents the workaround as
"only for plain-HTTP deployments; the principled fix is to terminate
TLS."

---

## 2026-05-28 — Maven format (eighth format landed)

**Decision:** Implement Forgejo's Maven registry as the eighth
package format. Mounts at `/api/packages/:tenant/maven` with a single
catch-all `*path` route per method. Ported from
`forgejo/routers/api/packages/maven/maven.go` (handlers) +
`api.go` (maven-metadata.xml shape) + `modules/packages/maven/metadata.go`
(POM parser).

**Catch-all path parsing:** Maven's URL shape is
`<groupId-with-slashes>/<artifactId>/<version>/<filename>` with a
special case for `<groupId>/<artifactId>/maven-metadata.xml` (no
version segment). Forgejo solves this with `extractPathParameters`
that consumes from the tail; we ported the algorithm verbatim,
adjusting only for gin's leading-`/` catch-all convention. The
illegal-characters regex (`[\\/:"<>|?\*]`) is preserved from
upstream.

**Generated maven-metadata.xml, on demand:** Forgejo and pkgmirror
both compute the per-(group,artifact) metadata XML from the live
version list at request time. We don't cache it. Sub-millisecond
even with hundreds of versions; "always fresh" beats any
cache-invalidation coordination. The element order
(`versioning>release` before `versioning>latest` before `versioning>versions`)
matters: older Maven 3.x parsers have historically warned or rejected
out-of-order metadata. We marshal via the same struct shape as
forgejo to lock the order.

**Checksum sidecar handling:** `.md5/.sha1/.sha256/.sha512` files
adjacent to artifacts are NOT stored. On GET we synthesize from the
blob's stored hash; on PUT we verify the supplied hex matches and
return 200. Mismatch is 400 so upload corruption surfaces
immediately. The same applies to `maven-metadata.xml.<hash>` —
synthesized from the generated XML.

**Per-package upload locking:** `mvn deploy` PUTs jar + pom +
sources.jar + javadoc.jar + maven-metadata.xml + each one's checksum
in rapid succession against the same `groupId:artifactId:version`.
Without a lock the race between `CreateVersion` calls produces
spurious `ErrDuplicatePackageVersion` errors. Forgejo serializes via
its `sync.ExclusivePool`; we use a `sync.Map` of `*sync.Mutex` keyed
on `<tenantID>|<groupId>:<artifactId>`. Slightly higher memory
footprint (mutexes never freed) but simpler than maintaining a pool.
Acceptable for our scale.

**POM-after-jar metadata backfill:** Maven's upload order isn't
deterministic — jar can arrive before pom. The lead `pom` carries
the canonical metadata (groupId/artifactId/version, licenses,
dependencies). On pom upload we always call
`UpdateVersionMetadata` to overwrite whatever empty metadata the
sibling jar's version-row creation left behind. Added
`models.UpdateVersionMetadata` for this; was not previously needed
by any other format.

**maven-default-http-blocker workaround in the blackbox:** Maven
3.8.1+ ships with a built-in mirror that routes every external HTTP
repository through `http://0.0.0.0/` to enforce HTTPS-by-default.
The blackbox test container talks to pkgmirror over the docker
network in plain HTTP, so the blocker hits before our endpoints
ever see the request. The settings.xml in
`tests/blackbox/maven/conformance_test.go` shadows the default
blocker with a same-id mirror whose `mirrorOf` matches nothing.
Production deployments should terminate TLS in front of pkgmirror
and avoid the override entirely.

**Validated:** real `mvn deploy:deploy-file` followed by `mvn
dependency:get` from a clean local repository round-trips through
the registry — pom + jar + sidecar checksums + generated
maven-metadata.xml all parse cleanly. 16 grey-box tests cover path
parsing edge cases, checksum verification, SNAPSHOT vs release in
the metadata's `release` element, and the
`ignore-client-pushed-maven-metadata` quirk.

---

## 2026-05-28 — Alpine (apk) format (seventh format landed)

**Decision:** Implement Forgejo's Alpine registry as the seventh
package format. Mounts at `/api/packages/:tenant/alpine`. Five
endpoints (one public-key, one upload, one download, one delete, plus
on-demand APKINDEX.tar.gz served via the download route's
filename-dispatcher), modeled on
`forgejo/routers/api/packages/alpine/alpine.go` (MIT) and
`forgejo/services/packages/alpine/repository.go`.

**Composite-key avoidance:** apk's repo layout addresses each `.apk`
by `(branch, repository, architecture, filename)`. Forgejo adds a
`composite_key` column to `package_files` to disambiguate; we don't
have that, so we encode the tuple directly into the file row's
`name`: `<branch>|<repository>|<architecture>|<basename>.apk`. This
keeps the existing `UNIQUE(version_id, name)` invariant intact for
multi-arch publishes of the same version. The on-wire `basename` is
recovered by splitting on `|` in the handler. Trade-off: SQL
queries that need to filter by coordinate end up doing `LIKE
'<branch>|<repository>|<arch>|%'`, which is fine for our scale but
would warrant a real column at high cardinality.

**APKINDEX built on demand:** Forgejo caches the signed
`APKINDEX.tar.gz` as a file on a synthetic `_alpine`/`_repository`
package and rebuilds it on every upload/delete. We skip the cache
and rebuild from the live file metadata on each GET. The rebuild is
sub-100ms even with a few hundred packages and the simpler "always
fresh" semantics dodges a whole class of cache-invalidation bugs.
Worth revisiting if we hit very large repos (alpine main has ~10k
packages) — building the index becomes O(n) per request and the
index file itself reaches several MB. The cache strategy is a
straightforward upgrade when we need it.

**Per-tenant RSA signing keys, stored as properties:** apk refuses
to accept an unsigned APKINDEX (`UNTRUSTED signature`), so signing
is non-negotiable. We generate a 4096-bit RSA keypair on first
request and persist it as properties on a synthetic `_alpine`
package row scoped to the tenant. This piggybacks on the existing
`package_properties` table rather than introducing a
`tenant_settings` table. The synthetic-package trick is what
forgejo does upstream (it calls it the "internal" package), so an
operator inspecting the DB sees a recognisable layout. Public-key
distribution is exposed at `GET /api/packages/:tenant/alpine/key`
with the right `<owner>@<fingerprint>.rsa.pub` filename — clients
drop the file in `/etc/apk/keys/`. Key rotation is currently a
manual SQL exercise; a `/key/rotate` endpoint with a grace period
is on the roadmap.

**`noarch` fan-out:** When PKGINFO advertises `arch = noarch` we
publish the same `.apk` file row under every architecture the repo
already has (falling back to `x86_64` if the repo is empty). Same
behavior as forgejo and a stock alpine mirror. The fan-out happens
under the same `CreatePackageOrAddFileToExisting` ingest path so
all the policy hooks fire per-arch.

**Cross-platform CI:** the blackbox test had to handle that
`apk update` always fetches `<repo>/<arch>/APKINDEX.tar.gz` where
`arch` is the *container's* runtime arch — `aarch64` on Apple
Silicon dev laptops, `x86_64` on linux/amd64 CI. The test now
publishes the fixture for both arches up front to stay
arch-agnostic without runtime branching.

**Validated wire-compat:** the blackbox test launches a real
`alpine:3.20` container, installs the tenant's public key, points
`/etc/apk/repositories` at us, and runs `apk update` followed by
`apk search`. The signature-verification step inside `apk update`
is the load-bearing assertion: any byte-level mismatch in our
APKINDEX.tar.gz construction (gzip stream count, tar trailer,
PKCS#1-v1.5 SHA1 detached signature, fingerprint header) would
manifest as `BAD signature`. The test passes against our build,
which means our APKINDEX is byte-equal to what real apk-tools
publishes.

**Skipped scope (deliberately):** we don't drive `apk add` in the
blackbox test because that requires building a fully-installable
`.apk` (rootfs tar + control hash dance + per-file signing). The
index format and signature is what matters for "this looks like a
real Alpine mirror"; `apk add` of an arbitrary upstream package is
an integration test for `apk` itself, not for our index.

**Two flake-shaped bugs caught by the 10× stress loop after initial
commit:**

1. *Missing tar body padding in the signature stream.* The original
   `writeGzipStream` ported verbatim from forgejo wrote
   `tar.WriteHeader` + `tar.Write` but never called `Flush` or
   `Close` when `addTarEnd=false`. Go's `tar.Writer` only pads the
   body to a 512-byte block boundary on the next `WriteHeader` or
   `Close`, so the signature stream was technically malformed (body
   ended at byte 512+N instead of a 512-byte boundary). Real `apk`
   tolerates it because it reads the signature by explicit byte
   count and never seeks past it, but any reader walking the
   concatenated tars fails. This was completely masked when the
   signature was 512 bytes (4096-bit RSA → no padding needed); it
   only surfaced when we ran tests with 2048-bit keys for speed.
   Fix: always `Flush()` when not closing.

2. *Nested DB query inside an open cursor → SQLITE_BUSY under
   load.* `loadIndexEntries` iterated `rows.Next()` on a join cursor
   and called `Models.GetProperty()` per row to fetch the
   `alpine.metadata` property. With the modernc.org/sqlite driver a
   second query while a cursor is active can race writers from a
   sibling test and stall on the per-file lock until the 5-second
   busy timeout. Fix: drain the cursor into a slice first, close
   it, then issue per-row property fetches against a free
   connection. Same pattern any future format with nested-lookup
   indices should follow.

Both bugs only manifested when the suite was run under `go test
./...` parallel load on a saturated CPU — exactly the condition CI
runs in. A 10×-rerun loop on the full suite is now green.

---

## 2026-05-28 — Generic format (sixth format landed)

**Decision:** Implement Forgejo's "generic" registry as the sixth
package format. Mounts at `/api/packages/:tenant/generic`. Four
endpoints, modeled on `forgejo/routers/api/packages/generic/generic.go`
(MIT):

- `PUT    /:name/:version/:filename` — upload (auth, 201 / 409 on dup)
- `GET    /:name/:version/:filename` — download
- `DELETE /:name/:version/:filename` — delete one file; if it was the
  last file in the version, the version row is also removed (matches
  Forgejo's `DeletePackageFile`)
- `DELETE /:name/:version` — delete the whole version + every file

There is no parser. The request body IS the blob; ingestion goes
straight through `Service.CreatePackageOrAddFileToExisting` so a
generic "package" carries many files per version (the
`linux-amd64.tar.gz` + `linux-arm64.tar.gz` + `darwin-arm64.tar.gz`
release-bundle shape, or Forgejo's documented use case of arbitrary
release artifacts under a single semver).

**Name and filename validation** ported verbatim from upstream:

- `packageNameRegex = \A[-_+.\w]+\z`, single-char names must be
  alphanumeric, the literal `".."` is rejected even though it would
  match the regex (defense against path-traversal even though our
  storage keys are sha256 hex).
- `filenameRegex = \A[-_+=:;.()\[\]{}~!@#$%^& \w]+\z`, plus rejection
  of `"."`, `".."`, and any value where leading/trailing whitespace
  differs from the trimmed version.
- Version must equal its trimmed form (no leading/trailing
  whitespace).

**Model additions:** the generic format is the first to need real
row-level deletes, since uploads can be undone by the caller. Three
new helpers on `models.Store`:

- `GetFileByVersionAndName(versionID, name)` — single-row lookup by
  `(version_id, lower_name)`.
- `DeleteFile(fileID)` — removes one `package_files` row. Does not
  touch the underlying blob (content-addressed; orphan GC is a
  separate pass).
- `DeleteVersion(versionID)` — removes the `package_versions` row;
  the `ON DELETE CASCADE` on `package_files.version_id` removes all
  attached files in one step.

These are general-purpose helpers (not generic-specific). RubyGems
yank is still `QuarantineVersion`-based — it's "soft delete with
audit trail," which is the right semantic for `gem yank`. Generic's
DELETE is hard delete because the caller's intent is literally
"remove this," and there's no upstream registry equivalent to
"hide from listings."

**Black-box client:** `curlimages/curl:8.10.1`. The conformance suite
drives a real `curl` from inside the network through:

- `curl -X PUT --data-binary @file URL` — upload.
- `curl -o downloaded URL` — download.
- `cmp` to verify byte equality.
- Additional from-the-test-process direct HTTP for multi-file +
  delete-version + auth-rejected cases (curl works there too but the
  in-process path is faster).

One gotcha worth recording: `curlimages/curl` runs as a non-root user,
so `/work` (root-owned by testcontainers' file injection) isn't
writable for the `-o downloaded.bin` flag. The harness puts both the
payload and the output in `/tmp` instead. Logged because every future
test that uses this image will hit it.

**What I'd revisit:**
- Forgejo also exposes a JSON metadata view of the package at
  `/api/v1/packages/...`. We don't have a packages-API surface yet so
  this is out of scope.
- The blob orphan GC is still a TODO. Generic is the first format
  where row deletion makes orphan accumulation observable in normal
  use; the new `storage.IterateObjects` + the model's
  `hash_sha256 UNIQUE` lookup makes this straightforward whenever we
  decide to implement it.

---

## 2026-05-28 — Storage interface re-shaped to match Forgejo's ObjectStorage

**Decision:** Replace the local 3-method `storage.Backend` interface
(`Put` / `Open` / `Delete`) with a faithful port of
[`forgejo/modules/storage.ObjectStorage`](forgejo/modules/storage/storage.go).
Six methods (`Open`, `Save`, `Stat`, `Delete`, `URL`, `IterateObjects`)
plus a richer `Object` return type that satisfies `io.ReadCloser +
io.Seeker + Stat() (os.FileInfo, error)`. The single implementation,
`LocalStorage`, ports the matching `forgejo/modules/storage.LocalStorage`.

**Why this shape and not my own:** Forgejo's `ObjectStorage` has been
through years of production use across LFS, package storage, avatars,
attachments, and now multiple cloud backends (MinIO is the canonical
remote impl). The original three-method interface I wrote was
filesystem-shaped — it lacked size-passing on `Save`, a stand-alone
`Stat`, a pre-signed-URL hook, and an iterator for GC. All four gaps
were going to be hit the moment we added a second backend; better to
adopt the battle-tested shape now while there's one caller-cluster to
migrate.

**Mapping:**

| New (mirrors Forgejo) | Old |
| --- | --- |
| `ObjectStorage.Open(path) (Object, error)` | `Backend.Open(key) (io.ReadSeekCloser, error)` |
| `ObjectStorage.Save(path, r, size) (int64, error)` | `Backend.Put(key, r) error` |
| `ObjectStorage.Stat(path) (os.FileInfo, error)` | — *(callers fell back to the DB row's `size`)* |
| `ObjectStorage.Delete(path) error` | `Backend.Delete(key) error` |
| `ObjectStorage.URL(path, name, params) (*url.URL, error)` | — *(no pre-signed redirect hook)* |
| `ObjectStorage.IterateObjects(prefix, fn) error` | — *(no iteration; GC was impossible)* |
| `Object` (Read + Close + Seek + Stat) | `io.ReadSeekCloser` |

**Implementation:** `LocalStorage` keeps the two-level on-disk sharding
(`<root>/<aa>/<bb>/<full-path>`) as a private optimization — Forgejo's
`LocalStorage` doesn't shard because their caller-side IDs are
typically small ints, but we're keyed by sha256 hex digests at every
path, so sharding cheaply caps directory fan-out. `IterateObjects` is
careful to strip the on-disk shard prefix before invoking the callback
so callers see the logical key they originally passed to `Save`.

**Compatibility:** None — `Backend` is gone. Internal-only interface
with five direct callers (`internal/packages/service.go` × 2,
`internal/packages/container/handler.go` × 4); all migrated in this
commit. Test fixtures updated by a single `sed` (`NewFS` →
`NewLocalStorage(ctx, root)`).

**Small wins enabled by the new shape (already cashed in):**

- The OCI upload path now passes the staged-file size all the way
  through to `Save` instead of doing a wasteful discard-copy after the
  fact. Saved one full file-rewind per blob upload.
- `OpenFile` returns `storage.Object`, so OCI HEAD handlers could read
  size from `obj.Stat()` instead of a second DB lookup — not yet
  rewired, but trivially available.

**What's still ahead of us, made possible by the new shape:**

- An S3 / MinIO backend is now a straight transliteration of
  `forgejo/modules/storage/minio.go`. Same 6-method interface.
- `URL()` enables 307 pre-signed-redirect responses for big-blob
  pulls (especially OCI layers). `LocalStorage.URL` returns
  `ErrURLNotSupported`; handlers fall back to streaming. Adding a
  redirect path to e.g. the OCI blob handler is one `if err == nil
  { c.Redirect(307, url.String()); return }`.
- `IterateObjects` enables a real orphan-blob GC ("walk storage,
  compare against `package_blobs` rows, delete anything not
  referenced").

**Tests:** 12 new unit tests for `LocalStorage` covering Save/Open/Stat
round-trip, ErrNotFound on missing keys (Open + Stat), idempotent
delete, atomic overwrite, unknown-size streaming (`size=-1`),
`ErrURLNotSupported` from `URL()`, `IterateObjects` happy path +
callback-error propagation + context cancellation, no-temp-file-
leakage invariant, and Object seekability. All 50+ existing tests
across the 5 package formats + admin + audit + 2 policy evaluators
pass unchanged; all 5 docker-backed blackbox conformance suites
(`go`, `pypi`, `npm`, `rubygems`, `container`) pass unchanged.

**What I'd revisit:** the `ctx` we hand to `NewLocalStorage` is only
consulted inside `IterateObjects`. Forgejo uses it more broadly for
shutdown propagation; ours could too once we have long-running
operations (GC, scheduled re-scans) plumbed in.

---

## 2026-05-28 — Container/OCI: multi-segment image names

**Decision:** Replace the single-segment `:image` routes with per-method
catch-all routes (`/v2/*action`) plus a regex-based dispatcher. This is
the same trick Forgejo uses (`forgejo/routers/api/packages/api.go`:
`blobsUploadsPattern`, `blobsPattern`, `manifestsPattern`) and the only
straightforward way to make image references like
`localhost:8080/default/myorg/team/svc:v1` work.

**Why a refactor was unavoidable:** gin's underlying router
(httprouter) does not allow mixing a static path parameter (`:image`)
with a wildcard (`*action`) at the same path position. We tried; it
panics on Register. The choice is therefore binary: keep the
single-segment routes and lose multi-segment names, or drop the
single-segment routes and dispatch everything inside one catch-all per
method. Forgejo took the same route under chi.

**Dispatcher design:**
- One `gin.HandlerFunc` per HTTP method, all registered against
  `/v2/*action`. The shared `dispatch(method)` closure inspects
  `c.Param("action")` (which gin populates with everything after
  `/v2`, including the leading `/`).
- Static cases first: empty tail → `/v2/` version probe; tail
  `"token"` → token-exchange endpoint.
- Pattern matches by method: `tagsListRE`, `manifestRE`, `blobRE`,
  `blobUploadsRootRE`, `blobUploadUUIDRE`. Each pattern is anchored at
  both ends and uses `(.+)` for the image name so it greedily absorbs
  interior slashes; the terminating literal (`/manifests/`,
  `/blobs/`, `/tags/list`) is what stops it.
- A small `setParams(c, k, v, ...)` helper appends to `c.Params` so
  the existing handlers' `c.Param("tenant")` / `c.Param("image")` /
  `c.Param("reference")` / etc. continue to work unchanged. No
  handler signatures needed touching.

**Greedy matching is correct:** for a request like
`/v2/default/myorg/team/svc/manifests/v1`, `manifestRE`'s
`^([^/]+)/(.+)/manifests/([^/]+)$` greedily consumes
`myorg/team/svc` as the image so the final `/manifests/v1` matches.
For `/v2/default/img/manifests/sha256:abc...`, the `[^/]+` reference
group correctly accepts digests (no slashes in `sha256:<hex>`).

**Edge case: image names that contain literal `"manifests"` or
`"blobs"` segments** (rare but valid) parse to the leftmost split.
E.g., `default/foo/blobs/manifests/v1` yields image=`foo/blobs`,
reference=`v1`. This matches Forgejo's behavior.

**Tests:** Four new grey-box tests against the http handler
(`TestOCI_MultiSegment_*`) and one new black-box test
(`TestOCIConformance_MultiSegmentImageName`) that drives
go-containerregistry against a three-segment image name and verifies
push + pull + tags/list all round-trip. Existing single-segment tests
continue to pass unchanged.

**What I'd revisit:** the regex constants are package-level and
compiled at init. Fine for now; if we ever add more endpoints we should
consolidate to a routing table-of-(method, pattern, handler) so the
dispatch function stays linear.

---

## 2026-05-27 — Container / OCI format (fifth format landed)

**Decision:** Implement OCI distribution v1.1 as the fifth format. This
is the playbook's "very high" complexity entry and the only format on
the roadmap that can't be mounted under `/api/packages/:tenant/...` —
OCI clients hard-code `/v2/` at the host root. Routes live at
`/v2/:tenant/:image/...`.

Routes:

- `GET    /v2/`                                    — version probe (200 if authed; 401 with `WWW-Authenticate: Bearer realm=...` if not)
- `GET    /v2/token`                               — Basic → Bearer exchange (round-trips the password as the token)
- `GET    /v2/:tenant/:image/tags/list`            — `crane ls` / `docker image ls`
- `HEAD   /v2/:tenant/:image/manifests/:reference` — manifest exists?
- `GET    /v2/:tenant/:image/manifests/:reference` — fetch manifest (tag or digest)
- `PUT    /v2/:tenant/:image/manifests/:reference` — upload manifest
- `DELETE /v2/:tenant/:image/manifests/:reference` — delete manifest (digest cleans property; tag is soft-delete)
- `HEAD   /v2/:tenant/:image/blobs/:digest`        — blob exists?
- `GET    /v2/:tenant/:image/blobs/:digest`        — fetch blob
- `POST   /v2/:tenant/:image/blobs/uploads/`       — start upload (supports monolithic shortcut via `?digest=...`)
- `PATCH  /v2/:tenant/:image/blobs/uploads/:uuid`  — chunked write
- `PUT    /v2/:tenant/:image/blobs/uploads/:uuid`  — finalize with `?digest=...`
- `DELETE /v2/:tenant/:image/blobs/uploads/:uuid`  — cancel

**Token-exchange auth:** Modeled on the spec, not on Forgejo's JWT
issuance. Our `/v2/token` endpoint:

1. Accepts HTTP Basic.
2. Extracts the password half (our existing tokens are inherently
   bearer-compatible — they're random 32-char base32 strings).
3. Returns it back as the bearer token in a JSON envelope.

So `pkm_<32 chars>` flows through unchanged: Basic password → bearer
token → next request's `Authorization: Bearer <same value>` →
extractToken recognizes it as a pkm-prefixed token. The /v2/token
endpoint is effectively a no-op pass-through. Forgejo issues short-lived
scoped JWTs; we chose simplicity instead and document the trade-off here.

**Anonymous bearer:** `go-containerregistry` (the library `crane` is
built from) rejects an empty `token` field in the bearer response.
For anonymous callers we return the literal string `"anonymous"` as the
bearer, which our auth middleware fails to look up and treats as
no-identity. This lets public-tenant reads work even when the client
mechanically follows the bearer dance.

**Storage model:** A repo is a Package; a tag is a Version (with the
manifest digest + media type stored in MetadataJSON). Manifest bytes are
just another content-addressed Blob. Looking up a blob by digest uses a
`container.blob.<digest>` package property mapping to `blob_id` — no
new schema. Looking up a manifest by digest uses
`container.manifest.<digest>` plus a sibling
`container.mediatype.<digest>` so HEAD can return Content-Type without
re-parsing the manifest JSON.

**In-memory upload tracker:** OCI's blob upload protocol is stateful
(POST opens a session, PATCH appends chunks, PUT finalizes). We keep
each in-flight session in a `sync.Mutex`-guarded map keyed by UUID,
backed by a temp file. State is in-process only — a pkgmirror restart
cancels in-flight uploads. Fine for typical `crane push` durations
(seconds); persisting the tracker is a clear follow-up if we ever care
about multi-hour pushes.

**Monolithic POST shortcut:** The spec allows
`POST /blobs/uploads/?digest=...` with the bytes in the body as a
one-shot upload, skipping the PATCH/PUT round trip. We support it
because some clients (notably `oras`) use it by default.

**MVP scope limits (documented in handler.go and README):**
- ~~Image names are a single path segment.~~ Multi-segment image names
  (e.g. `myorg/myimage`, `library/alpine`) now supported via per-method
  catch-all routes + regex dispatch — mirrors Forgejo's approach in
  `forgejo/routers/api/packages/api.go`. See 2026-05-28 entry below.
- No cross-repo blob mount via `?mount=<digest>&from=<other-repo>`.
  Clients that try this fall through to a normal upload session, which
  is spec-allowed.
- No `/v2/_catalog`. Cheap to add but not needed by any normal
  push/pull flow.

**Black-box conformance via library, not CLI:** We use
`go-containerregistry`'s `remote.Write` / `remote.Image` directly from
the test process rather than running `crane` in a docker container. The
library IS what crane is built on — the wire format is identical — and
it removes the need for the client container to have internet access
to pull a base image. The four tests cover: push+pull round-trip with
digest verification, tags listing across 3 pushed tags, anonymous pull
on a public tenant, and empty `scratch` image push (no layers, just a
config blob).

**What I'd revisit:**
- Multi-segment image names. The wildcard route is mechanical but
  changes the dispatch shape enough that I didn't want to do it
  speculatively.
- The bearer-token endpoint should mint actual short-lived,
  scope-limited tokens instead of round-tripping. Pre-existing
  `Authorization: Bearer pkm_...` headers would still work; the change
  is additive.
- A real OCI image config parser would let the policy engine see the
  labels (org.opencontainers.image.licenses, .source, .description)
  the way Forgejo does. That's a clean evolution and the right place
  for the supply-chain controls to hook in.

Dependency added: `github.com/google/go-containerregistry` (test-only,
pulled in by the blackbox suite under `//go:build blackbox`).

---

## 2026-05-27 — RubyGems format (fourth format landed)

**Decision:** Implement RubyGems as the fourth package format, mounted at
`/api/packages/:tenant/rubygems`. Both the modern compact index and the
legacy Marshal-encoded specs are served so we work with every gem CLI
from 2.x through current.

Routes (verbatim port of Forgejo's shape):

- `GET    /specs.4.8.gz` — Ruby Marshal-encoded specs index (every version)
- `GET    /latest_specs.4.8.gz` — newest version per package
- `GET    /prerelease_specs.4.8.gz` — empty (we don't model prerelease)
- `GET    /info/:package` — compact index info file
- `GET    /versions` — compact index versions file (with per-package md5)
- `GET    /quick/Marshal.4.8/:filename` — zlib-Marshal `Gem::Specification`
- `GET    /gems/:filename` — `.gem` tarball download
- `POST   /api/v1/gems` — upload (auth)
- `DELETE /api/v1/gems/yank` — yank a version (auth)

Parser + Marshal encoder ported from
`forgejo/modules/packages/rubygems/{metadata,marshal}.go` (MIT). SPDX +
attribution headers preserved on the derived files; differences are
limited to error-sentinel shape (we use plain `errors.New` since our
HTTP layer doesn't depend on Forgejo's `util.NewInvalidArgumentErrorf`).

**Ruby Marshal v4.8 encoder:** Implemented just enough types
(`nil`, bool, Fixnum, String, Symbol with link table, Array,
UserMarshal, UserDef, Object) to satisfy the registry endpoints. No
decoder — every endpoint we serve writes Marshal data, never reads it.
The encoder is upstream-byte-exact and exercised by the unit test
`TestMarshalEncoder` which uses the golden vectors from Forgejo's own
test suite. If the encoder is ever out of spec with Ruby Marshal those
tests fail immediately.

**Yank as policy quarantine:** `gem yank` semantics are "make this
version uninstallable but leave the archived `.gem` for forensic
reachability". That maps cleanly onto our existing
`models.QuarantineVersion` machinery with a stable `reason = "yanked"`,
which means yanks flow into the audit log and the `/admin/quarantine`
view for free — no new table, no parallel "is_yanked" column. To
un-yank, an operator promotes the version through the same admin
endpoint they'd use for any other quarantine.

**Filename lookup is O(N\*M):** Downloads come in as
`/gems/foo-1.0.0.gem` (or `/gems/foo-1.0.0-x86_64-linux.gem` for
non-`ruby` platforms). Rather than parse the filename — the platform
substring can itself contain hyphens — we list all RubyGems packages
in the tenant, list their versions, and recompute the canonical
filename until we hit a match. For tenant sizes a single mirror sees
this is fine; if it ever becomes a bottleneck we add a
`(tenant_id, lower_filename) → version_id` lookup table.

**Auth: bare-token Authorization header:** `gem push` sends the raw
token as the `Authorization` header value with no `Bearer ` scheme
prefix. We extend `auth.extractToken` to recognize headers that start
with our token prefix (`pkm_`) and treat the whole value as the token.
Safe because the prefix is unambiguous; same fallback also enables
naive cargo / chef / a few other clients that don't bother with
scheme prefixes either. Existing `Bearer` and `Basic` handling is
unchanged.

**`latest_specs` picks newest-by-upload, not highest-semver:** We don't
parse gem versions for comparison. The newest upload wins. Both `gem`
and Bundler re-validate against the compact index `/info/<gem>` before
resolving anyway, so the legacy specs response is purely advisory.
Documented explicitly in the handler so a future reader doesn't think
it's a bug.

**What I'd revisit:**
- A proper Ruby version comparator so `latest_specs` matches what
  upstream rubygems.org returns (highest semver).
- Format-aware lookup index for the filename→version path described
  above, once we have a workload that warrants it.
- The `/quick/Marshal.4.8` gemspec skeleton hard-codes
  `@rubygems_version = "3.2.3"` and `@specification_version = 4`. Real
  gems carry their own values; we should pull them from the parsed
  gemspec rather than the constants we ported from Forgejo.

---

## 2026-05-27 — npm format (third format landed)

**Decision:** Implement npm as the third package format, mounted at
`/api/packages/:tenant/npm`. Routes:

- `PUT  /:name`, `PUT  /@:scope/:name` — publish (single JSON document with base64 tarball)
- `GET  /:name`, `GET  /@:scope/:name` — packument
- `GET  /:name/-/:filename`, `GET  /@:scope/:name/-/:filename` — tarball download
- `GET  /-/package/:name/dist-tags` (+ scoped) — list dist-tags
- `PUT  /-/package/:name/dist-tags/:tag` (+ scoped) — set dist-tag
- `DELETE /-/package/:name/dist-tags/:tag` (+ scoped) — remove dist-tag

Parser modeled on `forgejo/modules/packages/npm/creator.go` (MIT). The
publish handler is structurally similar to Forgejo's, but uses the
pkgmirror service layer (`Service.CreatePackageAndAddFile`) and our
content-addressed blob storage.

**Scoped routing:** Gin's path parameters can't express `@scope/name` as a
single segment, so each route is registered twice — once unscoped
(`/:name`) and once scoped (`/@:scope/:name`). A `packageNameFromParams`
helper reassembles `@scope/name` from the two params at the top of every
handler. This costs us a few extra route registrations but keeps the rest
of the handler logic identical to the unscoped case.

**Dist-tags:** Stored as per-version `package_properties` rows keyed
`npm.tag.<tagname>` so a version can carry any number of tags
simultaneously and tags can be moved by deleting from the old version and
inserting on the new one. The `npm publish` body sets `dist-tags.latest`
on every publish — we honor that as a property on the published version,
which means listing dist-tags re-aggregates across all versions of the
package. (Alternatives considered: a per-package `dist_tags` JSON column;
rejected because it would race the multi-version write path and require
a new schema migration.)

**Single tarball per version:** Unlike PyPI (which has many files per
version — sdist + N wheels), npm uploads exactly one tarball per
`name@version`. We use `Service.CreatePackageAndAddFile` (the same path
used by goproxy), not the PyPI multi-file path.

**Lookup name:** `strings.ToLower(name)`. npm package names are already
constrained to be lowercase per the validation rules, but ToLower is
defensive in case clients send mixed case for scopes (`@Acme/Foo`).

**Integrity verification:** The `dist.integrity` field on every publish
is a Subresource Integrity (SRI) string. We recompute `sha512` of the
decoded attachment and reject the upload with 400 if it doesn't match.
This catches both transit corruption and malicious tampering of the
attached tarball before it ever touches blob storage.

**Semver dependency:** Added `github.com/hashicorp/go-version` for
version comparison/sorting in the packument view. Considered writing a
minimal semver parser by hand but the edge cases (pre-release ordering,
build metadata, `1.10.0` vs `1.2.0` numeric vs lexicographic) aren't
worth re-deriving when there's a mature library.

**What I'd revisit:** the npm protocol has a deprecated `_revisions`
mechanism for safe concurrent updates; we ignore it for now since no
modern client requires it. If we ever support a UI-driven "unpublish"
flow we'll need to plumb it through.

---

## 2026-05-27 — PyPI format (second format landed)

**Decision:** Implement PyPI as the second package format. Three endpoints:

- `POST /api/packages/:tenant/pypi/` — multipart "legacy upload" API
  (what `twine` and `pip upload` speak)
- `GET  /api/packages/:tenant/pypi/simple[/]` — PEP 503 root index
  and `GET .../simple/:name/` per-package index (HTML or PEP 691 JSON
  via Accept negotiation)
- `GET  /api/packages/:tenant/pypi/files/:name/:version/:filename` — download

Modeled on `forgejo/routers/api/packages/pypi/pypi.go` (MIT). No parser is
needed: package metadata arrives as multipart form fields (author, summary,
requires_python, etc.), not embedded in the wheel/sdist file itself. We
store per-version metadata in `package_properties` under `pypi.*` keys.

**Name canonicalization:** full PEP 503 normalization
(`re.sub(r"[-_.]+", "-", name).lower()`), stricter than Forgejo's partial
form (Forgejo only replaces individual `_` and `.` and does not lowercase).
To preserve the user-supplied display case (e.g. `Foo_Bar` shows as
`Foo_Bar` in the UI/JSON while being addressable as `foo-bar` in URLs)
we introduced `models.GetOrCreatePackageWithLookup` /
`GetPackageByLookup` and a `PackageLookupName` field on `pkgsvc.CreationInfo`.
The lookup key is separate from the display name and persists in the
existing `lower_name` column — no schema change.

**Multi-file versions:** PyPI's model is "one version → many files" (an
sdist + one or more wheel variants). Added
`pkgsvc.Service.CreatePackageOrAddFileToExisting` that, unlike
`CreatePackageAndAddFile`, tolerates an already-existing version and
attaches the new file to it. The existing
`UNIQUE(version_id, lower_name)` on `package_files` still surfaces a real
duplicate as `models.ErrDuplicatePackageFile` → 409.

**Black-box conformance:** uploads a **wheel** (not an sdist) and installs
it via `pip install --target=/work/site` in `python:3.12-slim`. Wheels are
pre-built so they install cleanly in stripped-down Python images that lack
`setuptools`. We construct a minimal PEP 427 universal wheel in-process
(`buildWheel` in `tests/blackbox/pypi/wheel_test.go`) so there's no
external fixture dependency. PEP 691 JSON conformance covered by a
separate host-side assertion.

**HTTP-vs-HTTPS:** unlike Go, pip honors in-URL Basic-auth credentials and
`.netrc` over plain HTTP, but it does require `--trusted-host` (or
`PIP_TRUSTED_HOST`) for non-HTTPS index URLs. The blackbox test sets
`PIP_TRUSTED_HOST=pkgmirror` and uses the (public, by harness convention)
default tenant, consistent with the goproxy strategy. Private-tenant +
authenticated `pip` flows work today over plain HTTP via in-URL creds; in
production we'd still terminate TLS in front of pkgmirror.

**Validation:** unit tests cover upload (auth gate, validation, sha256
mismatch, dup filename, multi-file version), HTML + JSON simple index,
download roundtrip, anonymous reads on public tenants. Black-box drives
real `pip install` + `python -c 'import foo; print(foo.greet())'` against
`python:3.12-slim`.

---

## 2026-05-27 — Multi-tenancy, users, and token auth

**Decision:** Introduce three new concepts and require an authenticated
identity for non-public reads and all writes:

- **`tenants`** — flat namespaces, primary key on `(tenant_id, type, name)`.
  Tenant name appears in URLs: `/api/packages/<tenant>/<format>/…`. Each
  tenant has a `visibility` (`private` / `public`).
- **`users`** — principals, either `human` or `service`. Decoupled from
  tenants so one user can belong to multiple tenants with different roles.
  `external_provider` + `external_subject` columns are forward-compat for
  OIDC/LDAP without forcing those today.
- **`tenant_members`** — many-to-many between users and tenants with a role
  (`reader` / `writer` / `tenant_admin`).
- **`tokens`** — opaque `pkm_<32 base32>` bearer strings, stored as
  `sha256(plaintext)` hex. Carry CSV scopes (`read`, `write`, `admin`) and
  optional `tenant_scope` + `expires_unix`.

Auth flows through the new `auth.Authenticator` interface; today's only
implementation is `TokenAuthenticator`. Future identity sources (OIDC,
LDAP, reverse-proxy header, mTLS) slot in alongside without handler
changes. See `docs/auth.md` for the full spec.

**Rationale:** the user confirmed multi-tenant isolation is a v1
requirement. Separating users from tenants (rather than collapsing both
into a Forgejo-style polymorphic `User` row) keeps the model crisp and
makes the OIDC/audit story straightforward.

**Crypto note:** SHA-256 (not bcrypt) for token hashing. Tokens have ~160
bits of entropy from `crypto/rand` — rainbow tables and brute force are
irrelevant; bcrypt would only add cost without security. This matches how
GitHub stores `ghp_…` PATs.

**Bootstrap:** on every start, `internal/bootstrap.Ensure` idempotently
creates the `admin` service user, the `default` tenant (with visibility
from `PKGMIRROR_DEFAULT_TENANT_VISIBILITY`), the admin → default
membership, and an initial admin token (from `PKGMIRROR_ADMIN_TOKEN` or
freshly minted and printed once to stderr).

**Migrations:** new `migrations` slice in `internal/db/db.go` driven by
`PRAGMA user_version`. v1 is the original schema; v2 adds tenants/users/
members/tokens and rewrites `packages` with the new `UNIQUE(tenant_id,
type, lower_name)` constraint. SQLite WAL was dropped from the DSN because
modernc.org/sqlite leaves `-wal`/`-shm` files past `Close()`, breaking
test cleanup; default rollback journaling is fine for our single-writer
workload.

**Known limitation:** in black-box tests we default the tenant to
`public` because the Go toolchain refuses to send Basic-auth credentials
over plain HTTP (a hardcoded rule, not configurable via GOAUTH/netrc/URL).
Production deploys must terminate TLS in front of pkgmirror for `go` (and
several other clients) to send credentials. Auth-gate behavior is
exhaustively covered by the in-process unit tests, which have no transport
restriction. See `docs/auth.md` "Per-format credential supply" for the
detail.

**Validation:** unit + black-box conformance suites pass. The blackbox
suite covers: anonymous read on public tenant succeeds; upload without
token returns 401; `go mod download`/`go build`/run flow works against
the real `golang:1.22-bookworm` client.

**Revisit when:** we add OIDC, web sessions, a token CRUD API, or per-
package visibility (currently only per-tenant). Also when we decide on
TLS termination — built-in vs always-via-proxy.

---

## 2026-05-27 — Black-box conformance via dockerized native clients

**Decision:** Conformance for each package format is validated by running the
real ecosystem client (`go`, `npm`, `pip`, `mvn`, …) in an official docker
container against a `pkgmirror` container, both on a per-test private docker
network. Orchestration uses
[`testcontainers-go`](https://golang.testcontainers.org/). All black-box
files are gated behind `//go:build blackbox`, so the default
`go test ./...` stays sub-second and CI-friendly even without docker.

**Rationale:** the only reliable way to prove wire-format compatibility
with an ecosystem is to drive its actual client. Containerizing both sides
eliminates "works on my mac because brew installed npm 10 but CI has 18"
drift and makes the test identical locally and on any CI runner with a
docker daemon. testcontainers' Ryuk reaper guarantees cleanup even on
crashed test processes.

**Implementation notes:**

- Dockerfile is intentionally BuildKit-free (no `--mount=type=cache`) so the
  legacy docker daemon builder used by testcontainers-go v0.42 can build it.
  Layer caching across runs is still good enough.
- `Client.Exec` wraps each command in `sh -c '… 2>&1'` and uses
  `tcexec.Multiplexed()` so callers receive a clean text stream instead of
  docker's framed stdout/stderr multiplex.
- See [`docs/blackbox-testing.md`](docs/blackbox-testing.md) for the full
  contract and "how to add a new format" guide.

**Validation:** `make test-blackbox` builds the pkgmirror image (~1 min cold,
seconds warm), brings up a `golang:1.22-bookworm` client container, drives
`go mod download` / `go list -m -versions` / `go build` against the mirror,
and runs the resulting binary. Two tests cover single-version and
multi-version flows.

**Revisit when:** we want a multi-version client matrix (e.g. test against
go 1.21 *and* 1.22), or want to test directly through the official
`GOPROXY` redirection behavior.

---

## 2026-05-27 — Build a fresh service, do not vendor Forgejo

**Decision:** Treat the spec's "Fork the Forgejo project and extract the package
registry code" as a *learning* exercise rather than a literal extraction. Build a
new, small Go service in `pkgmirror/` that reimplements only what is needed,
using Forgejo's code as a reference design.

**Rationale:** Forgejo's package registry is deeply coupled to:

- the Forgejo ORM (`forgejo.org/models/db`) on top of XORM
- the org/user "owner" concept (every package belongs to a `User` row)
- the Forgejo `context.Context` request abstraction, auth, sessions, CSRF, ACL
- the Forgejo storage abstraction (local/minio/azure)
- a giant web of `services/` packages

Verbatim extraction would drag in tens of thousands of lines and leave us with
a Forgejo-shaped service we couldn't actually run. The spec explicitly allows
"a whole new web server/framework if it makes sense" and prefers Gin + `html/template`.
So: borrow the *protocol* code (e.g. zip parsing for Go modules) and the *schema*
shape, write fresh handlers.

**Revisit when:** scaling to many formats — we may want to vendor specific
parser modules from `modules/packages/<fmt>/` rather than rewrite each one.

---

## 2026-05-27 — Web framework: Gin

**Decision:** Use `github.com/gin-gonic/gin` for HTTP routing.

**Rationale:** Spec calls it out by name. Mature, popular, simple middleware
model, good for both JSON APIs and `html/template` rendering.

---

## 2026-05-27 — Metadata DB: SQLite via `modernc.org/sqlite`

**Decision:** Single embedded SQLite database for all metadata. Use the
pure-Go driver `modernc.org/sqlite` (no CGO required).

**Rationale:** Simplest possible deployment story for an early-stage service.
A package mirror is overwhelmingly read-heavy; SQLite handles this fine for a
single-node deployment. Pure-Go driver keeps `go build` trivial on any platform.

**Revisit when:** we need multi-node / HA, or when write contention from
concurrent uploads becomes a real problem. Schema is plain SQL so a
Postgres swap is straightforward later.

---

## 2026-05-27 — Blob storage: filesystem, content-addressed by SHA-256

**Decision:** Store package file bytes on the local filesystem at paths
derived from the SHA-256 of the file content (`<root>/aa/bb/aabbcc…`).
Metadata DB references blobs by their hash. Identical blobs are stored once.

**Rationale:** Mirrors Forgejo's `package_blobs` approach. Trivial to
implement, dedupes naturally, easy to swap behind an interface (`storage.Backend`)
for S3/MinIO/Azure later.

---

## 2026-05-27 — No multi-tenant "owners" yet

**Decision:** Drop Forgejo's per-user/per-org package ownership. A package
is uniquely identified by `(type, name)` globally in this service.

**Rationale:** The mirror has no users — it's a single shared cache/mirror.
Adding owners is straightforward (add `owner_id` column + scope routes) when
we need ACLs.

**Revisit when:** authentication is added or different teams need isolated
namespaces.

---

## 2026-05-27 — No authentication on v1

**Decision:** All endpoints are open. Uploads via `PUT /api/packages/go/upload`
are unauthenticated.

**Rationale:** Spec explicitly flags "questions about how to handle authentication"
as something to be decided as we go. Keeping it open lets us validate the
end-to-end flow without infrastructure dependencies.

**Revisit when:** before any deploy beyond localhost. Likely add either a
shared bearer token (`PKGMIRROR_UPLOAD_TOKEN`) or OIDC.

**Open question for the user:** which auth model do you want — shared
secret, OIDC, mTLS, or integrated with an existing IdP?

---

## 2026-05-27 — UI: server-rendered `html/template` + Bootstrap 5 from CDN

**Decision:** Minimal server-rendered pages (index, per-package, per-version),
Bootstrap loaded from jsDelivr CDN. No build step, no JS framework.

**Rationale:** Spec says "We do NOT want to replicate the Forgejo UI" and
"we can simply start with Bootstrap" + "simplicity (such as go http templates)".

---

## 2026-05-27 — Go module proxy is the first format

**Decision:** Implement `go` first, matching the protocol described at
<https://go.dev/ref/mod#goproxy-protocol>.

**Rationale:** Spec lists it as the starting example. The protocol is small
and well-specified (5 endpoints), and the zip format is documented.
Forgejo's `modules/packages/goproxy/metadata.go` parser is ~90 lines we can
reimplement cleanly.

**Endpoints implemented:**

- `GET /api/packages/go/<module>/@v/list`
- `GET /api/packages/go/<module>/@v/<version>.info`
- `GET /api/packages/go/<module>/@v/<version>.mod`
- `GET /api/packages/go/<module>/@v/<version>.zip`
- `GET /api/packages/go/<module>/@latest`
- `PUT /api/packages/go/upload` (mirror-population endpoint — not part of
  the standard `GOPROXY` protocol, modeled on Forgejo's upload route)

**Validation:** Verified end-to-end against the real `go` toolchain:

```sh
GOPROXY=http://127.0.0.1:18080/api/packages/go GOSUMDB=off \
  go mod download -x example.com/foo
# 3× 200 OK on .info, .mod, .zip
```

Integration tests in `internal/packages/goproxy/handler_test.go` cover upload,
list, info, mod, zip, @latest, duplicate-upload (409), missing version (404),
and multi-version ordering.

---

## 2026-05-27 — Module path parsing

**Decision:** A Go module path can contain `/`. The proxy URL embeds it
literally (e.g. `example.com/foo/bar/@v/v1.0.0.info`). I route on
`/api/packages/go/*path` and parse `<module>/@v/<file>` from `path` in code,
rather than try to express it as a Gin route pattern.

**Rationale:** Gin's tree router can't express "any number of segments,
then a literal `@v` segment". A single catch-all + manual split is
straightforward and matches how the spec / Go toolchain emit these URLs.
