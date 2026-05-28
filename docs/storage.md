# Storage

This document describes how `pkgmirror` persists data: the split between
metadata and bytes, the interface that abstracts the bytes layer, the
single implementation that ships today, and the path to additional
backends (S3 / MinIO / GCS / Azure). It is the spec for the current
implementation and the design contract that future storage backends
must conform to.

The interface and the `LocalStorage` implementation are direct ports of
[`forgejo/modules/storage`](https://codeberg.org/forgejo/forgejo/src/branch/forgejo/modules/storage)
(MIT). The architectural decision and the rationale are recorded in
[DECISIONS.md](../DECISIONS.md) (2026-05-28); this doc is the
operator- and contributor-facing version of the same material.

---

## Two stores, separated by data shape

Persistence splits cleanly along a single line: **structured metadata**
lives in SQLite, **opaque byte streams** live in a content-addressed
blob store. Nothing else.

| What | Where | Why |
| --- | --- | --- |
| Packages, versions, files, properties, audit log, tenants, users, tokens, policy rules | SQLite (`pkgmirror.db`) | Small, transactional, indexed. ~1 KB per row. |
| Every artifact byte ever uploaded — gem files, npm tarballs, wheels, sdists, go module zips, OCI layers + manifests + configs | Blob store (`data/blobs/<aa>/<bb>/<full-sha256>`) | Large, immutable, content-addressed. MB–GB per object. |

The `package_files` table is the bridge: each row points at
`(version_id, blob_id, filename)` where `blob_id` references
`package_blobs` and `package_blobs.hash_sha256` is the key the blob
store uses on disk. **Multiple files can share the same blob, and
multiple tenants can share the same blob** — cross-format and
cross-tenant byte dedup is essentially free, because the key is the
content hash.

### Layout on disk

For a default `PKGMIRROR_DATA_DIR=./data` install:

```
data/
  pkgmirror.db                 SQLite — everything queryable
  blobs/
    a4/
      9f/
        a49fc4a9…d0e7         numpy wheel, 16 MiB
        a49fcb1f…228e         a random alpine layer, 3 MiB
      a0/
        …
    …
```

The two-level `<aa>/<bb>/` sharding ensures no single directory holds
more than ~65k entries even at multi-million-blob scale. The full hash
is repeated in the leaf filename so you can `find data/blobs -name
'<sha>'` from a shell when debugging.

### Configuration

| Env var | Default | Description |
| --- | --- | --- |
| `PKGMIRROR_DATA_DIR` | `./data` | Root for everything pkgmirror persists. |
| `PKGMIRROR_DB_PATH` | `$DATA_DIR/pkgmirror.db` | Override only the SQLite path. |
| `PKGMIRROR_BLOB_DIR` | `$DATA_DIR/blobs` | Override only the blob root. Set this to a separately-mounted volume if you want the DB on fast local disk and blobs on bulk storage. |

---

## The blob backend interface

[`internal/storage/storage.go`](../internal/storage/storage.go) defines
the `ObjectStorage` interface. It mirrors
[`forgejo/modules/storage.ObjectStorage`](https://codeberg.org/forgejo/forgejo/src/branch/forgejo/modules/storage/storage.go)
method-for-method:

```go
type Object interface {
    io.ReadCloser
    io.Seeker
    Stat() (os.FileInfo, error)
}

type ObjectStorage interface {
    Open(path string) (Object, error)
    Save(path string, r io.Reader, size int64) (int64, error)
    Stat(path string) (os.FileInfo, error)
    Delete(path string) error
    URL(path, name string, reqParams url.Values) (*url.URL, error)
    IterateObjects(prefix string, fn func(path string, obj Object) error) error
}
```

Plus two sentinels:

- `ErrNotFound` — returned by `Open` and `Stat` for missing keys.
- `ErrURLNotSupported` — returned by `URL()` on backends that don't
  issue redirects (e.g. `LocalStorage`).

### Semantics at a glance

| Method | Contract |
| --- | --- |
| `Open(path)` | Return an `Object` (`Read`, `Close`, `Seek`, `Stat`). `ErrNotFound` for misses. |
| `Save(path, r, size)` | Persist `r` at `path`. `size = -1` means "unknown, stream". Returns bytes written. Atomic from any reader's perspective. |
| `Stat(path)` | Return `os.FileInfo` without opening the body. `ErrNotFound` for misses. |
| `Delete(path)` | Remove the object. Missing objects are not an error (idempotent). |
| `URL(path, name, params)` | Return a pre-signed / redirectable URL, or `ErrURLNotSupported`. Handlers fall back to streaming when not supported. |
| `IterateObjects(prefix, fn)` | Walk every object under `prefix`. `fn`'s `Object` is owned by the iterator and closed automatically. Backends honor cancellation via the constructor's context. |

### Design properties that matter

- **No filenames in storage.** A blob is identified by its hash, never
  by `foo-1.0.0.tar.gz`. The filename lives in `package_files.name`
  and is just a label; the actual bytes are at
  `data/blobs/aa/bb/<hash>`.
- **Idempotent writes.** `Save()` on an existing key overwrites
  atomically. Combined with content addressing, this means tenant A
  and tenant B both uploading the same `numpy-1.26.0-cp312.whl` write
  the bytes once.
- **Atomic writes.** Implementations MUST guarantee that readers never
  observe a partial write. `LocalStorage` does this via temp file +
  `os.Rename`; S3-style backends get atomicity from `PutObject`.
- **Immutable.** There is no `Update`. A given key's bytes are forever
  those bytes; if they change, the key changes.

### Why the interface looks like this (vs. my first draft)

The first cut of this package was three methods — `Put` / `Open` /
`Delete` — and it was filesystem-shaped. The five gaps that surfaced
the moment we started thinking about a cloud backend:

1. `Put` didn't take `size`. S3 multipart wants known size up front.
2. There was no standalone `Stat`. Backends couldn't answer "how big"
   without opening the body and reading to EOF.
3. The returned reader was `io.ReadSeekCloser`, not an Object with
   `Stat()`. S3 `GetObject` doesn't naturally satisfy `Seek`; without
   `Stat()` you also can't set `Content-Length` from the open reader.
4. No `URL()` hook. Big-blob pulls (especially OCI layers) want 307
   pre-signed redirects so the body never streams through the
   registry; without it in the interface, we'd have to bolt it on
   later as a non-interface-compatible escape hatch.
5. No iterator. Orphan-blob GC is impossible without one.

Forgejo has been through every one of these. The current interface
adopts their shape verbatim so the next backend is a port job, not a
redesign.

---

## The `LocalStorage` implementation

`LocalStorage` is the only implementation that ships today. Filesystem-
backed, content-addressed, with on-disk sharding.

### What it does and doesn't do

- **Sharding:** internal to the implementation. The caller passes a
  logical key (always a sha256 hex digest, in practice);
  `buildLocalPath` maps it to `<root>/<aa>/<bb>/<full>`. Forgejo's
  `LocalStorage` doesn't shard because its caller-side keys are
  subsystem-specific (small ints, repo paths). Ours always shards
  because every key is a 64-char hex digest.
- **Atomicity:** `Save` writes to a `.tmp-*` file in the same directory
  as the destination, then `os.Rename`. Concurrent reads on the same
  key see either the old bytes or the new bytes, never a partial
  write.
- **Idempotency:** `Save` on an existing key is a normal write — the
  bytes are replaced atomically. Since keys are content hashes,
  "replaces" means "writes the same bytes" in practice; the operation
  is observably a no-op.
- **`Stat` and `Open` ENOENT:** wrapped as `ErrNotFound` so callers
  branch on a stable sentinel regardless of backend.
- **`URL()`:** returns `ErrURLNotSupported`. Handlers that try
  pre-signed redirects should detect this sentinel and fall through to
  streaming.
- **`IterateObjects`:** walks the on-disk tree under `prefix`, opens
  each regular file, hands it to the callback as an `Object`, and
  closes it automatically after the callback returns. The walk
  respects the constructor's context — a cancelled context aborts the
  walk mid-tree with `context.Canceled`. Skips dot-prefixed `.tmp-*`
  files left over from interrupted writes. Reverses the on-disk shard
  before invoking the callback so the path the callback sees is the
  logical key the caller originally passed to `Save`.

### Wiring

The backend is injected at exactly one point —
[`cmd/pkgmirror/main.go`](../cmd/pkgmirror/main.go):

```go
blobs, err := storage.NewLocalStorage(ctx, cfg.BlobDir)
…
svc := pkgsvc.NewService(pkgModels, blobs)
```

From there `pkgsvc.Service` carries it forward:

```go
type Service struct {
    Models  *models.Store
    Storage storage.ObjectStorage  // interface, not concrete type
}
```

Every format handler reaches the backend only through:

- `Service.CreatePackageAndAddFile` — the standard upload path
- `Service.CreatePackageOrAddFileToExisting` — the PyPI multi-file
  upload path
- `Service.OpenFile` — every format's download path

The only handler that calls `Storage` methods directly is the OCI
container handler, because blob uploads happen across multiple
requests and don't fit the standard `CreatePackage...` lifecycle:

| Handler | Method | Why direct |
| --- | --- | --- |
| `container.storeBytes` | `Storage.Save` | Manifest upload from an in-memory byte slice. |
| `container.storeStagedFile` | `Storage.Save` | Blob upload from the staged temp file after `UploadTracker.Finalize`. |
| `container.serveBlob` | `Storage.Open` | OCI `GET /v2/<name>/blobs/<digest>`. |
| `container.serveManifest` | `Storage.Open` | OCI `GET /v2/<name>/manifests/<reference>`. |

That's the entire surface area. Five wrapped call sites + four direct
call sites across the whole codebase.

---

## Adding a new backend

Implementing a backend is the same shape as implementing
[`forgejo/modules/storage/minio.go`](https://codeberg.org/forgejo/forgejo/src/branch/forgejo/modules/storage/minio.go).
Sketch for S3:

```go
// internal/storage/s3.go
type S3 struct {
    ctx    context.Context
    client *s3.Client
    bucket string
    prefix string  // e.g. "pkgmirror/blobs"
}

func NewS3(ctx context.Context, bucket, prefix string, …) (*S3, error) { … }

func (s *S3) keyFor(path string) string {
    // Optional: shard the same way LocalStorage does to ease cross-
    // backend migration. S3 doesn't need it but it makes
    // `aws s3 sync` between a local dump and a bucket trivial.
    if len(path) < 4 {
        return s.prefix + "/" + path
    }
    return s.prefix + "/" + path[0:2] + "/" + path[2:4] + "/" + path
}

func (s *S3) Open(path string) (Object, error) {
    out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
        Bucket: &s.bucket, Key: aws.String(s.keyFor(path)),
    })
    if isS3NotFound(err) {
        return nil, ErrNotFound
    }
    if err != nil { return nil, err }
    // Wrap out.Body in a type satisfying Object. Stat() returns a
    // synthetic FileInfo populated from out.ContentLength + LastModified.
    // Seek implementation streams a Ranged GetObject on each Seek(); see
    // Forgejo's minio.go for the exact shape.
    return &s3Object{...}, nil
}

func (s *S3) Save(path string, r io.Reader, size int64) (int64, error) {
    // size >= 0  -> single PutObject
    // size <  0  -> multipart upload
    …
}

func (s *S3) Stat(path string) (os.FileInfo, error) {
    out, err := s.client.HeadObject(…)
    if isS3NotFound(err) { return nil, ErrNotFound }
    …
}

func (s *S3) Delete(path string) error { … }

func (s *S3) URL(path, name string, reqParams url.Values) (*url.URL, error) {
    // Pre-signed GET URL with Content-Disposition=name.
    return s.presigner.PresignGetObject(…)
}

func (s *S3) IterateObjects(prefix string, fn func(string, Object) error) error {
    // ListObjectsV2 paginated, opening each via Open() inside fn,
    // honoring s.ctx for cancellation.
}
```

Then add a config switch in [`cmd/pkgmirror/main.go`](../cmd/pkgmirror/main.go):

```go
var blobs storage.ObjectStorage
switch cfg.StorageBackend {
case "local", "":
    blobs, err = storage.NewLocalStorage(ctx, cfg.BlobDir)
case "s3":
    blobs, err = storage.NewS3(ctx, cfg.S3Bucket, cfg.S3Prefix, …)
default:
    log.Fatalf("unknown storage backend %q", cfg.StorageBackend)
}
```

…and a few `PKGMIRROR_STORAGE_*` env vars in `internal/config/`.

**That's the whole change.** No handler touches storage directly except
the OCI container handler, and even there the interface is the
ObjectStorage methods — no format-handler code needs changes. No
model changes. No schema migration.

### Migration

The on-disk key scheme is the sha256 hex digest. A migration from
LocalStorage → S3 (or vice versa) is:

```sh
# LocalStorage -> S3
aws s3 sync data/blobs/ s3://my-bucket/pkgmirror/blobs/ \
    --exclude '.tmp-*'

# S3 -> LocalStorage
aws s3 sync s3://my-bucket/pkgmirror/blobs/ data/blobs/
```

You can also run `IterateObjects` on the source backend and `Save` to
the destination — that path works regardless of which two backends
are involved, and respects whatever sharding either side prefers.

### What every new backend must also do

| Requirement | Why |
| --- | --- |
| Atomic writes from any reader's perspective | Concurrent reads on the same key during a write must see old bytes or new bytes, never partial. |
| `Save` is idempotent for the same key + bytes | Content-addressed dedup relies on `Save("abc…", same-bytes)` being a no-op observably. |
| `Open` and `Stat` return `ErrNotFound` for misses | Handlers branch on this stable sentinel regardless of backend. |
| `Delete` is idempotent | Deleting a missing key must not error. |
| Respect the constructor's context for long-running ops | Especially `IterateObjects`. Shutdown propagation. |
| Return `ErrURLNotSupported` from `URL()` when redirects aren't available | Handlers fall through to streaming. Don't return a fake URL. |

---

## What's not done yet

### Orphan-blob GC

There is currently no garbage collection. A blob stays on disk forever,
even if every `package_file` referencing it is deleted. This is fine
on local disk where bytes are cheap; it'll matter more on S3 where
they're metered.

The shape of the fix is straightforward and made possible by
`IterateObjects`:

```go
// pseudo-code for a periodic GC pass
storage.IterateObjects("", func(key string, _ Object) error {
    if !modelsHasBlobWithHash(key) {
        return storage.Delete(key)
    }
    return nil
})
```

The DB query (`SELECT 1 FROM package_blobs WHERE hash_sha256 = ?`) is
indexed on `hash_sha256 UNIQUE` so the lookup is O(log n) per iterated
key.

### Pre-signed redirects for big-blob pulls

`URL()` exists on the interface but no handler calls it yet. For OCI
specifically, redirecting a `GET /v2/<name>/blobs/<digest>` to a
pre-signed S3 URL would mean a 200 MB layer pull never streams
through `pkgmirror`. One small block in the blob handler:

```go
if u, err := h.Service.Storage.URL(blob.HashSHA256, filename, nil); err == nil {
    c.Redirect(http.StatusTemporaryRedirect, u.String())
    return
}
// else fall through to the streaming path
```

`LocalStorage.URL` returns `ErrURLNotSupported`, so this code path is
a no-op on local installs and a big win on cloud installs.

### Multi-region replication

Out of scope. If you need it, configure replication at the bucket /
object-store level (S3 cross-region replication, GCS multi-region
buckets), not in pkgmirror.

### Encryption at rest

Out of scope at the application layer. Use whatever your storage
provider offers (S3 SSE, full-disk encryption, etc.). Content-
addressing makes this trivial because the bytes are opaque to
pkgmirror.

---

## Other backends worth considering

| Backend | Difficulty | When to pick |
| --- | --- | --- |
| **S3-compatible** (AWS S3, MinIO, Backblaze B2, R2, Wasabi) | Easy port of `forgejo/modules/storage/minio.go` | Default cloud answer. MinIO in dev = bit-identical to S3 in prod. |
| **GCS** | Easy — same shape as S3, different SDK | If you're already on GCP. |
| **Azure Blob** | Easy | If you're already on Azure. |
| **HTTP-backed** (read-only mirror at an arbitrary URL prefix) | Trivial | Useful for read-only DR replicas; not for writes. |
| **OCI Registry as backend** | Easy | Cute hack: use a "real" OCI registry as the byte store for all formats. We already speak the protocol on the receiving side. |
| **IPFS / content-addressed networks** | Medium | Interesting because content addressing already matches. Limited demand. |
| **SQLite BLOB column** | Trivial | **Don't.** SQLite handles small blobs poorly and the page-cache thrash kills you. Listed so it's explicitly ruled out. |

---

## See also

- [DECISIONS.md](../DECISIONS.md) (2026-05-28) — the decision log entry
  for adopting Forgejo's `ObjectStorage` shape, with the field-by-field
  mapping from the old `Backend` interface.
- [`forgejo/modules/storage`](https://codeberg.org/forgejo/forgejo/src/branch/forgejo/modules/storage)
  — the upstream reference. Our `LocalStorage` is a transliteration
  with sharding added; a future `S3` / `MinIO` backend would be a
  transliteration of `minio.go`.
- [adding-a-format.md](adding-a-format.md) — when adding a new format,
  every handler funnels through `Service.CreatePackageAndAddFile` /
  `Service.OpenFile`, which means the format never touches `Storage`
  directly. The exception is OCI; see the wiring table above.
