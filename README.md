# pkgmirror

A standalone, multi-format **package mirror with supply-chain controls
built in**. Inspired by (and learning from) the Gitea / Forgejo
built-in package registry, but rewritten as a focused, lightweight
HTTP service with no dependency on the Forgejo codebase.

pkgmirror's reason for existing is to be the **single chokepoint**
between your developers / CI and the public package ecosystems. Every
artifact that flows through it is cataloged, optionally inspected
against policy, and recorded in an audit log — so when an upstream
release gets compromised, a malicious typosquat lands in a registry,
or a regulator asks "who pulled what, when, and under what license?",
there's one place to answer from.

## Status

Ten formats shipped, plus a working policy + audit engine. Active
development; APIs surface-area-stable but no LTS guarantees yet.

### Package formats

- [x] Go module proxy (`go`) — `GOPROXY` v1 protocol
- [x] PyPI (`pypi`) — wheel + sdist upload, PEP 503 simple index, PEP 691 JSON
- [x] npm (`npm`) — publish, packument, tarball download, dist-tags, scoped packages
- [x] RubyGems (`rubygems`) — `gem push` / `gem install`, compact index, legacy specs.4.8.gz, yank
- [x] Container / OCI (`container`) — OCI distribution v1.1: manifests, blobs (monolithic + chunked), tags, token-exchange auth dance
- [x] Generic (`generic`) — PUT/GET/DELETE arbitrary blobs at `<name>/<version>/<filename>`
- [x] Alpine (`alpine`) — `apk add` / `apk update`, signed APKINDEX.tar.gz, per-tenant RSA key
- [x] Maven (`maven`) — `mvn deploy` / `mvn dependency:get`, POM metadata extraction, generated maven-metadata.xml, SHA-1/MD5/SHA-256/SHA-512 sidecar verification
- [x] Debian (`debian`) — `apt update` / `apt-cache show`, on-demand Packages/Release indices, per-tenant OpenPGP signing (Release.gpg + InRelease)
- [x] RPM (`rpm`) — `dnf install` / `dnf info`, on-demand primary/filelists/other + repomd.xml indices, per-tenant OpenPGP-signed `repomd.xml.asc`
- [ ] Cargo, Composer, Conan, Conda, Helm, NuGet, Pub, Swift, ALT, Arch, CRAN, Vagrant, Chef

Every format goes through the same ingest / storage / serve pipeline, so
the supply-chain controls below apply uniformly across all of them.

### Supply-chain controls

Full details + operator runbook in [docs/supply-chain.md](docs/supply-chain.md).

| Control | What it does | Status |
| --- | --- | --- |
| **Cooldown** | Hide newly-published versions until they've been in the mirror for N days. Shrinks the blast radius of a compromised upstream release before anyone in your org installs it. | shipped |
| **License allowlist** | Refuse, quarantine, or warn when an artifact's SPDX license isn't on your allowed list. (PyPI today; per-format extractors land alongside each format.) | shipped |
| **Quarantine** | "Stored but hidden" status. Versions flagged by policy are kept on disk for forensics but disappear from index listings and 403 on read until an admin promotes or rejects them. | shipped |
| **Audit log** | Every ingest, every non-Allow decision, and (per-tenant opt-in) every read recorded with actor, request ID, decision, and reason. Queryable via `/admin/audit`. | shipped |
| **Multi-tenancy** | Each tenant is an isolation boundary for packages, tokens, and policy. Public / private visibility controls anonymous reads per tenant. | shipped |
| **Token auth** | Per-user `pkm_*` tokens, stored as SHA-256 hashes; per-format compatibility shims so real client CLIs work unmodified (`go`, `pip`, `npm`, `gem`, `docker login`, `apk`, etc.). | shipped |
| **Cascading policy rules** | Rules cascade by specificity (format / tenant / package / version-glob). Most-specific rule wins per evaluator; strictest decision wins across evaluators. Manageable via YAML or `/admin/rules` HTTP API. | shipped |
| Per-version blocklist | Deny by exact `(name, version)`. | architecture supports it — implementation pending |
| Sigstore / PEP 740 attestation verification | Cryptographic provenance gate. | planned |
| OSV vulnerability gate | Block on known CVE at ingest or read. | planned |
| Typosquat detection, velocity caps, post-install scanning | Heuristic and reactive controls. | planned |

