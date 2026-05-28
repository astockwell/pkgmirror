# pkgmirror

A standalone, multi-format package mirror service. Inspired by (and learning from) the
Gitea / Forgejo built-in package registry, but rewritten as a focused, lightweight
HTTP service with no dependency on the Forgejo codebase.

## Status

Early scaffolding. Initial format implemented: **Go module proxy** (per
[`GOPROXY` protocol](https://go.dev/ref/mod#goproxy-protocol)).

Roadmap (from `pkgmirror-spec.md`):

- [x] Go module proxy (`go`)
- [x] PyPI (`pypi`) — wheel + sdist upload, PEP 503 simple index, PEP 691 JSON
- [x] npm (`npm`) — publish, packument, tarball download, dist-tags, scoped packages
- [x] RubyGems (`rubygems`) — `gem push` / `gem install`, compact index, legacy specs.4.8.gz, yank
- [ ] Generic
- [ ] Maven
- [ ] Container (OCI)
- [ ] Cargo, Composer, Conan, Conda, Helm, NuGet, Pub, Swift, RPM, Debian, Alpine, ALT, Arch, CRAN, Vagrant, Chef

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
| `PKGMIRROR_DEFAULT_TENANT` | `default` | name of the tenant auto-created on first boot |
| `PKGMIRROR_DEFAULT_TENANT_VISIBILITY` | `private` | `private` or `public` (controls anonymous reads) |
| `PKGMIRROR_ADMIN_TOKEN` | _(generated)_ | install this as the admin token; if unset, one is minted and printed once on first boot |
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

## Project layout

```
cmd/pkgmirror/        CLI entry point
internal/config/      env-driven config
internal/db/          SQLite open + migrations
internal/models/      DB models + queries
internal/storage/     content-addressed blob storage on the filesystem
internal/packages/    format-agnostic service layer (create package + file)
  goproxy/            Go module proxy parser + HTTP handlers
  pypi/               PyPI parser + HTTP handlers
  npm/                npm parser + HTTP handlers
  rubygems/           RubyGems parser + Ruby Marshal encoder + HTTP handlers
internal/server/      Gin router + middleware
internal/ui/          Bootstrap-based HTML UI
templates/            html/template files
```

See `DECISIONS.md` for the running log of architectural decisions and assumptions.
