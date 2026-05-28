//go:build blackbox

// Debian apt wire-compatibility test. Drives real `apt` from
// debian:bookworm-slim against our registry: install the tenant's
// signing public key in /etc/apt/keyrings/, point sources.list at
// us, then run `apt update` followed by `apt show` to confirm the
// uploaded .deb is in the parsed index.
//
// The signature-validation step inside `apt update` is the
// load-bearing assertion. If our InRelease clearsigned bytes,
// detached Release.gpg, or any Packages hash in Release doesn't
// match what we serve, apt prints a "GPG error" or "Hash Sum
// mismatch" and `apt update` exits non-zero.
//
// We don't drive `apt install foo` because that requires a fully
// extractable data.tar inside the .deb (rootfs payload, scripts,
// dependency closure) which is well beyond what we need to prove
// index + signature correctness.

package debian_blackbox_test

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/astockwell/pkgmirror/tests/blackbox/harness"

	"github.com/blakesmith/ar"
)

// repoBase returns the per-tenant Debian repo root that goes in
// /etc/apt/sources.list. apt appends `dists/<dist>/...` and
// `pool/<dist>/...` to whatever URL we provide.
func repoBase(s *harness.Stack) string {
	return s.InternalBaseURL + "/api/packages/" + harness.DefaultTenant + "/debian"
}

// buildDeb constructs a minimal .deb: an `ar` archive containing
// debian-binary, control.tar (with a single `control` file), and
// data.tar (empty). Real `apt install` would extract data.tar onto
// the rootfs; we don't drive `install`, so an empty data tarball is
// enough.
func buildDeb(t *testing.T, name, version, arch string) []byte {
	t.Helper()
	control := "Package: " + name +
		"\nVersion: " + version +
		"\nArchitecture: " + arch +
		"\nMaintainer: pkgmirror tests <tests@example.test>" +
		"\nDescription: pkgmirror conformance fixture\n"

	ctar := tarSingle(t, "control", []byte(control))
	dtar := tarSingle(t, ".", nil) // empty data.tar with a single directory entry

	var deb bytes.Buffer
	aw := ar.NewWriter(&deb)
	if err := aw.WriteGlobalHeader(); err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct {
		name string
		body []byte
	}{
		{"debian-binary", []byte("2.0\n")},
		{"control.tar", ctar},
		{"data.tar", dtar},
	} {
		if err := aw.WriteHeader(&ar.Header{Name: m.name, Mode: 0o600, Size: int64(len(m.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := aw.Write(m.body); err != nil {
			t.Fatal(err)
		}
	}
	return deb.Bytes()
}

func tarSingle(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	typeflag := tar.TypeReg
	if name == "." {
		typeflag = tar.TypeDir
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: byte(typeflag), Mode: 0o755, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if len(body) > 0 {
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDebianConformance_AptUpdateAndShow(t *testing.T) {
	ctx := context.Background()
	stack := harness.Start(ctx, t)

	// 1) Publish fixtures for both amd64 and arm64 so the test is
	// arch-agnostic. apt fetches Packages for the container's
	// runtime arch (amd64 on linux/amd64 CI runners, arm64 on
	// Apple Silicon dev laptops) and fails if it's missing.
	uploadURL := fmt.Sprintf("/api/packages/%s/debian/pool/bookworm/main/upload", harness.DefaultTenant)
	for _, arch := range []string{"amd64", "arm64"} {
		harness.UploadBytes(t, stack,
			uploadURL,
			"application/vnd.debian.binary-package",
			buildDeb(t, "fixture-pkg", "1.0.0", arch))
	}

	// 2) Start an apt client. We deliberately do NOT pre-stage the
	// pkgmirror sources or auth.conf in container Files — they
	// reference a key/path that doesn't exist yet, and apt-get
	// update during package install (step 3) would fail with
	// NO_PUBKEY. Configuration is written from inside the
	// container at step 4, after the key is installed.
	repo := repoBase(stack)
	client := stack.NewClient(ctx, t, harness.ClientSpec{
		Image:   "debian:bookworm-slim",
		WorkDir: "/work",
		Env: map[string]string{
			"DEBIAN_FRONTEND": "noninteractive",
		},
	})

	// 3) Bring in the tools we need from Debian's default mirrors
	// (which the base image already has configured). gnupg is the
	// `gpg` binary used to dearmor our PGP public key into the
	// binary keyring format apt expects.
	client.MustExec(t, "sh", "-c",
		"apt-get update -y >/dev/null && apt-get install -y --no-install-recommends curl ca-certificates gnupg >/dev/null")

	// 4) Fetch + dearmor our public key, then configure the apt
	// sources file pointing at us. The auth.conf must include the
	// `http://` protocol annotation; apt 2.x rejects credentials
	// without it as a security precaution against accidentally
	// leaking them over HTTPS.
	client.MustExec(t, "mkdir", "-p", "/etc/apt/keyrings")
	client.MustExec(t, "sh", "-c",
		fmt.Sprintf("curl -fsS -u x:%s %s/key.gpg | gpg --dearmor -o /etc/apt/keyrings/pkgmirror.gpg",
			stack.AdminToken, repo))

	authConf := fmt.Sprintf("machine %s\nlogin x\npassword %s\n", repo, stack.AdminToken)
	client.MustExec(t, "sh", "-c",
		fmt.Sprintf("printf %q > /etc/apt/auth.conf.d/pkgmirror.conf", authConf))

	sources := "Types: deb\n" +
		"URIs: " + repo + "\n" +
		"Suites: bookworm\n" +
		"Components: main\n" +
		"Signed-By: /etc/apt/keyrings/pkgmirror.gpg\n"
	client.MustExec(t, "sh", "-c",
		fmt.Sprintf("printf %q > /etc/apt/sources.list.d/pkgmirror.sources", sources))

	// Replace Debian's default sources so subsequent apt-get update
	// only talks to pkgmirror. We don't want the test to depend on
	// snapshot.debian.org or any external apt mirror, and we want
	// failures to point unambiguously at our registry.
	client.MustExec(t, "sh", "-c",
		`: > /etc/apt/sources.list && rm -f /etc/apt/sources.list.d/debian.sources`)

	// 5) apt update -- signature validation happens HERE. If our
	// InRelease clearsign, detached Release.gpg, or any Packages
	// hash in Release doesn't match, this fails loud.
	out := client.MustExec(t, "apt-get", "update")
	if !strings.Contains(out, repo) {
		t.Fatalf("apt update did not mention our repo:\n%s", out)
	}
	if strings.Contains(out, "Hash Sum mismatch") {
		t.Fatalf("apt update Hash Sum mismatch:\n%s", out)
	}
	if strings.Contains(out, "GPG error") {
		t.Fatalf("apt update GPG error:\n%s", out)
	}
	if strings.Contains(out, "is not signed") {
		t.Fatalf("apt update says repo is not signed:\n%s", out)
	}

	// 6) apt-cache show finds our package and parses its metadata.
	out = client.MustExec(t, "apt-cache", "show", "fixture-pkg")
	if !strings.Contains(out, "Version: 1.0.0") {
		t.Fatalf("apt-cache show did not list 1.0.0:\n%s", out)
	}
	if !strings.Contains(out, "Package: fixture-pkg") {
		t.Fatalf("apt-cache show did not list fixture-pkg:\n%s", out)
	}
}
