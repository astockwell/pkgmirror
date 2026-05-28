# Adding a package format

This document is the implementation playbook for every package format the
mirror supports. It's structured into three parts:

1. **[Anatomy](#anatomy-of-a-format)** — the components every format needs
   and how they fit into the existing pkgmirror tree.
2. **[Reference: Forgejo / Gitea](#reference-forgejo--gitea)** — the
   most important resource we have. Forgejo already implements every
   format on the roadmap. We should heavily borrow from it, with
   attribution.
3. **[Per-format recipes](#per-format-recipes)** — for each format on the
   roadmap, the protocol summary, links to the upstream Forgejo files to
   crib from, parser/handler complexity notes, and the docker image +
   commands the black-box conformance test should drive.

Read this end-to-end before starting a new format. It will save you
re-discovering the same wheel.

---

## Anatomy of a format

Every format consists of the same five concerns, mapped onto the same five
directories. Use the existing `go` (goproxy) implementation as the canonical
example:

| Concern | Where it lives | Tested by | Reference |
| --- | --- | --- | --- |
| **Wire format parser** | `internal/packages/<fmt>/parser.go` | unit (table-driven against fixtures) | `internal/packages/goproxy/parser.go` |
| **HTTP handlers** | `internal/packages/<fmt>/handler.go` | grey-box in-process (`httptest`) | `internal/packages/goproxy/handler.go` |
| **Route registration** | `internal/server/server.go` adds a group at `/api/packages/:tenant/<fmt>` | covered by handler tests | the `goGroup := r.Group(...)` line |
| **Format-specific auth shim** _(optional)_ | inline in `handler.go` or sibling `auth.go` | grey-box + black-box | see [Container](#container-oci) section for the most complex case |
| **Black-box conformance** | `tests/blackbox/<fmt>/conformance_test.go` | docker-based against the real client | `tests/blackbox/goproxy/conformance_test.go` |

What you do **NOT** need per format (these are shared infrastructure that
already exists):

- The `packages` / `package_versions` / `package_files` / `package_blobs` /
  `package_properties` schema — extend `package_properties` for any
  format-specific metadata you can't fit elsewhere; do not add new tables
  without a strong reason.
- `pkgsvc.HashedBuffer` / `pkgsvc.Service.CreatePackageAndAddFile` /
  `storage.FS` — the upload, hashing, and content-addressed blob storage
  pipeline. Every format funnels through these.
- The auth middleware and `RequireRead` / `RequireWrite` helpers.
- The Bootstrap UI, tenant resolution from `:tenant` path param,
  `WWW-Authenticate` challenge on 401.

### Step-by-step recipe

1. **Read the protocol spec** for the format (links per-format below).
2. **Read Forgejo's implementation** (see [Reference](#reference-forgejo--gitea)).
3. **Port the parser** into `internal/packages/<fmt>/parser.go`. Keep it
   pure (no DB, no HTTP). Add SPDX + Forgejo attribution headers — see
   [Attribution](#attribution-to-forgejo--gitea).
4. **Port unit tests** from `forgejo/modules/packages/<fmt>/metadata_test.go`
   into `internal/packages/<fmt>/parser_test.go`. The fixtures Forgejo
   uses are typically inlined in the test file; bring them across.
5. **Implement the handlers** in `internal/packages/<fmt>/handler.go`.
   Follow `goproxy/handler.go`'s shape:
   - `Handler` struct holds `*pkgsvc.Service`, `*models.Store`,
     `*tenants.Store`.
   - `Register(g *gin.RouterGroup)` mounts the routes.
   - Each handler:
     1. Resolves tenant from `:tenant` path param via `tenantFromPath`.
     2. Calls `auth.RequireRead` or `RequireWrite`.
     3. Delegates to models / service.
     4. Maps sentinel errors to HTTP status (404, 409, etc.).
6. **Wire the route** in `internal/server/server.go` (one line — a new
   `r.Group("/api/packages/:tenant/<fmt>")` and a `Register` call).
7. **Add grey-box tests** in `internal/packages/<fmt>/handler_test.go`
   modeled on `goproxy/handler_test.go`: spin up `httptest.NewServer`,
   exercise the protocol end-to-end including auth gates (401 on private
   tenant, 201 on upload, 200 on read, 409 on dup, etc.).
8. **Add a black-box conformance suite** in
   `tests/blackbox/<fmt>/conformance_test.go` per the per-format recipe.
9. **Pre-pull the client image** in `.github/workflows/ci.yml` `images=()`
   array.
10. **Update DECISIONS.md** with any non-obvious choices.

### Format-specific metadata: `package_properties`

The shared `package_properties(ref_type, ref_id, name, value)` table is the
extension point for anything you can't represent in the core columns.
Forgejo uses it the same way. Examples:

| Format | Properties typically stored |
| --- | --- |
| `go` | `go.mod` (full text of the module's go.mod) |
| `npm` | `npm.tag.latest`, `npm.tag.next`, etc. (dist-tag → version) |
| `container` | manifest digest, media type, layer digests |
| `maven` | groupId, artifactId, packaging, classifier |
| `rubygems` | platform, ruby_version requirement |

When in doubt, look at how Forgejo encodes the same metadata
(usually in `services/packages/<fmt>/<fmt>.go` or alongside the parser).

---

## Reference: Forgejo / Gitea

**Forgejo (and upstream Gitea) implements every package format on our
roadmap.** It is the single most valuable resource for this project. The
parsers, protocol quirks, and edge cases have been battle-tested by real
ecosystems for years.

### Where to look in the Forgejo tree

The Forgejo source is cloned in `forgejo/` next to this repo. For every
format we plan to implement, three locations matter:

```
forgejo/routers/api/packages/<fmt>/    HTTP handlers, route registration, auth
forgejo/modules/packages/<fmt>/        wire-format parsers + unit tests
forgejo/services/packages/<fmt>/       higher-level orchestration (some formats only)
```

Plus a few cross-cutting files used by every format:

- `forgejo/routers/api/packages/api.go` — central route registration for
  all formats; useful when you need to understand how Forgejo dispatches
  protocol-specific URL shapes.
- `forgejo/routers/api/packages/helper/` — shared response helpers (e.g.
  `ServePackageFile`, error mapping).
- `forgejo/services/packages/packages.go` — the `CreatePackageAndAddFile`
  orchestration our `pkgsvc.Service` is modeled on.
- `forgejo/modules/packages/hashed_buffer.go`,
  `forgejo/modules/packages/multi_hasher.go` — the upload buffer pattern
  our `pkgsvc.HashedBuffer` is modeled on.

### How to use it

**Default to transliteration, not invention.** The Forgejo authors have
already done the work of decoding each ecosystem's protocol — sometimes
from incomplete official docs, sometimes by reverse-engineering reference
clients. Reinventing this risks subtle bugs that only show up in the
black-box conformance test (or worse, in production).

The right workflow per format:

1. Open `forgejo/routers/api/packages/<fmt>/*.go` and skim every handler
   to understand the protocol shape.
2. Open `forgejo/modules/packages/<fmt>/*.go` and read the parser. Note
   any non-obvious behavior (size limits, special-case headers, version
   normalization, etc.).
3. Open the corresponding `_test.go` files. The test fixtures often
   document the protocol better than any spec.
4. Port the code into our tree, adapting to our Gin + Tenants + Auth shape.
   Most of the work is mechanical:
   - `forgejo.org/services/context` → `*gin.Context`
   - `ctx.Package.Owner` → `tenant := tenantFromPath(c)` + auth check
   - `packages_model.GetPackage(...)` → `h.Models.GetPackage(ctx, tenant.ID, ...)`
   - `packages_service.CreatePackageAndAddFile(...)` → `h.Service.CreatePackageAndAddFile(...)` with `TenantID: tenant.ID`
   - `apiError(...)` → `c.String(status, ...)`
   - sentinel errors are essentially the same shape; map them to ours
5. Run the unit tests, then the black-box conformance, then iterate.

### Attribution to Forgejo / Gitea

Forgejo and Gitea are both MIT licensed; this project is also MIT licensed
(see `LICENSE`). Reuse is welcome and explicitly compatible. We mark
provenance for the avoidance of any doubt:

- **For every file that is a transliteration or close adaptation** of a
  Forgejo file, prepend the SPDX + attribution header used by our existing
  derived files (`internal/packages/goproxy/parser.go`,
  `internal/packages/goproxy/handler.go`, `internal/packages/service.go`):

  ```go
  // Copyright 2026 Alex Stockwell and pkgmirror contributors.
  // Portions Copyright <year> The Gitea Authors.
  // SPDX-License-Identifier: MIT
  //
  // This file is a transliteration of forgejo/<path>/<file> from the
  // Forgejo project (https://codeberg.org/forgejo/forgejo), which is
  // itself MIT licensed. The <function> below preserves the upstream
  // algorithm; differences are limited to <what>.
  ```

- **For files merely "inspired by" Forgejo** (handlers that copy the
  endpoint shape but the code is mostly rewritten for our Gin + Tenant +
  Auth model), the attribution should describe what was lifted (the
  endpoint set, the JSON response struct shape, etc.) rather than claim
  pure transliteration.

- **The top-level `LICENSE`** already names Forgejo and Gitea as
  collective contributors; new derived files don't change that, they just
  document which specific files came from where.

- **Do NOT remove** copyright headers from Forgejo source you transliterate.
  Keep them and append our own, as in the existing examples.

When in doubt, err toward more attribution rather than less.

---

## Per-format recipes

For each format we plan to support, this section captures:

- the **protocol spec** to read first
- the **Forgejo files** to crib from
- an estimate of **parser complexity** (the hardest part is usually the
  on-wire metadata format)
- the **black-box client image + commands** to drive in the conformance
  suite

Listed in the order I'd recommend implementing them — easiest / highest-
leverage first.

### `generic`

A pass-through "any blob with a name" registry. The simplest format and a
useful sanity check for the full upload/download/delete loop with no
ecosystem-specific quirks.

- **Spec:** [Forgejo generic registry docs](https://forgejo.org/docs/latest/user/packages/generic/)
- **Forgejo:** `forgejo/routers/api/packages/generic/generic.go` (no
  parser needed — there's no metadata).
- **Routes:** `PUT/GET/DELETE /api/packages/:tenant/generic/:package/:version/:filename`
- **Parser complexity:** trivial — none.
- **Black-box client:** `curlimages/curl:8.10.1` or any of our other
  language images. Commands:
  - `curl -u user:$TOKEN --upload-file foo.bin <url>`
  - `curl -fsSL -u user:$TOKEN <url> -o bar.bin && diff foo.bin bar.bin`

### `npm`

JavaScript packages. Excellent next step because every front-end and Node
backend service ends up wanting an npm mirror.

- **Spec:** [npm registry API](https://github.com/npm/registry/blob/master/docs/REGISTRY-API.md),
  [package metadata format](https://github.com/npm/registry/blob/master/docs/responses/package-metadata.md)
- **Forgejo:**
  - `forgejo/routers/api/packages/npm/npm.go` — full handler set (publish,
    package metadata, tarball, dist-tags, search, etc.)
  - `forgejo/routers/api/packages/npm/api.go` — JSON response shapes
  - `forgejo/modules/packages/npm/creator.go` — parses an npm "publish"
    payload (a single JSON doc with the tarball base64-embedded)
  - `forgejo/modules/packages/npm/metadata.go` — the metadata document
    returned for `GET /<name>`
- **Routes:** roughly
  - `PUT /api/packages/:tenant/npm/:name` — publish
  - `GET /api/packages/:tenant/npm/:name` — metadata
  - `GET /api/packages/:tenant/npm/:name/-/:filename` — tarball
  - `GET/POST/PUT/DELETE /api/packages/:tenant/npm/-/package/:name/dist-tags[/:tag]`
  - `GET /api/packages/:tenant/npm/-/v1/search`
- **Parser complexity:** moderate. The publish payload is non-trivial
  (base64 inside JSON), but Forgejo's `creator.go` is portable.
- **Format-specific auth:** npm clients send `_authToken` either via
  `Authorization: Bearer <token>` (newer) or a `_auth` Basic header. Our
  existing middleware already handles both.
- **Black-box client:** `node:22-bookworm`. Commands:
  ```sh
  echo "registry=http://pkgmirror:8080/api/packages/<tenant>/npm/" > ~/.npmrc
  echo "//pkgmirror:8080/api/packages/<tenant>/npm/:_authToken=$TOKEN" >> ~/.npmrc
  npm publish --no-audit --no-fund ./fixture-pkg
  cd /work/consumer && npm install --no-audit --no-fund
  node -e 'console.log(require("foo").greet())'
  ```

### `pypi`

Python packages. Standardized protocols, widely used.

- **Spec:** [PEP 503 (simple index)](https://peps.python.org/pep-0503/),
  [PEP 691 (JSON index)](https://peps.python.org/pep-0691/),
  [pypa upload](https://warehouse.pypa.io/api-reference/legacy.html#upload-api)
- **Forgejo:**
  - `forgejo/routers/api/packages/pypi/pypi.go` — handlers
  - `forgejo/routers/api/packages/pypi/pypi_test.go` — useful for
    documented edge cases (PEP 503 normalization, etc.)
  - `forgejo/modules/packages/pypi/metadata.go` — parses sdist (`.tar.gz`)
    and wheel (`.whl`) METADATA files
- **Routes:**
  - `POST /api/packages/:tenant/pypi/` — upload (multipart, legacy API)
  - `GET  /api/packages/:tenant/pypi/simple/` — root index
  - `GET  /api/packages/:tenant/pypi/simple/:name/` — per-package index (HTML)
  - `GET  /api/packages/:tenant/pypi/files/:name/:version/:filename` — download
- **Parser complexity:** moderate. Wheels are zip files with a METADATA
  text file in RFC822 format. sdists are tarballs with PKG-INFO.
- **Black-box client:** `python:3.12-slim`. Commands:
  ```sh
  pip install --index-url http://x:$TOKEN@pkgmirror:8080/api/packages/<tenant>/pypi/simple foo==1.0.0
  python -c 'import foo; print(foo.greet())'
  ```
  Upload via `twine upload --repository-url ... dist/*` or by direct
  multipart `POST` from the test process.

### `maven`

Java / Kotlin / Scala / Groovy artifacts.

- **Spec:** [Maven repository layout](https://maven.apache.org/repository/layout.html).
  No formal central spec — the de-facto protocol is "static files at
  predictable paths."
- **Forgejo:**
  - `forgejo/routers/api/packages/maven/maven.go` — handlers
  - `forgejo/routers/api/packages/maven/api.go` — response shapes
  - `forgejo/modules/packages/maven/metadata.go` — parses `pom.xml`
- **Routes:** `GET/PUT /api/packages/:tenant/maven/<groupId-as-path>/<artifactId>/<version>/<filename>`
  (e.g. `com/example/foo/1.0.0/foo-1.0.0.jar`)
- **Parser complexity:** moderate. POM XML parsing has many optional
  fields; lean on Forgejo's parser verbatim.
- **Black-box client:** `maven:3.9-eclipse-temurin-21`. Commands:
  ```sh
  # ~/.m2/settings.xml configures a server with the token as <password>
  mvn deploy -DaltDeploymentRepository=pkgmirror::default::http://pkgmirror:8080/api/packages/<tenant>/maven
  mvn dependency:get -Dartifact=com.example:foo:1.0.0 -DremoteRepositories=pkgmirror::default::http://...
  ```

### `cargo`

Rust crates.

- **Spec:** [The Cargo Book — Registries](https://doc.rust-lang.org/cargo/reference/registries.html),
  [Registry Web API](https://doc.rust-lang.org/cargo/reference/registry-web-api.html)
- **Forgejo:**
  - `forgejo/routers/api/packages/cargo/cargo.go` — handlers
  - `forgejo/modules/packages/cargo/parser.go` — parses the `.crate`
    tarball metadata
- **Routes:** the cargo registry serves both a git-based "index"
  (config.json + sharded crate metadata files) and an HTTP API.
- **Parser complexity:** moderate. The biggest complication is the
  sharded index layout (`xx/yy/cratename`) Cargo expects.
- **Black-box client:** `rust:1.81-bookworm`. Commands:
  ```sh
  cargo publish --registry pkgmirror --token $TOKEN
  cargo add foo --registry pkgmirror
  cargo build
  ```

### `rubygems`

Ruby gems.

- **Spec:** [rubygems.org API](https://guides.rubygems.org/rubygems-org-api/)
- **Forgejo:**
  - `forgejo/routers/api/packages/rubygems/rubygems.go` — handlers
  - `forgejo/modules/packages/rubygems/metadata.go` — parses `.gem`
    archives (a TAR containing metadata.gz + data.tar.gz)
  - `forgejo/modules/packages/rubygems/marshal.go` — Ruby `Marshal`
    serialization; needed for the spec/quick-marshal endpoints
- **Routes:** legacy + modern endpoint set; see Forgejo for the full list.
- **Parser complexity:** **high** — Ruby's `Marshal` format must be
  implemented to satisfy `gem` and Bundler. Forgejo has a working
  implementation; copy it.
- **Black-box client:** `ruby:3.3-slim`. Commands:
  ```sh
  gem push --host http://x:$TOKEN@pkgmirror:8080/api/packages/<tenant>/rubygems foo-1.0.0.gem
  bundle config http://pkgmirror:8080/api/packages/<tenant>/rubygems x:$TOKEN
  bundle install
  ```

### `nuget`

.NET packages. Has two parallel protocols, V2 (OData) and V3 (JSON).

- **Spec:** [NuGet Server API v3](https://learn.microsoft.com/en-us/nuget/api/overview)
- **Forgejo:**
  - `forgejo/routers/api/packages/nuget/api_v2.go`, `api_v3.go` — two
    protocol versions, both needed for full client compatibility
  - `forgejo/routers/api/packages/nuget/links.go` — `@id` URL rewriting
  - `forgejo/routers/api/packages/nuget/auth.go` — NuGet's API key header
  - `forgejo/modules/packages/nuget/metadata.go` — parses `.nupkg` (a
    zip with a `.nuspec` XML file)
  - `forgejo/modules/packages/nuget/symbol_extractor.go` — debug symbols
- **Parser complexity:** moderate; the protocol surface is large because
  of V2 + V3.
- **Black-box client:** `mcr.microsoft.com/dotnet/sdk:8.0`. Commands:
  ```sh
  dotnet nuget add source http://pkgmirror:8080/api/packages/<tenant>/nuget/index.json -n pkgmirror -u x -p $TOKEN
  dotnet nuget push -s pkgmirror foo.1.0.0.nupkg
  dotnet add package foo --version 1.0.0
  ```

### `composer`

PHP packages.

- **Spec:** [Composer repository specification](https://getcomposer.org/doc/05-repositories.md#composer)
- **Forgejo:**
  - `forgejo/routers/api/packages/composer/composer.go`,
    `forgejo/routers/api/packages/composer/api.go`
  - `forgejo/modules/packages/composer/metadata.go` — parses
    `composer.json` from inside the zip
- **Parser complexity:** moderate; the registry response is a single
  JSON document at `/packages.json` plus per-package metadata files.
- **Black-box client:** `composer:2`. Commands:
  ```sh
  composer config repositories.pkgmirror composer http://pkgmirror:8080/api/packages/<tenant>/composer
  composer config http-basic.pkgmirror x $TOKEN
  composer require vendor/foo:^1.0
  ```

### `conan`

C / C++ packages.

- **Spec:** [Conan v2 server API](https://docs.conan.io/2/reference/conan_server.html)
- **Forgejo:**
  - `forgejo/routers/api/packages/conan/conan.go`, `auth.go`, `search.go`
  - `forgejo/modules/packages/conan/conanfile_parser.go` — parses
    `conanfile.py`/`conanfile.txt`
  - `forgejo/modules/packages/conan/conaninfo_parser.go` — parses build
    metadata
  - `forgejo/modules/packages/conan/reference.go` — Conan reference
    parsing (`name/version@user/channel`)
- **Parser complexity:** **high** — Conan has its own DSL for recipes
  and a non-trivial reference syntax. Definitely transliterate Forgejo.
- **Black-box client:** `conanio/gcc11-ubuntu16.04` (official). Commands:
  ```sh
  conan remote add pkgmirror http://pkgmirror:8080/api/packages/<tenant>/conan
  conan user -p $TOKEN -r pkgmirror x
  conan upload foo/1.0.0@user/channel -r pkgmirror --all
  conan install foo/1.0.0@user/channel -r pkgmirror
  ```

### `helm`

Kubernetes chart packages.

- **Spec:** [Helm Chart Repository Guide](https://helm.sh/docs/topics/chart_repository/)
- **Forgejo:**
  - `forgejo/routers/api/packages/helm/helm.go`
  - `forgejo/modules/packages/helm/metadata.go` — parses
    `Chart.yaml` from inside the `.tgz`
- **Parser complexity:** low — YAML inside a gzipped tar.
- **Black-box client:** `alpine/helm:3.16` or `helm:3.16`. Commands:
  ```sh
  helm repo add pkgmirror http://pkgmirror:8080/api/packages/<tenant>/helm --username x --password $TOKEN
  curl -u x:$TOKEN --data-binary @foo-1.0.0.tgz http://pkgmirror:8080/api/packages/<tenant>/helm/api/charts
  helm pull pkgmirror/foo --version 1.0.0
  ```

### Container (OCI)

OCI container images. **The most complex format** because of the token-
exchange auth dance and the multi-step manifest/blob upload protocol.

- **Spec:** [OCI Distribution Spec v1.1](https://github.com/opencontainers/distribution-spec/blob/main/spec.md)
- **Forgejo:**
  - `forgejo/routers/api/packages/container/container.go` — the bulk of
    the protocol (`/v2/`, manifest GET/PUT, blob upload via chunked
    `POST` → `PATCH` → `PUT`)
  - `forgejo/routers/api/packages/container/auth.go` — Bearer token
    realm + per-repo scoped JWT issuance
  - `forgejo/routers/api/packages/container/container_test.go` — extensive
    behavioral coverage
  - `forgejo/modules/packages/container/metadata.go` — manifest/config
    parsing, OCI vs Docker media-type handling
  - `forgejo/modules/packages/container/helm/` — sub-package for OCI-as-helm
- **Routes:** Many. Start by reading Forgejo's handler top-to-bottom.
- **Parser complexity:** **very high.** Don't reinvent.
- **Format-specific auth:** the OCI client (`docker`, `podman`, `crane`,
  `skopeo`, `oras`) speaks a token-exchange protocol:
  1. Client hits `/v2/`, gets `401` + `WWW-Authenticate: Bearer realm=…,service=…`.
  2. Client calls the realm with Basic-auth credentials, requesting a
     scope (e.g. `repository:foo:pull,push`).
  3. Server returns a short-lived JWT (or opaque bearer).
  4. Client uses the bearer for subsequent requests, scoped to that
     repo and action.
  We'll need a thin `container/auth.go` that issues these scoped tokens
  off the user's existing PAT identity. Forgejo's `auth.go` is the right
  reference.
- **Black-box client:** `docker:27` (with `dind`) or `gcr.io/go-containerregistry/crane:latest`
  for a simpler client. Commands:
  ```sh
  echo $TOKEN | crane auth login pkgmirror:8080 -u x --password-stdin
  crane copy alpine:3.20 pkgmirror:8080/<tenant>/alpine:3.20
  crane manifest pkgmirror:8080/<tenant>/alpine:3.20
  ```

### Debian (`apt`)

Debian / Ubuntu packages.

- **Spec:** [Debian Repository Format](https://wiki.debian.org/DebianRepository/Format)
- **Forgejo:**
  - `forgejo/routers/api/packages/debian/debian.go`
  - `forgejo/modules/packages/debian/metadata.go` — parses `.deb`
    (which is an `ar` archive containing `debian-binary`, `control.tar`,
    `data.tar`)
- **Routes:** complex — `Packages`, `Packages.gz`, `Release`, `InRelease`
  index generation across `dists/<dist>/<component>/binary-<arch>/`.
  Forgejo generates these on the fly.
- **Parser complexity:** moderate to high. The on-disk index format is
  rigid; signatures (`Release.gpg`, `InRelease`) are required by `apt`
  for trusted use.
- **Black-box client:** `debian:bookworm-slim`. Commands:
  ```sh
  echo "machine pkgmirror login x password $TOKEN" > /etc/apt/auth.conf.d/pkgmirror.conf
  echo "deb [trusted=yes] http://pkgmirror:8080/api/packages/<tenant>/debian bookworm main" > /etc/apt/sources.list.d/pkgmirror.list
  apt-get update && apt-get install -y foo
  ```

### Alpine (`apk`)

Alpine Linux packages.

- **Forgejo:** `forgejo/routers/api/packages/alpine/alpine.go`,
  `forgejo/modules/packages/alpine/metadata.go`
- **Parser complexity:** moderate. APKINDEX format + signature handling.
- **Black-box client:** `alpine:3.20`. Commands:
  ```sh
  echo "http://pkgmirror:8080/api/packages/<tenant>/alpine/v3.20/main" >> /etc/apk/repositories
  apk add foo
  ```

### RPM (`yum` / `dnf`)

RHEL / Fedora / Rocky / Alma packages.

- **Forgejo:** `forgejo/routers/api/packages/rpm/rpm.go`,
  `forgejo/modules/packages/rpm/metadata.go`
- **Parser complexity:** moderate to high. RPM has a complex binary
  metadata block.
- **Black-box client:** `fedora:41` or `rockylinux:9`. Commands:
  ```sh
  dnf config-manager --add-repo http://pkgmirror:8080/api/packages/<tenant>/rpm/<dist>/<arch>.repo
  dnf install -y foo
  ```

### Pub (Dart / Flutter)

- **Forgejo:** `forgejo/routers/api/packages/pub/pub.go`,
  `forgejo/modules/packages/pub/metadata.go`
- **Spec:** [Pub repository specification](https://github.com/dart-lang/pub/blob/master/doc/repository-spec-v2.md)
- **Black-box client:** `dart:3.5`.

### Swift

- **Forgejo:** `forgejo/routers/api/packages/swift/swift.go`,
  `forgejo/modules/packages/swift/metadata.go`
- **Spec:** [Swift Package Registry](https://github.com/apple/swift-package-manager/blob/main/Documentation/PackageRegistry/Registry.md)
- **Black-box client:** `swift:5.10`.

### CRAN (R)

- **Forgejo:** `forgejo/routers/api/packages/cran/cran.go`,
  `forgejo/modules/packages/cran/metadata.go`
- **Spec:** CRAN is conventional, not formally documented; Forgejo's
  parser is the reference.
- **Black-box client:** `r-base:4.4`.

### Conda

- **Forgejo:** `forgejo/routers/api/packages/conda/conda.go`,
  `forgejo/modules/packages/conda/metadata.go`
- **Black-box client:** `continuumio/miniconda3:latest`.

### Vagrant

- **Forgejo:** `forgejo/routers/api/packages/vagrant/vagrant.go`,
  `forgejo/modules/packages/vagrant/metadata.go`
- **Black-box client:** `hashicorp/vagrant:latest` (note: needs a hypervisor
  to actually run boxes; for protocol testing we can stop at the catalog
  JSON without spinning up VMs).

### Chef

- **Forgejo:** `forgejo/routers/api/packages/chef/chef.go`,
  `forgejo/routers/api/packages/chef/auth.go`,
  `forgejo/modules/packages/chef/metadata.go`
- **Notes:** Chef's auth (`Mixlib::Authentication`) is unusual — RSA
  signed request headers. Forgejo's `auth.go` is essential.
- **Black-box client:** `chef/chef:18` (the bundled Knife client).

### ALT, Arch

- **ALT:** `forgejo/routers/api/packages/alt/alt.go` (no separate
  parser package).
- **Arch:** `forgejo/routers/api/packages/arch/arch.go`,
  `forgejo/modules/packages/arch/metadata.go`
- Niche but covered by Forgejo. Recipes mirror the RPM/Debian patterns.

---

## Quick-reference matrix

| Format | Difficulty | Forgejo parser path | Black-box image |
| --- | --- | --- | --- |
| generic ✓ done | trivial | — | `curlimages/curl:8.10.1` |
| go ✓ done | easy | `modules/packages/goproxy/` | `golang:1.22-bookworm` |
| helm | low | `modules/packages/helm/` | `alpine/helm:3.16` |
| pub | low | `modules/packages/pub/` | `dart:3.5` |
| swift | low | `modules/packages/swift/` | `swift:5.10` |
| npm ✓ done | moderate | `modules/packages/npm/` | `node:22-bookworm` |
| pypi ✓ done | moderate | `modules/packages/pypi/` | `python:3.12-slim` |
| maven ✓ done | moderate | `modules/packages/maven/` | `maven:3.9-eclipse-temurin-21` |
| composer | moderate | `modules/packages/composer/` | `composer:2` |
| cargo | moderate | `modules/packages/cargo/` | `rust:1.81-bookworm` |
| nuget | moderate | `modules/packages/nuget/` | `mcr.microsoft.com/dotnet/sdk:8.0` |
| cran | moderate | `modules/packages/cran/` | `r-base:4.4` |
| conda | moderate | `modules/packages/conda/` | `continuumio/miniconda3` |
| vagrant | moderate | `modules/packages/vagrant/` | `hashicorp/vagrant:latest` |
| alpine ✓ done | moderate-high | `modules/packages/alpine/` | `alpine:3.20` |
| debian | moderate-high | `modules/packages/debian/` | `debian:bookworm-slim` |
| rpm | high | `modules/packages/rpm/` | `fedora:41` |
| arch | moderate-high | `modules/packages/arch/` | `archlinux:base` |
| alt | high | _(handler only)_ | (limited image availability) |
| rubygems ✓ done | high (Marshal) | `modules/packages/rubygems/` | `ruby:3.3-slim` |
| conan | high | `modules/packages/conan/` | `conanio/gcc11-ubuntu16.04` |
| chef | high (auth) | `modules/packages/chef/` | `chef/chef:18` |
| container ✓ done | very high | `modules/packages/container/` | `gcr.io/go-containerregistry/crane` |

---

## See also

- [auth.md](auth.md) — per-format credential mechanics; required reading
  before implementing any format with a non-trivial auth shim
  (`container`, `chef`).
- [blackbox-testing.md](blackbox-testing.md) — the harness contract,
  including the copy-paste template for a new format's
  `conformance_test.go`.
- The MIT LICENSE at the repo root, and the SPDX headers on existing
  derived files — model your attribution on those.
