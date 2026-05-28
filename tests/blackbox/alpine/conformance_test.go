//go:build blackbox

// Alpine apk wire-compatibility test. Drives a real `apk` client
// (alpine:3.20) against our registry: install the tenant's RSA public
// key, point /etc/apk/repositories at us, then run `apk update`. The
// signature validation built into `apk update` is the load-bearing
// assertion — if our APKINDEX signing doesn't match what apk expects
// byte-for-byte, this test fails with `BAD signature`.
//
// We don't attempt `apk add foo` because that requires building a
// fully-installable .apk (rootfs tar + scripts + dependency closure),
// which is well beyond the scope of "is our index format correct".
// `apk search` after `apk update` proves the index is parsed
// successfully without trying to install anything.

package alpine_blackbox_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/astockwell/pkgmirror/tests/blackbox/harness"
)

// registryURL is the per-tenant repo root. apk appends
// `/<arch>/APKINDEX.tar.gz` and `/<arch>/<filename>.apk` to whatever
// goes into /etc/apk/repositories, so the URL we hand the client
// stops at the <branch>/<repository> level.
func registryURL(s *harness.Stack, branch, repository string) string {
	return s.InternalBaseURL + "/api/packages/" + harness.DefaultTenant + "/alpine/" + branch + "/" + repository
}

// buildAPK constructs a minimal but parser-valid .apk: two gzip
// streams, the second containing a tar with a single .PKGINFO entry.
// `apk update` only reads the APKINDEX (which our server generates
// from PKGINFO), not the .apk body, so this is enough to pass the
// index round trip. If we later add a real `apk add` test we'd need
// the data tar + control hash dance too.
func buildAPK(t *testing.T, pkginfo string) []byte {
	t.Helper()
	var buf bytes.Buffer

	// Stream 1: dummy. A real .apk has a SIGN.RSA stream here; we
	// don't sign individual packages and apk doesn't verify them
	// during `update`, only `add`.
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	mustTarFile(t, tw, ".SIGN.RSA.placeholder", []byte{0})
	mustClose(t, tw)
	mustClose(t, zw)

	// Stream 2: control. This is the stream our server parses for
	// PKGINFO metadata and whose gz bytes feed the "Q1" checksum.
	zw = gzip.NewWriter(&buf)
	tw = tar.NewWriter(zw)
	mustTarFile(t, tw, ".PKGINFO", []byte(pkginfo))
	mustClose(t, tw)
	mustClose(t, zw)

	return buf.Bytes()
}

func mustTarFile(t *testing.T, tw *tar.Writer, name string, body []byte) {
	t.Helper()
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Mode: 0o600, Size: int64(len(body)),
	}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("tar write: %v", err)
	}
}

type closer interface{ Close() error }

func mustClose(t *testing.T, c closer) {
	t.Helper()
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func samplePKGINFO(name, version, arch, desc string) string {
	return "pkgname = " + name + "\n" +
		"pkgver = " + version + "\n" +
		"pkgdesc = " + desc + "\n" +
		"url = https://example.test/" + name + "\n" +
		"size = 1024\n" +
		"arch = " + arch + "\n" +
		"origin = " + name + "\n" +
		"maintainer = pkgmirror-tests <tests@example.test>\n" +
		"license = MIT\n" +
		"builddate = 1700000000\n"
}

// fetchPublicKey downloads the per-tenant RSA public key from the
// registry. apk needs the *.rsa.pub filename to match what the
// signature stream embedded inside APKINDEX.tar.gz advertises
// (`<owner>@<fingerprint>.rsa.pub`); the server's Content-Disposition
// gives us that filename, but we don't actually parse it here — apk
// matches purely on file content during signature verification.
func fetchPublicKey(t *testing.T, s *harness.Stack) ([]byte, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, s.HostBaseURL+"/api/packages/"+harness.DefaultTenant+"/alpine/key", nil)
	req.Header.Set("Authorization", "Bearer "+s.AdminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fetch key: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fetch key: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	// Pull filename out of Content-Disposition: attachment;
	// filename="<owner>@<fingerprint>.rsa.pub". Anything goes wrong
	// and we fall back to a generic name — apk only checks content,
	// not filename, but a recognizable name aids debugging.
	cd := resp.Header.Get("Content-Disposition")
	filename := "pkgmirror.rsa.pub"
	if i := strings.Index(cd, `filename="`); i >= 0 {
		rest := cd[i+len(`filename="`):]
		if j := strings.Index(rest, `"`); j >= 0 {
			filename = rest[:j]
		}
	}
	return body, filename
}

// TestAlpineConformance_ApkUpdateAndSearch is the end-to-end wire
// test. We upload one .apk, run `apk update` (which forces signature
// verification against the installed public key), then `apk search`
// to prove the index parsed.
func TestAlpineConformance_ApkUpdateAndSearch(t *testing.T) {
	ctx := context.Background()
	stack := harness.Start(ctx, t)

	// 1) Upload fixture .apks. We publish for both x86_64 and
	// aarch64 because CI hosts and dev laptops disagree on the
	// native arch alpine:3.20 will run (`apk` fetches
	// `<arch>/APKINDEX.tar.gz` where arch is the container's
	// runtime arch — `aarch64` on Apple Silicon, `x86_64` on
	// linux/amd64 runners). Uploading both arches keeps the test
	// arch-agnostic without any branching on uname.
	uploadURL := fmt.Sprintf("/api/packages/%s/alpine/v3.20/main", harness.DefaultTenant)
	for _, arch := range []string{"x86_64", "aarch64"} {
		apkBody := buildAPK(t, samplePKGINFO("fixture-pkg", "1.0.0", arch, "pkgmirror conformance fixture"))
		harness.UploadBytes(t, stack, uploadURL, "application/octet-stream", apkBody)
	}

	// 2) Fetch the tenant's signing public key so we can install it
	// in the apk client.
	pubKey, keyFilename := fetchPublicKey(t, stack)

	// 3) Start an alpine:3.20 client container, install the key,
	// point /etc/apk/repositories at us.
	repo := registryURL(stack, "v3.20", "main")
	client := stack.NewClient(ctx, t, harness.ClientSpec{
		Image:   "alpine:3.20",
		WorkDir: "/work",
		Files: map[string]string{
			// Drop the public key in apk's trust store. apk reads
			// every *.pub here during verification.
			"/etc/apk/keys/" + keyFilename: string(pubKey),
			// Replace the default mirror with just our registry so
			// `apk update` doesn't try to reach the public Alpine
			// CDN (which is blocked in CI and unreliable elsewhere).
			"/etc/apk/repositories": repo + "\n",
		},
	})

	// 4) apk update — this is where signature verification happens.
	// If our APKINDEX signing is wrong we get "UNTRUSTED signature"
	// here and the test fails loud and clear.
	out := client.MustExec(t, "apk", "update")
	if !strings.Contains(out, "OK:") {
		t.Fatalf("apk update did not report OK:\n%s", out)
	}

	// 5) apk search confirms our package shows up in the parsed
	// index. The output format is `<name>-<version> description`.
	out = client.MustExec(t, "apk", "search", "-v", "fixture-pkg")
	if !strings.Contains(out, "fixture-pkg-1.0.0") {
		t.Fatalf("apk search did not list fixture-pkg-1.0.0:\n%s", out)
	}
}
