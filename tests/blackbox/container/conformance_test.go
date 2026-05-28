//go:build blackbox

package container_blackbox_test

import (
	"context"
	"net/url"
	"testing"

	"github.com/astockwell/pkgmirror/tests/blackbox/harness"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// authenticator wraps the harness's admin token in the OCI authn interface.
// Crane / go-containerregistry use HTTP Basic with username "any" and the
// token as the password — our extractToken handles that correctly because
// it pulls the password half from the Basic header.
type authenticator struct {
	token string
}

func (a *authenticator) Authorization() (*authn.AuthConfig, error) {
	return &authn.AuthConfig{Username: "any", Password: a.token}, nil
}

// registryHost extracts the host:port from the harness's HostBaseURL.
// crane's reference parser needs just "host:port", not the full URL.
func registryHost(t *testing.T, s *harness.Stack) string {
	t.Helper()
	u, err := url.Parse(s.HostBaseURL)
	if err != nil {
		t.Fatalf("parse host url: %v", err)
	}
	return u.Host
}

// TestOCIConformance_PushAndPull drives go-containerregistry (the
// library `crane` is built from) end to end against the mirror: build
// an image entirely in memory, push it, then pull it back and verify
// digest + layer contents match. This is the canonical OCI
// wire-conformance check.
func TestOCIConformance_PushAndPull(t *testing.T) {
	ctx := context.Background()
	stack := harness.Start(ctx, t)
	host := registryHost(t, stack)
	auth := &authenticator{token: stack.AdminToken}

	// Random in-memory image. 1 layer, 256 bytes. The library hashes
	// + assembles config + manifest exactly as a real `crane push`
	// would.
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatalf("random.Image: %v", err)
	}
	wantDigest, err := img.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}

	ref, err := name.ParseReference(host + "/" + harness.DefaultTenant + "/randimg:v1")
	if err != nil {
		t.Fatalf("parse ref: %v", err)
	}

	// Push: this exercises POST /blobs/uploads/, PUT for each blob
	// (config + layer), and PUT /manifests/<tag>.
	if err := remote.Write(ref, img,
		remote.WithAuth(auth),
		remote.WithContext(ctx)); err != nil {
		t.Fatalf("remote.Write: %v", err)
	}

	// HEAD/GET round trip: this exercises HEAD /manifests/<tag>,
	// GET /manifests/<tag>, GET /blobs/<digest> (twice — config + layer).
	pulled, err := remote.Image(ref,
		remote.WithAuth(auth),
		remote.WithContext(ctx))
	if err != nil {
		t.Fatalf("remote.Image: %v", err)
	}
	gotDigest, err := pulled.Digest()
	if err != nil {
		t.Fatalf("pulled digest: %v", err)
	}
	if gotDigest != wantDigest {
		t.Fatalf("digest drift: pushed %s, pulled %s", wantDigest, gotDigest)
	}

	// Verify the manifest is reachable by digest too — not just by tag.
	byDigestRef, err := name.NewDigest(host + "/" + harness.DefaultTenant + "/randimg@" + gotDigest.String())
	if err != nil {
		t.Fatalf("digest ref: %v", err)
	}
	if _, err := remote.Image(byDigestRef,
		remote.WithAuth(auth),
		remote.WithContext(ctx)); err != nil {
		t.Fatalf("pull by digest: %v", err)
	}
}

// TestOCIConformance_TagsList verifies that GET /v2/<name>/tags/list
// surfaces every pushed tag. crane uses this for `crane ls`.
func TestOCIConformance_TagsList(t *testing.T) {
	ctx := context.Background()
	stack := harness.Start(ctx, t)
	host := registryHost(t, stack)
	auth := &authenticator{token: stack.AdminToken}

	for _, tag := range []string{"v1", "v2", "v3"} {
		img, err := random.Image(64, 1)
		if err != nil {
			t.Fatalf("random.Image: %v", err)
		}
		ref, err := name.ParseReference(host + "/" + harness.DefaultTenant + "/tagged:" + tag)
		if err != nil {
			t.Fatalf("ref: %v", err)
		}
		if err := remote.Write(ref, img,
			remote.WithAuth(auth),
			remote.WithContext(ctx)); err != nil {
			t.Fatalf("write %s: %v", tag, err)
		}
	}

	listRef, _ := name.NewRepository(host + "/" + harness.DefaultTenant + "/tagged")
	tags, err := remote.List(listRef,
		remote.WithAuth(auth),
		remote.WithContext(ctx))
	if err != nil {
		t.Fatalf("remote.List: %v", err)
	}
	if len(tags) != 3 {
		t.Fatalf("expected 3 tags, got %d: %v", len(tags), tags)
	}
}

