//go:build blackbox

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

// Start builds the pkgmirror image (cached by docker), creates a fresh
// docker network, launches the container, and waits for /-/healthz.
// All resources are torn down via t.Cleanup.
func Start(ctx context.Context, t *testing.T) *Stack {
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

	req := testcontainers.ContainerRequest{
		FromDockerfile: testcontainers.FromDockerfile{
			Context:       repoRoot(t),
			Dockerfile:    "Dockerfile",
			PrintBuildLog: testing.Verbose(),
			KeepImage:     true,
		},
		ExposedPorts: []string{"8080/tcp"},
		Env: map[string]string{
			"PKGMIRROR_ADMIN_TOKEN":                bootstrapAdminToken,
			// Public default tenant so the go toolchain can fetch over
			// plain HTTP without needing credentials (Go refuses to send
			// auth headers over HTTP regardless of GOAUTH/netrc/URL).
			// Auth-gate behavior on private tenants is exercised by the
			// in-process tests in internal/packages/goproxy.
			"PKGMIRROR_DEFAULT_TENANT_VISIBILITY": "public",
		},
		Networks: []string{net.Name},
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
