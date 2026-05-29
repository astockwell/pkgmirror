#!/usr/bin/env bash
# tools/tailwindcss/install.sh — pinned-version installer for the Tailwind
# standalone CLI, used by `make build-css` and `make watch-css`.
#
# Design choices:
#
# - Pins an EXACT upstream version so `make build-css` is reproducible
#   across machines and CI runs. Bumping the version requires also
#   bumping the SHA-256 below, which is the supply-chain hygiene we
#   exist to provide for our users.
#
# - Verifies the downloaded binary's SHA-256 against an embedded
#   expected value. Failure to match is fatal — do NOT proceed with an
#   unverified binary.
#
# - Maps Darwin/Linux × amd64/arm64 to the upstream release naming.
#   Operators on other platforms (FreeBSD, Windows native) build CSS
#   themselves with their own toolchain; pkgmirror ships the committed
#   CSS bundle so a `make build` doesn't require Tailwind at all.
#
# Output path: $BIN_DIR/tailwindcss (defaults to ./bin).

set -euo pipefail

VERSION="${TAILWINDCSS_VERSION:-v4.1.13}"
BIN_DIR="${BIN_DIR:-./bin}"
TARGET="${BIN_DIR}/tailwindcss"

# SHA-256 for each (os, arch) at the pinned VERSION. Update when bumping.
# Empty entries are intentionally left so an operator on an unsupported
# platform sees a clear "not pinned for your platform" error rather
# than silently downloading and trusting an unchecked binary.
#
# To regenerate after a version bump:
#   curl -sL https://github.com/tailwindlabs/tailwindcss/releases/download/$VERSION/tailwindcss-<os>-<arch> | shasum -a 256
declare -A EXPECTED_SHA256
EXPECTED_SHA256["darwin-arm64-v4.1.13"]=""
EXPECTED_SHA256["darwin-x64-v4.1.13"]=""
EXPECTED_SHA256["linux-arm64-v4.1.13"]=""
EXPECTED_SHA256["linux-x64-v4.1.13"]=""

uname_os() {
  case "$(uname -s)" in
    Darwin) echo darwin ;;
    Linux)  echo linux ;;
    *) echo "unsupported OS: $(uname -s)" >&2; exit 1 ;;
  esac
}

uname_arch() {
  case "$(uname -m)" in
    arm64|aarch64) echo arm64 ;;
    x86_64|amd64)  echo x64 ;;
    *) echo "unsupported arch: $(uname -m)" >&2; exit 1 ;;
  esac
}

main() {
  local os arch key url tmp dl_sha expected
  os="$(uname_os)"
  arch="$(uname_arch)"
  key="${os}-${arch}-${VERSION}"

  expected="${EXPECTED_SHA256[$key]:-}"
  if [[ -z "$expected" ]]; then
    cat >&2 <<EOF
tailwindcss installer: no pinned SHA-256 for $key.

This script refuses to download an unverified binary. To proceed, EITHER:

  1. Pin the SHA in tools/tailwindcss/install.sh by running:
       curl -sL "https://github.com/tailwindlabs/tailwindcss/releases/download/${VERSION}/tailwindcss-${os}-${arch}" | shasum -a 256
     then editing EXPECTED_SHA256[$key] to that value, OR

  2. Install tailwindcss manually and put the binary at ${TARGET}, OR

  3. Skip Tailwind entirely. The committed bundle at
     internal/console/static/css/console.css is what gets embedded;
     'make build-css' is only needed when you edit input.css or add a
     new Tailwind class to a template.
EOF
    exit 1
  fi

  url="https://github.com/tailwindlabs/tailwindcss/releases/download/${VERSION}/tailwindcss-${os}-${arch}"
  mkdir -p "$BIN_DIR"
  tmp="$(mktemp -t pkgmirror-tw.XXXXXX)"
  trap 'rm -f "$tmp"' EXIT

  echo "==> Downloading tailwindcss ${VERSION} (${os}/${arch})"
  curl --fail --location --silent --show-error -o "$tmp" "$url"

  dl_sha="$(shasum -a 256 "$tmp" | awk '{print $1}')"
  if [[ "$dl_sha" != "$expected" ]]; then
    echo "tailwindcss installer: SHA-256 mismatch for $key" >&2
    echo "  expected: $expected" >&2
    echo "  got:      $dl_sha" >&2
    exit 1
  fi

  chmod +x "$tmp"
  mv "$tmp" "$TARGET"
  trap - EXIT

  echo "==> Installed: $TARGET"
  "$TARGET" --help | head -2 || true
}

main "$@"
