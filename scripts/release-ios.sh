#!/usr/bin/env bash
# Release CodeAgentRuntime.xcframework to GitHub Releases.
#
# Usage:
#   scripts/release-ios.sh <version>
#
# Example:
#   scripts/release-ios.sh 0.2.0
#
# Prerequisites:
#   - gh CLI installed and authenticated (`gh auth login`)
#   - xcframework already built via `scripts/build-ios.sh`
#
# Steps:
#   1. Check build/CodeAgentRuntime.xcframework exists
#   2. Zip it
#   3. Compute SPM checksum
#   4. Create a GitHub Release and upload the zip
#   5. Print the .binaryTarget(url:checksum:) snippet for AgentKit's Package.swift
set -euo pipefail

cd "$(dirname "$0")/.."

# --- knobs ---------------------------------------------------------------------
FRAMEWORK_NAME="CodeAgentRuntime"
ZIP_NAME="${FRAMEWORK_NAME}.xcframework.zip"
SRC_DIR="build/${FRAMEWORK_NAME}.xcframework"
REPO="tuxi/code-agent"
# -------------------------------------------------------------------------------

VERSION="${1:-}"
if [ -z "$VERSION" ]; then
  echo "error: version required"
  echo "usage: scripts/release-ios.sh <version>"
  echo "example: scripts/release-ios.sh 0.2.0"
  exit 1
fi

# --- verify prerequisites ------------------------------------------------------
command -v gh >/dev/null 2>&1 || { echo "error: gh CLI not found (brew install gh)"; exit 1; }
gh auth status >/dev/null 2>&1 || { echo "error: gh not authenticated (run: gh auth login)"; exit 1; }

if [ ! -d "${SRC_DIR}" ]; then
  echo "error: ${SRC_DIR} not found"
  echo "       run scripts/build-ios.sh first"
  exit 1
fi

# --- zip -----------------------------------------------------------------------
echo "==> packaging ${ZIP_NAME}..."
rm -f "build/${ZIP_NAME}"
cd build
zip -r -y "${ZIP_NAME}" "${FRAMEWORK_NAME}.xcframework/" >/dev/null
cd ..
echo "    done: build/${ZIP_NAME} ($(du -sh "build/${ZIP_NAME}" | cut -f1))"

# --- checksum ------------------------------------------------------------------
echo "==> computing SPM checksum..."
CHECKSUM=$(swift package compute-checksum "build/${ZIP_NAME}")
echo "    ${CHECKSUM}"

# --- GitHub Release ------------------------------------------------------------
NOTES="CodeAgentRuntime.xcframework — built with \`gomobile bind -target=ios,iossimulator\`

**Slices:**
- \`ios-arm64\` — device
- \`ios-arm64_x86_64-simulator\` — simulator

**AgentKit Package.swift:**
\`\`\`swift
.binaryTarget(
    name: \"CodeAgentRuntime\",
    url: \"https://github.com/${REPO}/releases/download/${VERSION}/CodeAgentRuntime.xcframework.zip\",
    checksum: \"${CHECKSUM}\"
)
\`\`\`"

echo "==> creating GitHub Release ${VERSION}..."
if gh release view "${VERSION}" --repo "${REPO}" >/dev/null 2>&1; then
  echo "    release ${VERSION} already exists — skipping creation"
else
  gh release create "${VERSION}" \
    --repo "${REPO}" \
    --title "v${VERSION}" \
    --notes "${NOTES}" \
    "build/${ZIP_NAME}"
fi

# --- result --------------------------------------------------------------------
echo
echo "==> done"
echo "    Release: https://github.com/${REPO}/releases/tag/${VERSION}"
echo
echo "    Update AgentKit/Package.swift:"
echo "    ┌─────────────────────────────────────────────────────┐"
echo "    │ .binaryTarget(                                     │"
echo "    │     name: \"${FRAMEWORK_NAME}\",                     │"
echo "    │     url: \"https://github.com/${REPO}/releases/download/${VERSION}/${ZIP_NAME}\","
echo "    │     checksum: \"${CHECKSUM}\"                       │"
echo "    │ )                                                  │"
echo "    └─────────────────────────────────────────────────────┘"
echo
echo "    Then commit & push AgentKit:"
echo "      cd ../AgentKit"
echo "      # edit Package.swift with the snippet above"
echo "      git add Package.swift"
echo "      git commit -m 'chore: bump CodeAgentRuntime to ${VERSION}'"
echo "      git push origin main"
