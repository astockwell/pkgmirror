# pkgmirror

A standalone, multi-format package mirror service. Inspired by (and learning from) the
Gitea / Forgejo built-in package registry, but rewritten as a focused, lightweight
HTTP service with no dependency on the Forgejo codebase.

## Status

Early scaffolding. Initial format implemented: **Go module proxy** (per
[`GOPROXY` protocol](https://go.dev/ref/mod#goproxy-protocol)).

Roadmap (from `pkgmirror-spec.md`):

- [x] Go module proxy (`go`)
- [ ] Generic
- [ ] npm
- [ ] PyPI
- [ ] Maven
- [ ] Container (OCI)
- [ ] Cargo, Composer, Conan, Conda, Helm, NuGet, Pub, RubyGems, Swift, RPM, Debian, Alpine, ALT, Arch, CRAN, Vagrant, Chef

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

## Project layout

```
cmd/pkgmirror/        CLI entry point
internal/config/      env-driven config
internal/db/          SQLite open + migrations
internal/models/      DB models + queries
internal/storage/     content-addressed blob storage on the filesystem
internal/packages/    format-agnostic service layer (create package + file)
  goproxy/            Go module proxy parser + HTTP handlers
internal/server/      Gin router + middleware
internal/ui/          Bootstrap-based HTML UI
templates/            html/template files
```

See `DECISIONS.md` for the running log of architectural decisions and assumptions.
