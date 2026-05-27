# Black-box conformance testing

This document describes how `pkgmirror` is validated against the real package
ecosystem clients (`go`, `npm`, `pip`, `mvn`, …) using disposable docker
containers, and how to add a new format's test suite.

## Goals

1. **Wire-format conformance.** Prove the mirror is byte-for-byte compatible
   with each ecosystem's native client. The only way to know that for
   certain is to run the native client against the mirror and inspect what
   it accepts.
2. **CI-friendly.** Tests must run cleanly on a vanilla GitHub Actions
   `ubuntu-latest` runner (and any other CI with a docker daemon) with no
   manual setup.
3. **Hermetic.** No reliance on whatever client tooling happens to be on the
   developer's machine. No leftover state between tests.
4. **Uniform.** Adding a new format's conformance suite should be a
   copy-paste of an existing one, changing only the client image and the
   command(s) it runs.

## Non-goals

- Load / soak testing. Conformance suites assert *correctness*, not
  performance. (Separate harness later.)
- Testing the `pkgmirror` UI. Covered by in-process grey-box tests.
- Cross-version client matrices. We pin to a single recent minor version per
  ecosystem and bump deliberately. Multi-version matrices are easy to add
  later by parameterizing the image tag.

## Architecture

```
                ┌─────────────────────────────────────┐
                │  per-test docker network (random)   │
   ┌────────────┴──────────┐   ┌────────────────────┐
   │ pkgmirror container   │   │ client container   │
   │ (our Dockerfile)      │←──│ official: golang / │
   │ network alias:        │   │ node / python / …  │
   │   "pkgmirror"         │   │ entrypoint: sleep  │
   │ listening on :8080    │   │ (we exec commands) │
   └───────────────────────┘   └────────────────────┘
                ▲                       ▲
                │                       │ docker exec
                │                       │
                └──── docker API ───────┘
                       (testcontainers-go)
                              │
                ┌─────────────┴──────────────┐
                │  go test process (host)    │
                │  tests/blackbox/<format>/  │
                └────────────────────────────┘
```

