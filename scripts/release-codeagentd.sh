#!/usr/bin/env bash
# Release codeagentd standalone binary to GitHub Releases.
#
# Usage:
#   scripts/release-codeagentd.sh <version>
#
# Example:
#   scripts/release-codeagentd.sh 2.0.0-54
#
# Prerequisites:
#   - gh CLI installed and authenticated (`gh auth login`)
#   - codeagentd already built via `go build` or `scripts/build-ios.sh`
#     (the chater repo's build-codeagentd.sh also works)
#
# Steps:
#   1. Build codeagentd for macOS arm64 (if not already built)
#   2. Create a GitHub Release and upload the binary
#   3. Print the Talkify Build Phase snippet
set -euo pipefail

cd "$(dirname "$0")/.."

# --- knobs ---------------------------------------------------------------------
BINARY_NAME="codeagentd"
REPO="tuxi/code-agent-releases"
# -------------------------------------------------------------------------------

VERSION="${1:-}"
if [ -z "$VERSION" ]; then
  echo "error: version required"
  echo "usage: scripts/release-codeagentd.sh <version>"
  echo "example: scripts/release-codeagentd.sh 2.0.0-54"
  exit 1
fi

# --- verify prerequisites ------------------------------------------------------
command -v gh >/dev/null 2>&1 || { echo "error: gh CLI not found (brew install gh)"; exit 1; }
gh auth status >/dev/null 2>&1 || { echo "error: gh not authenticated (run: gh auth login)"; exit 1; }

# --- build ---------------------------------------------------------------------
echo "==> building codeagentd for macOS arm64..."
GOOS=darwin GOARCH=arm64 go build \
    -ldflags="-s -w -X code-agent/internal/buildinfo.Version=${VERSION}" \
    -o "build/${BINARY_NAME}" \
    ./cmd/codeagentd

echo "    done: build/${BINARY_NAME} ($(du -sh "build/${BINARY_NAME}" | cut -f1))"
file "build/${BINARY_NAME}"

# --- quick smoke test ----------------------------------------------------------
echo "==> smoke test..."
TMP_PORT_FILE=$(mktemp)
"build/${BINARY_NAME}" --port-file="${TMP_PORT_FILE}" 127.0.0.1:0 &
DAEMON_PID=$!
sleep 2
if [ -f "${TMP_PORT_FILE}" ]; then
  PORT=$(cat "${TMP_PORT_FILE}")
  if curl -sk "https://127.0.0.1:${PORT}/healthz" 2>/dev/null | grep -q ok; then
    echo "    healthz OK (port ${PORT})"
  elif curl -s "http://127.0.0.1:${PORT}/healthz" 2>/dev/null | grep -q ok; then
    echo "    healthz OK (port ${PORT}, HTTP)"
  else
    echo "    warning: healthz check failed (port ${PORT})"
  fi
else
  echo "    warning: port-file not written — daemon may have crashed"
fi
kill ${DAEMON_PID} 2>/dev/null || true
wait ${DAEMON_PID} 2>/dev/null || true
rm -f "${TMP_PORT_FILE}"

# --- GitHub Release ------------------------------------------------------------
NOTES="Standalone \`codeagentd\` binary for Talkify macOS Direct distribution.

Built with \`go build -ldflags=\"-s -w\" ./cmd/codeagentd\`.

**Talkify Build Phase** (already in \`project.pbxproj\`):
\`\`\`bash
BINARY=\"\${SRCROOT}/build/codeagentd\"
RELEASE_URL=\"https://github.com/${REPO}/releases/latest/download/codeagentd\"
# ... see project.pbxproj for the full script
\`\`\`
"

echo "==> creating GitHub Release ${VERSION}..."
if gh release view "${VERSION}" --repo "${REPO}" >/dev/null 2>&1; then
  echo "    release ${VERSION} already exists — skipping creation"
else
  gh release create "${VERSION}" \
    --repo "${REPO}" \
    --title "codeagentd v${VERSION}" \
    --notes "${NOTES}" \
    "build/${BINARY_NAME}"
fi

# --- result --------------------------------------------------------------------
echo
echo "==> done"
echo "    Release: https://github.com/${REPO}/releases/tag/${VERSION}"
echo "    Download: https://github.com/${REPO}/releases/latest/download/codeagentd"
echo
echo "    The Talkify Xcode Build Phase will auto-download this binary."
echo "    No changes needed in the chater project."
