#!/usr/bin/env bash
# Release the codeagent CLI binary to GitHub Releases on the main repo.
#
# Primary consumer: macOS Recovery Mode (docs/recovery-mode.md) — boot into
# recovery, curl the binary, run the TUI to diagnose a Mac that won't boot.
#
# Usage:
#   scripts/release-codeagent.sh <version>
#
# Example:
#   scripts/release-codeagent.sh v1.6.4        # tag v1.6.4 (created at HEAD if missing)
#
# Prerequisites:
#   - gh CLI installed and authenticated (`gh auth login`)
#
# Deliberately NOT published to tuxi/code-agent-releases: that repo's
# `releases/latest/download/codeagentd` link is consumed by the Talkify Xcode
# build phase, and a CLI-only release would become "latest" and 404 it.
set -euo pipefail

cd "$(dirname "$0")/.."

# --- knobs ---------------------------------------------------------------------
BINARY_NAME="codeagent"
REPO="tuxi/code-agent"
# Pure-Go build on purpose: CGO_ENABLED=0 keeps the binary free of local dylib
# assumptions so it runs inside the minimal recoveryOS userland.
# -------------------------------------------------------------------------------

VERSION="${1:-}"
if [ -z "$VERSION" ]; then
  echo "error: version required"
  echo "usage: scripts/release-codeagent.sh <version>"
  echo "example: scripts/release-codeagent.sh v1.6.4"
  exit 1
fi
# Normalize to a v-prefixed tag (main repo tags are v1.x.y).
case "$VERSION" in
  v*) ;;
  *) VERSION="v${VERSION}" ;;
esac

# --- verify prerequisites ------------------------------------------------------
command -v gh >/dev/null 2>&1 || { echo "error: gh CLI not found (brew install gh)"; exit 1; }
gh auth status >/dev/null 2>&1 || { echo "error: gh not authenticated (run: gh auth login)"; exit 1; }

# --- build ----------------------------------------------------------------------
# Universal binary (arm64 + x86_64): recoveryOS has no Rosetta, and the Mac's
# arch is unknown until it boots. CGO_ENABLED=0 cross-compiles without clang.
echo "==> building ${BINARY_NAME} for macOS universal (arm64 + x86_64), version ${VERSION}..."
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build \
    -ldflags="-s -w -X code-agent/internal/buildinfo.Version=${VERSION}" \
    -o "build/${BINARY_NAME}.arm64" \
    ./cmd/codeagent

CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build \
    -ldflags="-s -w -X code-agent/internal/buildinfo.Version=${VERSION}" \
    -o "build/${BINARY_NAME}.x86_64" \
    ./cmd/codeagent

lipo -create -output "build/${BINARY_NAME}" "build/${BINARY_NAME}.arm64" "build/${BINARY_NAME}.x86_64"
rm -f "build/${BINARY_NAME}.arm64" "build/${BINARY_NAME}.x86_64"

echo "    done: build/${BINARY_NAME} ($(du -sh "build/${BINARY_NAME}" | cut -f1))"
lipo -info "build/${BINARY_NAME}"

# Bare filename (not build/codeagent) so `shasum -a 256 -c codeagent.sha256`
# works against the downloaded asset, whose name is `codeagent`.
(cd build && shasum -a 256 "${BINARY_NAME}" > "${BINARY_NAME}.sha256")

# --- smoke test -----------------------------------------------------------------
# The TUI needs a TTY, so smoke-test a read-only entry point instead: `sessions`
# only opens the store (no model, no API key). It is NOT trust-exempt — the trust
# gate at cmd/codeagent/main.go runs before command dispatch — so pass --trust,
# the same flag the recovery runbook uses. Redirecting HOME to a temp dir exercises
# the fresh-$HOME store-creation path recovery mode depends on.
echo "==> smoke test..."
SMOKE_DIR=$(mktemp -d)
trap 'rm -rf "${SMOKE_DIR}"' EXIT
if HOME="${SMOKE_DIR}/home" "build/${BINARY_NAME}" --trust sessions > "${SMOKE_DIR}/out" 2>&1; then
  echo "    sessions OK: $(tail -1 "${SMOKE_DIR}/out")"
else
  echo "error: smoke test failed:" >&2
  cat "${SMOKE_DIR}/out" >&2
  exit 1
fi

# --- GitHub Release -------------------------------------------------------------
NOTES="Standalone \`codeagent\` CLI binary (universal: Apple Silicon + Intel).

Built with \`CGO_ENABLED=0\` so it runs inside macOS Recovery Mode's minimal userland.

**macOS Recovery quick start** (in recovery Terminal, after mounting a writable
volume such as the Data volume):

\`\`\`sh
curl -LO https://github.com/${REPO}/releases/latest/download/codeagent
chmod +x codeagent
export DEEPSEEK_API_KEY=sk-...   # or source a key file on the mounted volume
HOME=\"\$PWD/home\" ./codeagent --trust
\`\`\`

Full runbook: [docs/recovery-mode.md](https://github.com/${REPO}/blob/main/docs/recovery-mode.md)
"

echo "==> creating GitHub Release ${VERSION} on ${REPO}..."
if gh release view "${VERSION}" --repo "${REPO}" >/dev/null 2>&1; then
  echo "    release ${VERSION} already exists — uploading assets only"
  gh release upload "${VERSION}" --repo "${REPO}" --clobber "build/${BINARY_NAME}" "build/${BINARY_NAME}.sha256"
else
  gh release create "${VERSION}" \
    --repo "${REPO}" \
    --title "${BINARY_NAME} ${VERSION}" \
    --notes "${NOTES}" \
    "build/${BINARY_NAME}" "build/${BINARY_NAME}.sha256"
fi

# --- result ---------------------------------------------------------------------
echo
echo "==> done"
echo "    Release:  https://github.com/${REPO}/releases/tag/${VERSION}"
echo "    Download: https://github.com/${REPO}/releases/latest/download/codeagent"
echo
echo "    Recovery-mode one-liner (run inside recovery Terminal on a mounted volume):"
echo "      curl -LO https://github.com/${REPO}/releases/latest/download/codeagent && chmod +x codeagent && HOME=\"\$PWD/home\" ./codeagent --trust"