## Quickstart

```sh
# from the pkgmirror/ directory
go run ./cmd/pkgmirror
# -> listens on :8080 by default
```

Environment variables:

| Var | Default | Description |
| --- | --- | --- |
| `PKGMIRROR_ADDR` | `:8080` | listen address |
| `PKGMIRROR_DATA_DIR` | `./data` | root dir for SQLite db + blob storage |
| `PKGMIRROR_DB_PATH` | `$DATA_DIR/pkgmirror.db` | SQLite db file path |
| `PKGMIRROR_BLOB_DIR` | `$DATA_DIR/blobs` | filesystem blob storage root |
| `PKGMIRROR_TMP_DIR` | `$DATA_DIR/tmp` | staging dir for in-flight upload buffers + OCI blob uploads. Defaults under `DATA_DIR` so it shares a filesystem with `BLOB_DIR` and the move-to-blob is a cheap rename. Set explicitly to put staging on a different volume. |
| `PKGMIRROR_DEFAULT_TENANT` | `default` | name of the tenant auto-created on first boot |
| `PKGMIRROR_DEFAULT_TENANT_VISIBILITY` | `private` | `private` or `public` (controls anonymous reads) |
| `PKGMIRROR_ADMIN_TOKEN` | _(generated)_ | install this as the admin token; if unset, one is minted and printed once on first boot |
| `PKGMIRROR_TLS_CERT` | _(unset)_ | path to a PEM-encoded TLS cert chain (leaf + intermediates). Both this and `PKGMIRROR_TLS_KEY` must be set to switch the listener to HTTPS. |
| `PKGMIRROR_TLS_KEY` | _(unset)_ | path to the matching PEM-encoded private key. |
| `PKGMIRROR_LOG_LEVEL` | `info` | (reserved) |

## Using the Go module proxy

