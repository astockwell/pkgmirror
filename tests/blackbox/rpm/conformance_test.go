//go:build blackbox

// RPM dnf wire-compatibility test. Drives real `dnf` from
// fedora:41 against our registry: install the tenant's GPG public
// key into rpm's keyring, point /etc/yum.repos.d/ at us, run `dnf
// makecache` (which validates the signed repomd.xml.asc) and `dnf
// info` (which parses our generated primary.xml).
//
// The repomd.xml.asc signature check inside `dnf makecache` is the
// load-bearing assertion. If our index bytes drift between
// /repomd.xml and /repomd.xml.asc — the same on-demand-signing trap
// that bit Debian — `dnf makecache` fails with "Could not find a
// valid GPG signature" or similar.

package rpm_blackbox_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/astockwell/pkgmirror/tests/blackbox/harness"
)

// fixtureRPMBase64Gz is the upstream Forgejo test rpm (gitea-test
// 1.0.2-1.x86_64.rpm), gzipped + base64'd. Same fixture as the
// grey-box test; we don't share since blackbox tests deliberately
// don't import internal packages.
const fixtureRPMBase64Gz = `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`

func decodeFixtureRPM(t *testing.T) []byte {
	t.Helper()
	gz, err := base64.StdEncoding.DecodeString(fixtureRPMBase64Gz)
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	gr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	body, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return body
}

func repoBase(s *harness.Stack, group string) string {
	return s.InternalBaseURL + "/api/packages/" + harness.DefaultTenant + "/rpm/" + group
}

func TestRPMConformance_DnfMakecacheAndInfo(t *testing.T) {
	ctx := context.Background()
	stack := harness.Start(ctx, t)

	// 1) Publish the fixture .rpm into group "el9".
	uploadURL := fmt.Sprintf("/api/packages/%s/rpm/el9/upload", harness.DefaultTenant)
	harness.UploadBytes(t, stack, uploadURL, "application/x-rpm", decodeFixtureRPM(t))

	repo := repoBase(stack, "el9")

	// 2) Start a fedora:41 client. dnf is preinstalled there; we
	// don't need to install anything extra.
	client := stack.NewClient(ctx, t, harness.ClientSpec{
		Image:   "fedora:41",
		WorkDir: "/work",
	})

	// 3) Trust the registry. Fetching the .repo file from us is the
	// natural path operators take but the URL embedded in it would
	// reference the *internal* container hostname, which works
	// fine for the test. We also need to install the GPG public
	// key as a *file path* in the .repo's `gpgkey=` (dnf accepts
	// file:// or http:// URIs).
	client.MustExec(t, "sh", "-c",
		fmt.Sprintf(`curl -fsS -u x:%s %s/repository.key -o /etc/pki/rpm-gpg/RPM-GPG-KEY-pkgmirror && \
			rpm --import /etc/pki/rpm-gpg/RPM-GPG-KEY-pkgmirror`,
			stack.AdminToken, repo))

	// 4) Configure the dnf repo. We write it directly rather than
	// `curl ... > .repo` because the auto-rendered repository.repo
	// embeds the URL without credentials (dnf reads creds from
	// /etc/dnf/credentials.d/, but that's distro-version-sensitive).
	// Embedding `user:token@host` in the baseurl is the simplest
	// thing that works with both dnf 4 and dnf 5.
	authedRepo := strings.Replace(repo, "http://", "http://x:"+stack.AdminToken+"@", 1)
	repoConf := fmt.Sprintf(`[pkgmirror-test]
name=pkgmirror test
baseurl=%s
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-pkgmirror
`, authedRepo)
	client.MustExec(t, "sh", "-c",
		fmt.Sprintf("printf %q > /etc/yum.repos.d/pkgmirror.repo", repoConf))

	// 5) Disable all the default fedora repos so this test doesn't
	// pull from a Fedora mirror (which is blocked in CI and would
	// also pollute the failure message).
	client.MustExec(t, "sh", "-c",
		"rm -f /etc/yum.repos.d/fedora.repo /etc/yum.repos.d/fedora-updates.repo /etc/yum.repos.d/fedora-cisco-openh264.repo")

	// 6) dnf makecache — this is where the repomd.xml.asc signature
	// gets validated. If our deterministic-timestamp approach for
	// the signed-on-demand bytes is wrong, this fails with
	// "Could not find a valid GPG signature".
	out := client.MustExec(t, "dnf", "-y", "--repo=pkgmirror-test", "makecache")
	if strings.Contains(out, "GPG check FAILED") {
		t.Fatalf("dnf makecache reported GPG check failure:\n%s", out)
	}
	if strings.Contains(out, "Could not find") {
		t.Fatalf("dnf makecache could not find something:\n%s", out)
	}

	// 7) dnf info parses our primary.xml and surfaces the package
	// metadata. The presence of "Version : 1.0.2" and our license
	// proves the metadata extraction round-tripped end to end.
	out = client.MustExec(t, "dnf", "-y", "--repo=pkgmirror-test", "info", "gitea-test")
	for _, want := range []string{
		"Name", "gitea-test",
		"Version", "1.0.2",
		"License", "MIT",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("dnf info output missing %q:\n%s", want, out)
		}
	}
}