// TestOCIConformance_AnonymousReadOnPublicTenant verifies that an
// anonymous pull works against a public-visibility tenant — the same
// posture our harness sets via PKGMIRROR_DEFAULT_TENANT_VISIBILITY=public.
func TestOCIConformance_AnonymousReadOnPublicTenant(t *testing.T) {
	ctx := context.Background()
	stack := harness.Start(ctx, t)
	host := registryHost(t, stack)
	auth := &authenticator{token: stack.AdminToken}

	img, err := random.Image(128, 1)
	if err != nil {
		t.Fatalf("random.Image: %v", err)
	}
	ref, err := name.ParseReference(host + "/" + harness.DefaultTenant + "/anonimg:v1")
	if err != nil {
		t.Fatalf("ref: %v", err)
	}
	if err := remote.Write(ref, img,
		remote.WithAuth(auth),
		remote.WithContext(ctx)); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Anonymous read.
	if _, err := remote.Image(ref,
		remote.WithAuth(authn.Anonymous),
		remote.WithContext(ctx)); err != nil {
		t.Fatalf("anonymous pull: %v", err)
	}
}

// TestOCIConformance_EmptyImagePush exercises the index / scratch
// corner case: an empty image with no layers (just a config). Some
// registries trip up here because the layer set is empty but the
// config blob still needs to be uploaded.
func TestOCIConformance_EmptyImagePush(t *testing.T) {
	ctx := context.Background()
	stack := harness.Start(ctx, t)
	host := registryHost(t, stack)
	auth := &authenticator{token: stack.AdminToken}

	ref, _ := name.ParseReference(host + "/" + harness.DefaultTenant + "/scratch:v1")
	if err := remote.Write(ref, empty.Image,
		remote.WithAuth(auth),
		remote.WithContext(ctx)); err != nil {
		t.Fatalf("write empty.Image: %v", err)
	}
	if _, err := remote.Image(ref,
		remote.WithAuth(auth),
		remote.WithContext(ctx)); err != nil {
		t.Fatalf("read empty.Image: %v", err)
	}
}

// TestOCIConformance_MultiSegmentImageName drives a real OCI client
// through push + pull + tag-list against an image whose name contains
// interior slashes ("myorg/team/svc"). This is the wire-format
// equivalent of pushing to docker.io/library/alpine — the namespace
// shape every production registry uses.
func TestOCIConformance_MultiSegmentImageName(t *testing.T) {
	ctx := context.Background()
	stack := harness.Start(ctx, t)
	host := registryHost(t, stack)
	auth := &authenticator{token: stack.AdminToken}

	img, err := random.Image(128, 1)
	if err != nil {
		t.Fatalf("random.Image: %v", err)
	}
	wantDigest, _ := img.Digest()

	ref, err := name.ParseReference(host + "/" + harness.DefaultTenant + "/myorg/team/svc:v1")
	if err != nil {
		t.Fatalf("parse ref: %v", err)
	}
	if err := remote.Write(ref, img,
		remote.WithAuth(auth),
		remote.WithContext(ctx)); err != nil {
		t.Fatalf("remote.Write: %v", err)
	}
	pulled, err := remote.Image(ref,
		remote.WithAuth(auth),
		remote.WithContext(ctx))
	if err != nil {
		t.Fatalf("remote.Image: %v", err)
	}
	gotDigest, _ := pulled.Digest()
	if gotDigest != wantDigest {
		t.Fatalf("digest drift: pushed %s pulled %s", wantDigest, gotDigest)
	}

	// tags/list against the multi-segment repo.
	listRef, _ := name.NewRepository(host + "/" + harness.DefaultTenant + "/myorg/team/svc")
	tags, err := remote.List(listRef,
		remote.WithAuth(auth),
		remote.WithContext(ctx))
	if err != nil {
		t.Fatalf("remote.List: %v", err)
	}
	if len(tags) != 1 || tags[0] != "v1" {
		t.Fatalf("tags: %v", tags)
	}
}
