//go:build blackbox || integration

package harness

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcnet "github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// pkgmirrorAlias is the network alias under which the pkgmirror container is
// reachable from sibling containers on the same docker network.
const pkgmirrorAlias = "pkgmirror"

// bootstrapAdminToken is a fixed plaintext admin token injected via
// PKGMIRROR_ADMIN_TOKEN. Black-box tests use it for uploads and reads
// against private tenants. It is NOT a secret — it is regenerated for each
// test process and only exists inside ephemeral docker containers.
const bootstrapAdminToken = "pkm_blackboxtestadminadminadminadmi" // 32 base32 chars

// DefaultTenant is the name of the tenant pkgmirror auto-creates on first
// boot. Black-box tests use it as their playground.
const DefaultTenant = "default"

// Stack is a running pkgmirror container + its private docker network. Use
// Stack.NewClient to bring up a sibling client container that can talk to
// pkgmirror at InternalBaseURL.
type Stack struct {
	// HostBaseURL is reachable from the host (e.g. for direct uploads from
	// the test process). It contains a random high port.
	HostBaseURL string

	// InternalBaseURL is reachable from sibling containers on the stack
	// network. Always "http://pkgmirror:8080".
	InternalBaseURL string

	// AdminToken is the plaintext admin token usable for all reads/writes
	// in the bootstrapped default tenant.
	AdminToken string

	// Network is the per-test docker network.
	Network *testcontainers.DockerNetwork

	// Container is the running pkgmirror container.
	Container testcontainers.Container
}

// Options are knobs passed to StartWithOptions. The zero value matches
// the original Start() semantics: pull-through OFF (so blackbox tests
// are insulated from upstream behavior changes), public default tenant.
//
// Integration tests (tests/integration/...) populate these to opt INTO
// pull-through against real public registries.
type Options struct {
	// ExtraEnv is merged into the pkgmirror container's env after the
	// defaults; keys here win on collision so callers can override
	// PKGMIRROR_UPSTREAM_DEFAULT_MODE, the User-Agent, etc.
	ExtraEnv map[string]string

	// AllowEgress, when true, gives the pkgmirror container access to
	// the real internet (the default network in Docker already permits
	// egress, so this is a no-op today; reserved for the future where
	// we may sandbox blackbox via --network=none and only re-enable
	// for integration).
	AllowEgress bool
}

// Start launches a default-configured pkgmirror stack with pull-through
// DISABLED. Equivalent to StartWithOptions(ctx, t, Options{}).
//
// Use this from blackbox tests so the suite remains independent of
// any changes to upstream pull-through behavior.
func Start(ctx context.Context, t *testing.T) *Stack {
	return StartWithOptions(ctx, t, Options{})
}

// StartWithOptions builds the pkgmirror image (cached by docker), creates
// a fresh docker network, launches the container with the requested
// option overlay, and waits for /-/healthz. All resources are torn down
// via t.Cleanup.
func StartWithOptions(ctx context.Context, t *testing.T, opts Options) *Stack {
	t.Helper()

	net, err := tcnet.New(ctx)
	if err != nil {
		t.Fatalf("create docker network: %v", err)
	}
	t.Cleanup(func() {
		// network.Remove() needs a fresh context in case the test context
		// was already cancelled.
		shCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = net.Remove(shCtx)
	})

	env := map[string]string{
		"PKGMIRROR_ADMIN_TOKEN": bootstrapAdminToken,
		// Public default tenant so the go toolchain can fetch over
		// plain HTTP without needing credentials (Go refuses to send
		// auth headers over HTTP regardless of GOAUTH/netrc/URL).
		// Auth-gate behavior on private tenants is exercised by the
		// in-process tests in internal/packages/goproxy.
		"PKGMIRROR_DEFAULT_TENANT_VISIBILITY": "public",
		// Default OFF so blackbox suites - which test format-protocol
		// conformance, NOT pull-through - are not affected by future
		// changes to the upstream system. Integration tests opt in
		// via Options.ExtraEnv.
		"PKGMIRROR_UPSTREAM_DEFAULT_MODE": "off",
	}
	for k, v := range opts.ExtraEnv {
		env[k] = v
	}

	req := testcontainers.ContainerRequest{
		FromDockerfile: testcontainers.FromDockerfile{
			Context:       repoRoot(t),
			Dockerfile:    "Dockerfile",
			PrintBuildLog: testing.Verbose(),
			KeepImage:     true,
		},
		ExposedPorts: []string{"8080/tcp"},
		Env:          env,
		Networks:     []string{net.Name},
		NetworkAliases: map[string][]string{
			net.Name: {pkgmirrorAlias},
		},
		WaitingFor: wait.ForHTTP("/-/healthz").
			WithPort("8080/tcp").
			WithStartupTimeout(60 * time.Second),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start pkgmirror container: %v", err)
	}
	t.Cleanup(func() {
		dumpLogsOnFailure(t, container, "pkgmirror")
		shCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = container.Terminate(shCtx)
	})

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := container.MappedPort(ctx, "8080/tcp")
	if err != nil {
		t.Fatalf("mapped port: %v", err)
	}

	return &Stack{
		HostBaseURL:     fmt.Sprintf("http://%s:%s", host, port.Port()),
		InternalBaseURL: fmt.Sprintf("http://%s:8080", pkgmirrorAlias),
		AdminToken:      bootstrapAdminToken,
		Network:         net,
		Container:       container,
	}
}

// repoRoot resolves the repository root by walking up from this file's
// location. tests/blackbox/harness/pkgmirror.go → ../../..
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	// file: <repo>/tests/blackbox/harness/pkgmirror.go
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

// dumpLogsOnFailure writes the container's stdout+stderr to the test log
// if (and only if) the test has failed.
func dumpLogsOnFailure(t *testing.T, c testcontainers.Container, label string) {
	if !t.Failed() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rc, err := c.Logs(ctx)
	if err != nil {
		t.Logf("[%s] could not fetch logs: %v", label, err)
		return
	}
	defer rc.Close()
	buf := make([]byte, 64*1024)
	n, _ := rc.Read(buf)
	t.Logf("[%s container logs]\n%s", label, string(buf[:n]))
}