Upload a Go module zip (per the [Go module zip format](https://go.dev/ref/mod#zip-files)):

```sh
curl -X PUT -u user:$PKGMIRROR_ADMIN_TOKEN \
  --data-binary @example.com_foo_v1.0.0.zip \
  http://localhost:8080/api/packages/default/go/upload
```

Configure `go` to use it. **Note:** the `go` toolchain refuses to send
Basic-auth credentials over plain HTTP; for private tenants, terminate TLS
in front of pkgmirror. For public tenants, no client-side credentials are
needed:

```sh
export GOPROXY=http://localhost:8080/api/packages/default/go,direct
go get example.com/foo@v1.0.0
```

See [`docs/auth.md`](docs/auth.md) for the full auth model and per-format
credential mechanics.

Browse the UI at <http://localhost:8080/>.

## Testing

```sh
make test            # fast unit / grey-box tests (no docker)
make test-blackbox   # full conformance suite (needs docker)
```

The black-box suite runs each ecosystem's real client (e.g. `go`,
eventually `npm`, `pip`, …) inside official docker images against a
containerized `pkgmirror`. See
[`docs/blackbox-testing.md`](docs/blackbox-testing.md) for the design and
"how to add a new format" guide.

## Using the PyPI registry

Upload a wheel or sdist (the "legacy" multipart form API that `twine` speaks):

```sh
curl -X POST -u user:$PKGMIRROR_ADMIN_TOKEN \
  -F ":action=file_upload" -F protocol_version=1 \
  -F name=foo -F version=1.0.0 -F filetype=bdist_wheel -F pyversion=py3 \
  -F metadata_version=2.1 \
  -F content=@foo-1.0.0-py3-none-any.whl \
  http://localhost:8080/api/packages/default/pypi/
```

Install with `pip`:

```sh
pip install \
  --index-url http://user:$PKGMIRROR_ADMIN_TOKEN@localhost:8080/api/packages/default/pypi/simple/ \
  --trusted-host localhost \
  foo
```

`pip` accepts in-URL Basic-auth credentials and `.netrc` over plain HTTP
— unlike `go`, which refuses anything but HTTPS.

## Using the npm registry

Point `npm` at the mirror with a per-tenant `.npmrc`. The trailing slash on
the registry URL is required by `npm`:

```sh
cat > ~/.npmrc <<EOF
registry=http://localhost:8080/api/packages/default/npm/
//localhost:8080/api/packages/default/npm/:_authToken=$PKGMIRROR_ADMIN_TOKEN
EOF
```

Publish a package the normal way — `npm publish` sends a single JSON
document with a base64-encoded tarball, and the mirror verifies the SRI
`integrity` field before storing:

```sh
cd my-package
npm publish
```

Install it from a consumer project:

```sh
npm install my-package
npm install @acme/widget        # scoped packages route via /@scope/name
```

Dist-tags use the standard `npm` CLI:

```sh
npm dist-tag add my-package@1.2.3 beta
npm dist-tag ls my-package
npm dist-tag rm my-package beta
```

Unlike `go`, `npm` is happy with token auth over plain HTTP via
`_authToken`, so no TLS termination is required for development. The
auth gate is identical to the other formats — see
[`docs/auth.md`](docs/auth.md).

## Using the RubyGems registry

Drop a credentials file at `~/.gem/credentials` (the `gem` CLI requires
mode `0600`):

```sh
mkdir -p ~/.gem && chmod 700 ~/.gem
cat > ~/.gem/credentials <<EOF
---
:rubygems_api_key: $PKGMIRROR_ADMIN_TOKEN
http://localhost:8080/api/packages/default/rubygems: $PKGMIRROR_ADMIN_TOKEN
EOF
chmod 600 ~/.gem/credentials
```

Publish a gem with the standard `gem push` command. `--host` must exactly
match the key in `credentials`:

```sh
gem build mygem.gemspec
gem push --host http://localhost:8080/api/packages/default/rubygems mygem-1.0.0.gem
```

Install a gem by pointing `--source` at the mirror:

```sh
gem install mygem --source http://localhost:8080/api/packages/default/rubygems/
```

Bundler config:

```sh
bundle config http://localhost:8080/api/packages/default/rubygems/ \
    pkgmirror:$PKGMIRROR_ADMIN_TOKEN
```

Yank a published version with the upstream API:

```sh
gem yank mygem -v 1.0.0 \
    --host http://localhost:8080/api/packages/default/rubygems
```

The mirror serves both the modern compact index
(`/info/<gem>`, `/versions`) used by Bundler 2.x and `gem install`, and
the legacy Marshal-encoded `/specs.4.8.gz` / `/quick/Marshal.4.8/*.gemspec.rz`
used by older clients. `gem push` sends the raw token as the
`Authorization` header value with no scheme prefix; the auth middleware
accepts that form alongside `Bearer` and `Basic` because pkgmirror tokens
carry an unambiguous `pkm_` prefix.

## Using the container (OCI) registry

Unlike every other format, OCI clients (`docker`, `podman`, `crane`,
`skopeo`, `oras`) require the registry endpoint to live at the host
root (`/v2/...`), not under `/api/packages/`. pkgmirror mounts the OCI
endpoints at `/v2/:tenant/:image/...` so the image reference shape is
`<host>:<port>/<tenant>/<image>:<tag>`.

Log in with `docker login` (or `crane auth login`); the username is
ignored, the password is your pkgmirror token:

```sh
echo "$PKGMIRROR_ADMIN_TOKEN" | docker login localhost:8080 -u any --password-stdin
```

Push an image:

```sh
docker tag alpine:3.20 localhost:8080/default/alpine:3.20
docker push localhost:8080/default/alpine:3.20
```

Or with `crane`:

```sh
crane copy alpine:3.20 localhost:8080/default/alpine:3.20
crane manifest localhost:8080/default/alpine:3.20
crane ls localhost:8080/default/alpine
```

OCI clients speak a token-exchange dance: hit `/v2/`, follow the
`WWW-Authenticate: Bearer realm=...` header to `/v2/token`, exchange
Basic credentials for a bearer token, then use that bearer for the rest
of the session. pkgmirror's token endpoint round-trips the password
from Basic back as the bearer value, so the same `pkm_` token works
throughout. Anonymous reads on public tenants get a placeholder bearer
(`anonymous`) that the auth middleware silently treats as no-identity.

Image names may contain interior slashes (`docker push
localhost:8080/default/myorg/team/svc:v1`). Internally the routes are
registered as catch-alls per HTTP method and dispatched via regex, the
same approach Forgejo takes; the dispatcher greedily matches the
image name up to the trailing `/manifests/`, `/blobs/`, or
`/tags/list` delimiter.

Not yet implemented: cross-repo blob mount via `?mount=&from=` (falls
through to a normal upload session, which is spec-allowed); the
`/v2/_catalog` endpoint.

## Using the generic registry

The generic format is a pass-through: arbitrary blobs are stored by
`(tenant, name, version, filename)` with no ecosystem-specific
metadata. Useful for binary artifacts that don't fit any other format
— build outputs, release bundles, signed installers, etc.

```sh
# Upload
curl -fsS -X PUT \
    -H "Authorization: Bearer $PKGMIRROR_ADMIN_TOKEN" \
    --data-binary @./build/installer.bin \
    http://localhost:8080/api/packages/default/generic/installer/1.0.0/installer.bin

# Download
curl -fsS -o /tmp/installer.bin \
    -H "Authorization: Bearer $PKGMIRROR_ADMIN_TOKEN" \
    http://localhost:8080/api/packages/default/generic/installer/1.0.0/installer.bin

# Delete one file
curl -fsS -X DELETE \
    -H "Authorization: Bearer $PKGMIRROR_ADMIN_TOKEN" \
    http://localhost:8080/api/packages/default/generic/installer/1.0.0/installer.bin

# Delete a whole version (and every file in it)
curl -fsS -X DELETE \
    -H "Authorization: Bearer $PKGMIRROR_ADMIN_TOKEN" \
    http://localhost:8080/api/packages/default/generic/installer/1.0.0
```

Duplicate filenames within the same version return 409. Many files
are allowed per version (think `linux-amd64.tar.gz` +
`linux-arm64.tar.gz` + `darwin-arm64.tar.gz` under the same
`v1.0.0`). Deleting the last file in a version automatically removes
the version row too.

## Using the Alpine (apk) registry

Alpine repos live at `<branch>/<repository>/<architecture>/`, mirroring
the layout `apk` expects in `/etc/apk/repositories`. pkgmirror generates
a per-tenant RSA signing keypair on first request and signs every
APKINDEX.tar.gz with it, so plain `apk update` (no `--allow-untrusted`)
works once the public key is installed:

```sh
# Install the tenant's public key into /etc/apk/keys/
curl -fsS -H "Authorization: Bearer $PKGMIRROR_ADMIN_TOKEN" \
    -o /etc/apk/keys/pkgmirror.rsa.pub \
    http://localhost:8080/api/packages/default/alpine/key

# Add the repo
echo "http://localhost:8080/api/packages/default/alpine/v3.20/main" \
    >> /etc/apk/repositories

# Now the standard workflow works
apk update
apk search mypackage
apk add mypackage
```

Publish a `.apk` with `PUT`. The branch (e.g. `v3.20`) and repository
(e.g. `main`, `community`) come from the path; the architecture is
parsed out of the `.apk`'s `.PKGINFO`:

```sh
curl -fsS -X PUT \
    -H "Authorization: Bearer $PKGMIRROR_ADMIN_TOKEN" \
    --data-binary @./mypackage-1.0.0.apk \
    http://localhost:8080/api/packages/default/alpine/v3.20/main
```

A `.apk` whose `PKGINFO` advertises `arch = noarch` is fanned out across
every architecture the repository already has, falling back to `x86_64`
if the repo is empty — same semantics as forgejo and a stock alpine
mirror. Delete a single file:

```sh
curl -fsS -X DELETE \
    -H "Authorization: Bearer $PKGMIRROR_ADMIN_TOKEN" \
    http://localhost:8080/api/packages/default/alpine/v3.20/main/x86_64/mypackage-1.0.0.apk
```

Each tenant's signing key is generated lazily and stored as a property
on a synthetic `_alpine` package row. Rotating keys is currently a
manual SQL operation; a `/key/rotate` endpoint is on the roadmap once
we settle on a key-rollover UX that doesn't break already-installed
clients.

## Using the Maven registry

Maven and Gradle clients treat the registry as "static files at
predictable URLs" (the Maven 2 repository layout). pkgmirror exposes
the per-tenant repo at `/api/packages/:tenant/maven/` and follows the
GAV path convention: `<groupId-with-slashes>/<artifactId>/<version>/<filename>`.

Configure a `~/.m2/settings.xml` with credentials. If pkgmirror is
serving HTTPS (either built-in TLS via `PKGMIRROR_TLS_CERT` /
`PKGMIRROR_TLS_KEY` or behind a reverse proxy that terminates TLS),
this is the only block you need:

```xml
<settings xmlns="http://maven.apache.org/SETTINGS/1.0.0">
  <servers>
    <server>
      <id>pkgmirror</id>
      <username>x</username>
      <password>${env.PKGMIRROR_ADMIN_TOKEN}</password>
    </server>
  </servers>
</settings>
```

If you're running pkgmirror over plain HTTP (development, internal
network), you also need to override Maven 3.8.1+'s built-in
`maven-default-http-blocker` mirror, which routes every HTTP repo
through `http://0.0.0.0/` to enforce HTTPS-by-default:

