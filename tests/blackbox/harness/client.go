//go:build blackbox || integration

package harness

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
)

// ClientSpec describes a sibling container we want to start on the stack
// network and exec commands inside.
type ClientSpec struct {
	// Image is the docker image tag, e.g. "golang:1.22-bookworm".
	Image string

	// Env are environment variables to set in the container.
	Env map[string]string

	// WorkDir is the working directory inside the container. Files are
	// written and Exec commands are run from here.
	WorkDir string

	// Files maps absolute container paths to file contents. The files are
	// created before the container starts.
	Files map[string]string
}

// Client is a running, idle container we can Exec commands inside.
type Client struct {
	Container testcontainers.Container
	Image     string
}

// NewClient brings up a container of spec.Image on the stack's network. The
// container is started with an "idle" entrypoint (`sleep infinity`) so the
// test can drive it through Client.Exec.
func (s *Stack) NewClient(ctx context.Context, t *testing.T, spec ClientSpec) *Client {
	t.Helper()

	files := make([]testcontainers.ContainerFile, 0, len(spec.Files))
	// Sort for deterministic ordering.
	paths := make([]string, 0, len(spec.Files))
	for p := range spec.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		body := spec.Files[p]
		files = append(files, testcontainers.ContainerFile{
			ContainerFilePath: p,
			Reader:            bytes.NewReader([]byte(body)),
			FileMode:          0o644,
		})
	}

	req := testcontainers.ContainerRequest{
		Image:      spec.Image,
		Networks:   []string{s.Network.Name},
		Env:        spec.Env,
		WorkingDir: spec.WorkDir,
		Files:      files,
		// Override the image's entrypoint so the container stays up while
		// we exec commands. `sleep infinity` is supported by every modern
		// debian/ubuntu/alpine base image we'd plausibly use.
		Entrypoint: []string{"sleep", "infinity"},
		Cmd:        nil,
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start client container (%s): %v", spec.Image, err)
	}
	t.Cleanup(func() {
		dumpLogsOnFailure(t, container, "client:"+spec.Image)
		shCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = container.Terminate(shCtx)
	})

	return &Client{Container: container, Image: spec.Image}
}

// Exec runs cmd in the container and returns its merged stdout+stderr, the
// exit code, and any docker-level error. The command is executed via
// `sh -c` with 2>&1 to merge streams into a single byte stream; we then
// ask testcontainers to demultiplex the docker exec frame protocol so
// callers get plain text back.
func (c *Client) Exec(ctx context.Context, cmd ...string) (string, int, error) {
	if len(cmd) == 0 {
		return "", -1, fmt.Errorf("Exec: empty command")
	}
	shellCmd := []string{"sh", "-c", shellJoin(cmd) + " 2>&1"}
	code, reader, err := c.Container.Exec(ctx, shellCmd, tcexec.Multiplexed())
	if err != nil {
		return "", code, fmt.Errorf("exec %v: %w", cmd, err)
	}
	out, _ := io.ReadAll(reader)
	return string(out), code, nil
}

// MustExec runs cmd and fails the test on non-zero exit or docker error.
// Returns the merged output.
func (c *Client) MustExec(t *testing.T, cmd ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	out, code, err := c.Exec(ctx, cmd...)
	if err != nil {
		t.Fatalf("MustExec %v: docker error: %v\noutput:\n%s", cmd, err, out)
	}
	if code != 0 {
		t.Fatalf("MustExec %v: exit=%d\noutput:\n%s", cmd, code, out)
	}
	return out
}

// CopyFileFrom copies a file from inside the container to the host as a
// byte slice. Useful for asserting that the client cached the correct bytes.
func (c *Client) CopyFileFrom(ctx context.Context, containerPath string) ([]byte, error) {
	rc, err := c.Container.CopyFileFromContainer(ctx, containerPath)
	if err != nil {
		return nil, fmt.Errorf("copy %s: %w", containerPath, err)
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// shellJoin quotes args for safe interpolation into `sh -c`. Each arg is
// single-quoted; embedded single quotes are escaped via the standard
// '\'' dance.
func shellJoin(args []string) string {
	var b bytes.Buffer
	for i, a := range args {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteByte('\'')
		for _, r := range a {
			if r == '\'' {
				b.WriteString(`'\''`)
			} else {
				b.WriteRune(r)
			}
		}
		b.WriteByte('\'')
	}
	return b.String()
}