Both the system under test and the client run as containers on a private
docker network created per test. The test process is a Go binary that uses
[`testcontainers-go`](https://golang.testcontainers.org/) to orchestrate
everything via the docker API. The test process never speaks the package
protocol directly — it tells the client container to run its native
command (`go mod download`, `npm install`, `pip install`, …) and asserts on
exit codes, stdout, and the bytes the client cached locally.

### Why containers (not host-installed tools)

| Concern | Host-installed | Docker per language |
| --- | --- | --- |
| "Works on my mac" drift | Likely | Eliminated |
| CI bootstrap | Custom per CI | Just needs docker |
| Multiple language clients on one box | Manual installs | One image each |
| Pinning client version | Hard | Trivial (image tag) |
| Cleanup between tests | Risky | Container destroyed |
| Speed | Faster | +1–3s per container (acceptable) |

### Why containerize pkgmirror too

We could have run pkgmirror as a host binary and pointed containerized
clients at it (`host.docker.internal` on macOS, `--add-host` tricks on
Linux). We don't, because:

- The network topology is identical to production.
- Catches container-only bugs (file perms, env parsing, embedded assets).
- The client just connects to `http://pkgmirror:8080` — no platform-specific
  host-gateway gymnastics.
- One less "did you rebuild the host binary?" foot-gun.

### Why `testcontainers-go`

We use [`testcontainers-go`](https://golang.testcontainers.org/) as the
docker orchestration library because:

- It handles container build, network creation, port mapping, log capture,
  and cleanup-on-test-failure out of the box.
- The "Ryuk" reaper sidecar ensures that even if our test process crashes
  hard, orphaned containers and networks get cleaned up.
- It has first-class support for the wait conditions we need
  (`wait.ForHTTP("/-/healthz")`).
- It works identically on Docker Desktop, colima, Rancher Desktop, and CI
  runners.

Alternatives considered and rejected:

- **Plain `exec.Command("docker", …)` shell-outs.** Zero new Go deps, but we
  would reinvent wait conditions, network cleanup, log multiplexing, and
  port allocation. Estimated 300+ lines of fragile glue we'd then maintain.
- **`docker compose`.** Nice for "spin up the whole stack" but awkward when
  each test wants an isolated stack, and doesn't compose well with
  `t.Cleanup`.

## Layout

```
pkgmirror/
├── Dockerfile                       # multi-stage build of pkgmirror
├── .dockerignore
└── tests/
    └── blackbox/
        ├── harness/                 # shared infrastructure (Go pkg)
        │   ├── pkgmirror.go         # builds and runs the pkgmirror container
        │   ├── client.go            # spins up + execs into client containers
        │   └── doc.go
        ├── goproxy/
        │   ├── conformance_test.go  # golang:1.22 client
        │   └── fixtures.go          # builds Go module zips on the fly
        ├── npm/                     # later
        ├── pypi/                    # later
        └── maven/                   # later
```

All black-box test files carry `//go:build blackbox`, so the default
`go test ./...` ignores them and stays sub-second. The conformance suite
runs only when the build tag is set (see Running below).

## Running

### Locally

You need a running docker daemon (Docker Desktop, colima, Rancher Desktop,
Podman with the docker socket compat layer — anything that exposes
`/var/run/docker.sock` or equivalent).

```sh
# Fast (no docker required) — unit / grey-box tests only.
make test

# Full conformance suite — builds the pkgmirror image, pulls client
# images, and drives them.
make test-blackbox
```

The first run builds the pkgmirror image (~30s on a cold cache) and pulls
each client image (~5–60s depending on size and network). Subsequent runs
reuse cached layers.

### In CI

The repo includes `.github/workflows/blackbox.yml` which:

1. Checks out the repo.
2. Sets up Docker Buildx (for layer caching).
3. Pre-pulls client images in parallel.
4. Runs the conformance suite with `-tags=blackbox`.
5. Uploads container logs on failure.

Container logs from the failing test are dumped via `t.Logf`, so they
appear inline in the failing test's output even without artifact upload.

## Harness contract

`tests/blackbox/harness` exposes:

```go
// Start brings up a fresh pkgmirror container on a new docker network
// and returns a Stack with cleanup registered on t.
func Start(ctx context.Context, t *testing.T) *Stack

type Stack struct {
    // HostBaseURL is the URL reachable from the host (random high port).
    // Use this for direct probes from the test process (e.g. uploads).
    HostBaseURL string

    // InternalBaseURL is the URL reachable from sibling containers on the
    // stack's network: "http://pkgmirror:8080".
    InternalBaseURL string
}

// NewClient starts a long-lived container of the given image on the stack
// network. The container's entrypoint is overridden so it sleeps until
// terminated; tests drive it by calling Client.Exec.
func (s *Stack) NewClient(ctx context.Context, t *testing.T, spec ClientSpec) *Client

type ClientSpec struct {
    Image   string             // e.g. "golang:1.22-bookworm"
    Env     map[string]string  // env vars set in the container
    WorkDir string             // working dir inside the container
    Files   map[string]string  // file path inside container → contents
}

func (c *Client) Exec(ctx context.Context, cmd ...string) (stdout string, exitCode int, err error)
func (c *Client) MustExec(t *testing.T, cmd ...string) string
```

`MustExec` fails the test on non-zero exit, dumping pkgmirror logs and
client output for diagnosis.

## How to add a new format

1. Implement the format's HTTP handlers (e.g. `internal/packages/npm/`).
2. Create `tests/blackbox/<format>/conformance_test.go`:

   ```go
   //go:build blackbox

   package npm_blackbox_test

   func TestNPMConformance(t *testing.T) {
       ctx := context.Background()
       stack := harness.Start(ctx, t)

       // 1. Populate the mirror with a fixture package via the upload API.
       harness.UploadTarball(t, stack, "testdata/foo-1.0.0.tgz")

       // 2. Drive the real client.
       client := stack.NewClient(ctx, t, harness.ClientSpec{
           Image: "node:22-bookworm",
           Env: map[string]string{
               "npm_config_registry": stack.InternalBaseURL + "/api/packages/npm",
           },
           WorkDir: "/work",
           Files: map[string]string{
               "/work/package.json": `{"name":"consumer","dependencies":{"foo":"1.0.0"}}`,
           },
       })
       out := client.MustExec(t, "npm", "install", "--no-audit", "--no-fund")
       t.Logf("npm install:\n%s", out)

       // 3. Assert on what the client did.
       out = client.MustExec(t, "node", "-e", `console.log(require("foo").greet())`)
       if !strings.Contains(out, "hello") {
           t.Fatalf("expected greet() to print 'hello', got: %s", out)
       }
   }
   ```

3. Pick the official, debian-based, pinned-to-minor-version image:
   - Go: `golang:1.22-bookworm`
   - Node: `node:22-bookworm`
   - Python: `python:3.12-slim`
   - Ruby: `ruby:3.3-slim`
   - Java/Maven: `maven:3.9-eclipse-temurin-21`
   - Rust: `rust:1.81-bookworm`
   - .NET: `mcr.microsoft.com/dotnet/sdk:8.0`
   - PHP: `composer:2`
   - Dart: `dart:3.5`
   - Swift: `swift:5.10`

4. Add the new test path to `.github/workflows/blackbox.yml` if you want it
   to pre-pull the image (purely an optimization; the test will pull on
   demand otherwise).

## Image pinning policy

- Pin to **minor** version (e.g. `node:22-bookworm`, not `node:22.1.0` and
  not `node:22`). Major.minor changes deliberately, patch is tracked
  automatically.
- Prefer **debian-bookworm** flavor over alpine to avoid musl/glibc
  surprises with native packages (npm node-gyp, Python wheels). Use alpine
  variants only when an ecosystem demands it.
- Tag bumps go in their own commit so a client regression can be bisected.

## Performance budget

Approximate wall-clock per conformance suite on a warm cache:

| Phase | Cost |
| --- | --- |
| pkgmirror image build (cached) | <2s |
| pkgmirror container start + healthz | ~1s |
| Client container start | 1–3s |
| One `Exec` call | ~100ms + command time |
| Suite total (1 upload + 2–3 exec) | 5–15s |

Cold cache (no images): add 30–90s of image pulls, dominated by the client.
CI runs all suites in parallel across formats; full matrix completes in
1–3 minutes.

## Failure diagnosis

When a black-box test fails, four sources of evidence get dumped to the
test log:

1. **Test assertion** (which expectation failed and what was seen).
2. **Client `stdout` + `stderr`** (merged, captured by `Exec`).
3. **pkgmirror container logs** (captured via the testcontainers log API).
4. **Docker exec exit code** (often the first thing to look at).

Plus, because containers are torn down on `t.Cleanup`, a passing test
leaves nothing behind; a failing test under `-failfast` keeps the
containers via the `TC_KEEP_CONTAINERS=1` env var (set this when you want
to `docker exec` into the failed client by hand).