```xml
  <mirrors>
    <mirror>
      <id>maven-default-http-blocker</id>
      <mirrorOf>dummy</mirrorOf>
      <url>http://0.0.0.0/</url>
      <blocked>false</blocked>
    </mirror>
  </mirrors>
```

The principled fix is to terminate TLS — see [TLS](#tls) below.

Publish a jar with `mvn deploy` or the lower-level `deploy-file` goal:

```sh
mvn deploy:deploy-file \
    -DrepositoryId=pkgmirror \
    -Durl=http://localhost:8080/api/packages/default/maven \
    -DgroupId=com.example \
    -DartifactId=foo \
    -Dversion=1.0.0 \
    -Dpackaging=jar \
    -Dfile=target/foo-1.0.0.jar \
    -DpomFile=pom.xml
```

Resolve from a Maven project by adding a `<repository>` block to your
`pom.xml`:

```xml
<repositories>
  <repository>
    <id>pkgmirror</id>
    <url>http://localhost:8080/api/packages/default/maven</url>
  </repository>
</repositories>
```

The registry generates `maven-metadata.xml` on demand from the live
version list, so it always reflects the current state — no
build-on-upload coordination. SHA-1, MD5, SHA-256, and SHA-512 sidecar
files are synthesized from the stored blob hashes; checksum PUT
requests are verified against the same hashes (mismatch → 400). The
canonical license string is extracted from each artifact's POM and
flows through the same supply-chain policy engine as every other
format (cooldown, allowlist, quarantine).

## Using the Debian (apt) registry

pkgmirror serves a standard Debian binary repository at
`/api/packages/:tenant/debian/`. The layout matches what `apt`
expects out of the box: `pool/<dist>/<component>/<file>.deb` for
artifacts, `dists/<dist>/...` for the signed index family. On first
request pkgmirror lazily generates a per-tenant OpenPGP signing
keypair; subsequent `apt update` calls sign the
`InRelease` / `Release.gpg` files with the same key.

Install the public key into apt's trust store:

```sh
mkdir -p /etc/apt/keyrings
curl -fsS -u x:$PKGMIRROR_ADMIN_TOKEN \
    http://localhost:8080/api/packages/default/debian/key.gpg \
    | gpg --dearmor -o /etc/apt/keyrings/pkgmirror.gpg
```

Configure the sources file. Note the `Signed-By:` line — apt 2.x
deprecates the global trust store, so each repo carries the
keyring path that signs it:

```sh
cat > /etc/apt/sources.list.d/pkgmirror.sources <<EOF
Types: deb
URIs: http://localhost:8080/api/packages/default/debian
Suites: bookworm
Components: main
Signed-By: /etc/apt/keyrings/pkgmirror.gpg
EOF
```

Credentials for non-public tenants go in `/etc/apt/auth.conf.d/`.
**The `URIs:` value MUST include the protocol (`http://`)** — apt
2.x rejects unprotected `machine` lines as a security precaution:

```sh
cat > /etc/apt/auth.conf.d/pkgmirror.conf <<EOF
machine http://localhost:8080/api/packages/default/debian
login x
password $PKGMIRROR_ADMIN_TOKEN
EOF
chmod 600 /etc/apt/auth.conf.d/pkgmirror.conf
```

Now the standard workflow works:

```sh
apt-get update
apt-cache show mypackage
apt-get install -y mypackage
```

Publish a `.deb`. The distribution (e.g. `bookworm`, `bullseye`) and
component (e.g. `main`, `contrib`) come from the path; the
architecture is parsed out of the `.deb`'s `control` file:

```sh
curl -fsS -X PUT \
    -u x:$PKGMIRROR_ADMIN_TOKEN \
    --data-binary @./mypackage_1.0.0_amd64.deb \
    http://localhost:8080/api/packages/default/debian/pool/bookworm/main/upload
```

Delete a published `.deb`:

```sh
curl -fsS -X DELETE \
    -u x:$PKGMIRROR_ADMIN_TOKEN \
    http://localhost:8080/api/packages/default/debian/pool/bookworm/main/mypackage/1.0.0/amd64
```

The `Packages` / `Packages.gz` / `Packages.xz` indices and the
`Release` / `Release.gpg` / `InRelease` triple are all generated on
demand from the live package list — no build-on-upload coordination.
The `Date:` field in `Release` is derived from the newest file in the
distribution so the detached signature in `Release.gpg` matches the
plain `Release` body byte-for-byte across separate requests.

## Using the RPM registry

The RPM endpoints are rooted at
`/api/packages/:tenant/rpm/:group/`. `:group` is a free-form label
that groups packages into independent repositories (one per OS
release, channel, etc.) — commonly `el9`, `fedora41`, `stable`,
etc. Multi-segment groups are not currently supported.

Grab the per-tenant GPG public key and the dnf `.repo` file (the
`.repo` file detects http/https automatically based on how it was
fetched):

```sh
sudo curl -fsS \
    -o /etc/pki/rpm-gpg/RPM-GPG-KEY-pkgmirror \
    http://localhost:8080/api/packages/default/rpm/el9/repository.key
sudo curl -fsS \
    -o /etc/yum.repos.d/pkgmirror.repo \
    http://localhost:8080/api/packages/default/rpm/el9/repository.repo
sudo rpm --import /etc/pki/rpm-gpg/RPM-GPG-KEY-pkgmirror
sudo dnf makecache --repo=pkgmirror-default-el9
```

If the tenant is private, dnf needs credentials. The simplest
portable form is an authed `baseurl` written by hand:

```ini
[pkgmirror-default-el9]
name=pkgmirror default/el9
baseurl=http://x:$PKGMIRROR_ADMIN_TOKEN@localhost:8080/api/packages/default/rpm/el9
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-pkgmirror
```

Upload an `.rpm`. Name / version / release / architecture are parsed
out of the binary RPM header:

```sh
curl -fsS -X PUT \
    -u x:$PKGMIRROR_ADMIN_TOKEN \
    --data-binary @./mypackage-1.0.0-1.x86_64.rpm \
    http://localhost:8080/api/packages/default/rpm/el9/upload
```

Delete a published `.rpm`:

```sh
curl -fsS -X DELETE \
    -u x:$PKGMIRROR_ADMIN_TOKEN \
    http://localhost:8080/api/packages/default/rpm/el9/package/mypackage/1.0.0-1/x86_64
```

The `repodata/{repomd,primary,filelists,other}.xml*` files and the
detached `repodata/repomd.xml.asc` are all generated on demand from
the live package list. The `<timestamp>` in `repomd.xml` is derived
from the newest file in the group so the signature stays valid across
separate requests — same Stable-Bytes pattern as Debian. See
[docs/known-deviations-from-spec.md](docs/known-deviations-from-spec.md)
for the details.

## TLS

pkgmirror can terminate TLS itself. Set both
`PKGMIRROR_TLS_CERT` and `PKGMIRROR_TLS_KEY` to PEM-encoded files and
the listener switches from HTTP to HTTPS on the same `PKGMIRROR_ADDR`:

```sh
export PKGMIRROR_TLS_CERT=/etc/pkgmirror/tls/fullchain.pem
export PKGMIRROR_TLS_KEY=/etc/pkgmirror/tls/privkey.pem
export PKGMIRROR_ADDR=:8443
pkgmirror
# -> https://0.0.0.0:8443
```

`PKGMIRROR_TLS_CERT` is the full chain (leaf certificate concatenated
with any intermediates) — the same shape Let's Encrypt's
`fullchain.pem` and most managed cert services produce.
`PKGMIRROR_TLS_KEY` is the matching private key.

Setting only one of the two is a misconfiguration and the process
fails fast at boot. Hot cert reload is not currently supported; for
cert rotation, restart pkgmirror after the new files are in place
(systemd / Kubernetes rolling restart works fine).

pkgmirror does **not** ship ACME / Let's Encrypt integration. For
automatic certs, run pkgmirror behind a reverse proxy (nginx, Caddy,
Traefik) and leave `PKGMIRROR_TLS_*` empty — the proxy terminates TLS
and pkgmirror serves plain HTTP on the loopback interface. This is the
recommended deployment shape for production; built-in TLS is provided
for single-binary deployments and dev environments where running a
proxy is overkill.

## Project layout

```
cmd/pkgmirror/        CLI entry point
internal/config/      env-driven config
internal/db/          SQLite open + migrations
internal/models/      DB models + queries
internal/storage/     content-addressed blob storage on the filesystem
internal/auth/        token middleware + per-format compatibility shims
internal/tenants/     multi-tenant isolation + visibility model
internal/users/       user records + group membership
internal/tokens/      pkm_* token issuance + verification
internal/policy/      supply-chain policy engine
  cooldown/           cooldown evaluator (min-age-in-mirror)
  license/            license-allowlist evaluator (SPDX)
internal/audit/       buffered audit logger + query API
internal/admin/       /admin endpoints (rules, audit, quarantine)
internal/syncutil/    shared concurrency primitives (refcount-driven ExclusivePool)
internal/packages/    format-agnostic service layer (create package + file)
  goproxy/            Go module proxy parser + HTTP handlers
  pypi/               PyPI parser + HTTP handlers
  npm/                npm parser + HTTP handlers
  rubygems/           RubyGems parser + Ruby Marshal encoder + HTTP handlers
  container/          OCI manifest parser + blob upload tracker + /v2/ handlers
  generic/            Pass-through PUT/GET/DELETE handlers, no parser
  alpine/             .apk PKGINFO parser + APKINDEX.tar.gz builder + RSA signing
  maven/              pom.xml parser + maven-metadata.xml generator + checksum sidecars
  debian/             .deb parser + on-demand Packages/Release builder + OpenPGP signing
  rpm/                .rpm header parser + on-demand repomd/primary/filelists/other + OpenPGP signing
internal/server/      Gin router + middleware
internal/ui/          Bootstrap-based HTML UI
templates/            html/template files
tests/blackbox/       per-format docker-driven conformance suites
docs/                 operator + developer documentation
```

## Documentation

- [docs/supply-chain.md](docs/supply-chain.md) — policy engine operator runbook
  (cooldown, license allowlist, quarantine workflow, audit queries)
- [docs/auth.md](docs/auth.md) — token model, per-format credential mechanics,
  tenant visibility, anonymous-read rules
- [docs/storage.md](docs/storage.md) — content-addressed blob layout,
  the staging-tmp story, key invariants for filesystem operators
- [docs/blackbox-testing.md](docs/blackbox-testing.md) — the conformance
  harness contract and how to add a new format's blackbox
- [docs/adding-a-format.md](docs/adding-a-format.md) — the new-format playbook
  with per-ecosystem recipes
- [docs/known-deviations-from-spec.md](docs/known-deviations-from-spec.md) —
  where pkgmirror's wire behavior intentionally differs from canonical
  format specs (e.g. how the Debian `Release.Date` field is derived)
- [DECISIONS.md](DECISIONS.md) — running log of architectural decisions
  and assumptions
- [ATTRIBUTIONS.md](ATTRIBUTIONS.md) — file-by-file mapping of every
  source file adapted from an upstream project (mostly Forgejo / Gitea)
- [LICENSE](LICENSE) — MIT

## Acknowledgments

Most of pkgmirror's package-format support is adapted from the
[Forgejo](https://codeberg.org/forgejo/forgejo) project and its
ancestor [Gitea](https://github.com/go-gitea/gitea). The Forgejo
maintainers' care in keeping the per-file MIT attribution on the
original Gitea-origin files is what makes a project like this
possible without a license retrofit. Thank you.

See [ATTRIBUTIONS.md](ATTRIBUTIONS.md) for the full file-by-file
provenance.
