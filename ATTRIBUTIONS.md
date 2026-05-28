# Attributions

pkgmirror is licensed under the MIT License (see [`LICENSE`](LICENSE)). This
document is a more detailed accounting of code, designs, and protocols
adapted from other projects than the LICENSE file itself can comfortably
hold. It is published as a courtesy in the spirit of giving credit
where credit is due, beyond what the license strictly requires.

If you spot an attribution that's missing, vague, or incorrect, please
open an issue — we'd rather over-credit than under-credit.

---

## Primary upstream: Forgejo / Gitea

pkgmirror's package format support is heavily indebted to the
[Forgejo](https://codeberg.org/forgejo/forgejo) project and its
ancestor [Gitea](https://github.com/go-gitea/gitea). Forgejo as a
combined work is GPL-3.0; however, the specific source files we ported
from carry per-file `SPDX-License-Identifier: MIT` headers and
`Copyright The Gitea Authors`. Those files are MIT-licensed and were
imported into pkgmirror under the same MIT license that governs
pkgmirror as a whole.

The MIT license requires that the original copyright notice be
preserved in "all copies or substantial portions of the Software." We
preserve it in three places:

1. The per-file headers on every derived source file (see the file
   listing below).
2. The top-level [`LICENSE`](LICENSE) file's attribution block.
3. This document.

### File-by-file mapping

Format: `pkgmirror file` ← `upstream Forgejo file` — description of the
adaptation.

#### Service layer

- [`internal/packages/service.go`](internal/packages/service.go)
  ← `services/packages/packages.go`,
  `modules/packages/hashed_buffer.go`,
  `modules/packages/multi_hasher.go` — the multi-hash buffered upload
  staging area and the `CreatePackageAndAddFile` /
  `CreatePackageOrAddFileToExisting` orchestration are modeled
  directly on the upstream. The hash-set choice
  (MD5+SHA1+SHA256+SHA512) and the rename-into-blob-store flow are
  preserved verbatim because every format handler depends on the same
  shape.
- [`internal/storage/storage.go`](internal/storage/storage.go)
  ← `modules/storage/storage.go`,
  `modules/storage/local.go` — the `ObjectStorage` interface and the
  `<aa>/<bb>/<full-sha256>` content-addressed sharding scheme were
  ported so future drop-in S3/MinIO backends can match upstream
  expectations.
- [`internal/syncutil/exclusive_pool.go`](internal/syncutil/exclusive_pool.go)
  ← `modules/sync/exclusive_pool.go` — refcount-driven map-of-mutexes
  used to serialize per-key writes (Maven's multi-file `mvn deploy`
  today; future Debian / NuGet / RPM formats will use it too). The
  refcount-and-delete behavior is the load-bearing property: memory
  stays bounded by *concurrent* keys, not unique keys ever seen.
  Originally from Gogs; preserved verbatim from forgejo.

#### Go module proxy

- [`internal/packages/goproxy/parser.go`](internal/packages/goproxy/parser.go)
  ← `modules/packages/goproxy/metadata.go` — direct transliteration of
  the zip-walker that extracts `go.mod` and computes the module
  version. The algorithm is preserved byte-for-byte; differences are
  limited to error wrapping and stylistic cleanup.
- [`internal/packages/goproxy/handler.go`](internal/packages/goproxy/handler.go)
  ← `routers/api/packages/goproxy/goproxy.go` — the 5-endpoint shape
  (`/@v/list`, `/@v/<ver>.info`, `.mod`, `.zip`, `/@latest`) and the
  semver-sort-and-pick-newest "latest" logic are modeled on upstream.

#### PyPI

- [`internal/packages/pypi/parser.go`](internal/packages/pypi/parser.go)
  ← `modules/packages/pypi/metadata.go` — METADATA / PKG-INFO parser
  for wheels and sdists. The wheel-vs-sdist file-shape sniff and the
  PEP 503 normalization (`re.sub(r"[-_.]+", "-", name).lower()`) are
  preserved.
- [`internal/packages/pypi/handler.go`](internal/packages/pypi/handler.go)
  ← `routers/api/packages/pypi/pypi.go` — the legacy
  `:action=file_upload` multipart shape and the PEP 503 simple index
  HTML + PEP 691 JSON output are modeled on upstream so real `twine`
  and `pip` clients need no special-casing.

#### npm

- [`internal/packages/npm/parser.go`](internal/packages/npm/parser.go)
  ← `modules/packages/npm/metadata.go` — the publish-payload parser
  (single JSON document with base64 tarball) and the SRI `integrity`
  verification flow are ported. The scoped-name normalization
  (`@scope/name` → `@scope%2Fname`) follows upstream.
- [`internal/packages/npm/handler.go`](internal/packages/npm/handler.go)
  ← `routers/api/packages/npm/npm.go` — packument, tarball download,
  dist-tag CRUD, and the scoped-package routing all model upstream.

#### RubyGems

- [`internal/packages/rubygems/parser.go`](internal/packages/rubygems/parser.go)
  ← `modules/packages/rubygems/metadata.go` — `.gem` tarball parser,
  zlib-marshal `metadata.gz` decode, dependency extraction.
- [`internal/packages/rubygems/marshal.go`](internal/packages/rubygems/marshal.go)
  ← `modules/packages/rubygems/marshal.go` — minimal Ruby `Marshal`
  v4.8 encoder. The supported types (`nil`, `true`, `false`, integer,
  string, symbol, array, user-marshal) are exactly the set upstream
  emits because `specs.4.8.gz` and `*.gemspec.rz` parse with the
  reference `Marshal.load`.
- [`internal/packages/rubygems/handler.go`](internal/packages/rubygems/handler.go)
  ← `routers/api/packages/rubygems/rubygems.go` — the dual legacy
  (`/specs.4.8.gz`, `/quick/Marshal.4.8/*.gemspec.rz`) and modern
  compact-index (`/info/<gem>`, `/versions`) endpoints are modeled on
  upstream so both old `gem` and new Bundler clients work.
- [`internal/packages/rubygems/parser_test.go`](internal/packages/rubygems/parser_test.go)
  ← `modules/packages/rubygems/metadata_test.go` — test fixtures
  reused so we exercise the same edge cases the upstream parser does.

#### Container / OCI

- [`internal/packages/container/manifest.go`](internal/packages/container/manifest.go)
  ← `modules/packages/container/metadata.go` — OCI manifest +
  manifest-list parser and the digest-from-bytes computation.
- [`internal/packages/container/upload.go`](internal/packages/container/upload.go)
  ← `services/packages/container/blob_uploader.go` (conceptual) —
  the upload-session-as-temp-file model is from Forgejo; the
  in-memory tracker + idle-sweeper + orphan-sweeper implementation
  is original to pkgmirror but solves the same problem the upstream
  service does.
- [`internal/packages/container/handler.go`](internal/packages/container/handler.go)
  ← `routers/api/packages/container/container.go`,
  `routers/api/packages/container/auth.go` — `/v2/` route shape, the
  three-step POST → PATCH → PUT chunked-upload protocol, and the
  token-exchange auth dance. The multi-segment-image-name dispatcher
  is modeled on upstream's regex catch-all.

#### Generic

- [`internal/packages/generic/handler.go`](internal/packages/generic/handler.go)
  ← `routers/api/packages/generic/generic.go` — PUT/GET/DELETE shape
  and the "delete file; if last file, delete version" cascading
  semantics.

#### Alpine (apk)

- [`internal/packages/alpine/parser.go`](internal/packages/alpine/parser.go)
  ← `modules/packages/alpine/metadata.go` — the multistream-gzip
  walker, the `Q1<base64(sha1)>` checksum computation, and the
  PKGINFO key=value parser are direct transliterations. The exact
  PKGINFO key set (pkgname, pkgver, builddate, depend, provides, …)
  is preserved.
- [`internal/packages/alpine/parser_test.go`](internal/packages/alpine/parser_test.go)
  ← `modules/packages/alpine/metadata_test.go` — fixture (the
  `createPKGINFOContent` helper, the "Q1..." byte-exact checksum
  assertion) ported so we lock the same edge cases.
- [`internal/packages/alpine/index.go`](internal/packages/alpine/index.go)
  ← `services/packages/alpine/repository.go` — APKINDEX.tar.gz
  construction, the detached-signature-as-separate-gzip-stream
  format, the per-tenant RSA keypair generation, and the `writeGzipStream`
  helper. (We added an explicit `Flush()` to fix a tar-padding bug
  that's latent in the upstream too at non-aligned signature sizes.)
- [`internal/packages/alpine/handler.go`](internal/packages/alpine/handler.go)
  ← `routers/api/packages/alpine/alpine.go` — the five endpoint shape,
  the `noarch` fan-out, and the property-based file metadata storage
  are modeled on upstream. We embed the `(branch|repo|arch)` composite
  key into the file name instead of adding a `composite_key` column.

#### Maven

- [`internal/packages/maven/parser.go`](internal/packages/maven/parser.go)
  ← `modules/packages/maven/metadata.go` — direct transliteration of
  the POM XML parser, including the parent-`<groupId>`-inheritance
  fallback and the charset-aware XML decoder for ISO-8859-1 / etc.
  POMs in the wild.
- [`internal/packages/maven/parser_test.go`](internal/packages/maven/parser_test.go)
  ← `modules/packages/maven/metadata_test.go` — fixtures and the
  ISO-8859-1 encoding test ported verbatim so we exercise the same
  edge cases.
- [`internal/packages/maven/handler.go`](internal/packages/maven/handler.go)
  ← `routers/api/packages/maven/maven.go`,
  `routers/api/packages/maven/api.go` — the catch-all path parsing
  (`<groupId-as-path>/<artifactId>/<version>/<filename>`), the
  per-extension dispatch (pom triggers metadata extraction, checksum
  sidecars verified-but-not-stored, jar/other as ordinary blobs), and
  the on-demand `maven-metadata.xml` builder are modeled on upstream.
  Per-package upload locking uses a `sync.Map` rather than forgejo's
  `ExclusivePool` but serves the same purpose.

---

## OCI Distribution Spec

The `/v2/...` route shape, the manifest media types, the chunked blob
upload state machine, and the digest format used by the container
registry are governed by the
[OCI Distribution Specification](https://github.com/opencontainers/distribution-spec)
(Apache-2.0). The spec itself is not code, but every wire-format
decision in `internal/packages/container/` is derived from it.

## go-containerregistry (test-only)

The blackbox conformance suite in `tests/blackbox/container/` uses
[`github.com/google/go-containerregistry`](https://github.com/google/go-containerregistry)
(Apache-2.0) to drive a real OCI client against pkgmirror. It is a test
dependency only; no go-containerregistry code is compiled into the
shipped binary.

## testcontainers-go (test-only)

The blackbox harness uses
[`github.com/testcontainers/testcontainers-go`](https://github.com/testcontainers/testcontainers-go)
(MIT) to spin up sibling container images. Test-only.

## Other Go module dependencies

A full machine-readable list of direct and transitive Go module
dependencies (and their licenses) lives in
[`go.mod`](go.mod) and `go.sum`. Every dependency we link into the
shipped binary is permissively licensed (MIT, Apache-2.0, BSD-3-Clause,
or equivalent); we do not link any GPL or LGPL code into the
production binary.

---

## What we did NOT take from Forgejo

For clarity, the following pieces are pkgmirror-original designs, not
ports:

- The supply-chain policy engine (`internal/policy/`): the
  evaluator-stack model, cooldown evaluator, license allowlist
  evaluator, and the cascading-rule specificity calculus are
  pkgmirror-original. Forgejo has no policy engine of this shape.
- The audit log (`internal/audit/`): buffered logger + drainer
  goroutine + SQLite sink. Forgejo logs but does not have a
  structured audit table or query API of this kind.
- Multi-tenancy (`internal/tenants/`): Forgejo uses its user/org
  model; pkgmirror uses a flatter tenant model with explicit
  visibility and membership tables.
- The `pkm_*` token scheme and the per-format compatibility shims
  (`internal/auth/`, `internal/tokens/`): pkgmirror-original token
  format and middleware, designed to make every CLI work unmodified.
- The admin HTTP API (`internal/admin/`).
- The HTML UI (`internal/ui/` and `templates/`).
- The blackbox harness (`tests/blackbox/harness/`).
- The Makefile, Dockerfile, CI workflow, and all documentation
  under `docs/`.

This list is offered for honesty about provenance, not as a claim of
exclusivity.
