#!/usr/bin/env bash
# Build CodeAgentRuntime.xcframework from ./mobile for embedding the codeagent runtime
# inside the iOS/macOS app. Produces a verified .xcframework (the only binary
# format SPM's .binaryTarget accepts) with a correct module name and a sane
# MinimumOSVersion — directly consumable, no manual post-processing.
#
# A current gomobile (`-target=ios,iossimulator`) emits a proper .xcframework
# containing both the device and simulator slices, so a single bind is enough.
# If an outdated gomobile emits something else, the script dumps what it produced
# and tells you to update — rather than silently assembling a broken artifact.
#
# Usage:  scripts/build-ios.sh [output-dir]
#   output-dir defaults to ./build  ->  ./build/CodeAgentRuntime.xcframework
set -euo pipefail

cd "$(dirname "$0")/.."

# --- knobs ---------------------------------------------------------------------
FRAMEWORK_NAME="CodeAgentRuntime"   # => Swift module name: `import CodeAgentRuntime`
IOS_MIN="15.0"               # MinimumOSVersion written into every inner Info.plist
PKG="./mobile"               # Go package bound (symbol prefix is `Mobile`, its package name)
OUT_DIR="${1:-build}"
# Skills shipped with the iOS app. These SKILL.md files are user-level (global)
# skills, copied into the Application Support directory on first launch so they
# are available to every workspace. The user can add their own skills there later.
# List skill directory names (not full paths).
BUNDLED_SKILLS=(review-change verify-change skill-creator)
SKILLS_SRC="./skills"
# -------------------------------------------------------------------------------

OUT="${OUT_DIR}/${FRAMEWORK_NAME}.xcframework"
export PATH="$(go env GOPATH)/bin:${PATH}"

command -v xcodebuild >/dev/null 2>&1 || { echo "error: xcodebuild not found (install Xcode)"; exit 1; }

if ! command -v gomobile >/dev/null 2>&1; then
  echo "==> installing gomobile + gobind"
  go install golang.org/x/mobile/cmd/gomobile@latest
  go install golang.org/x/mobile/cmd/gobind@latest
fi

echo "==> gomobile init"
gomobile init

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo "==> gomobile bind (ios + iossimulator)"
gomobile bind \
  -target=ios,iossimulator \
  -iosversion="${IOS_MIN}" \
  -o "${WORK}/${FRAMEWORK_NAME}.xcframework" \
  "${PKG}"

PRODUCED="${WORK}/${FRAMEWORK_NAME}.xcframework"
if [ ! -e "${PRODUCED}/Info.plist" ]; then
  echo "error: gomobile did not produce a valid .xcframework."
  echo "       it produced (in ${WORK}):"
  find "${WORK}" -maxdepth 3 -print
  echo
  echo "       Your gomobile is likely outdated. Update it and retry:"
  echo "         go install golang.org/x/mobile/cmd/gomobile@latest"
  echo "         go install golang.org/x/mobile/cmd/gobind@latest"
  echo "         gomobile init"
  exit 1
fi

echo "==> installing -> ${OUT}"
mkdir -p "${OUT_DIR}"
rm -rf "${OUT}"
mv "${PRODUCED}" "${OUT}"

# Belt-and-suspenders: force MinimumOSVersion on every inner Info.plist, in case a
# gomobile build wrote a bogus value (e.g. 100.0).
echo "==> normalizing MinimumOSVersion -> ${IOS_MIN}"
while IFS= read -r -d '' plist; do
  /usr/libexec/PlistBuddy -c "Set :MinimumOSVersion ${IOS_MIN}" "${plist}" 2>/dev/null \
    || /usr/libexec/PlistBuddy -c "Add :MinimumOSVersion string ${IOS_MIN}" "${plist}" 2>/dev/null \
    || true
done < <(find "${OUT}" -name Info.plist -print0)

# ---- package skills alongside the xcframework -----------------------------------
echo "==> packaging skills"
SKILLS_OUT="${OUT_DIR}/skills"
rm -rf "${SKILLS_OUT}"
mkdir -p "${SKILLS_OUT}"
copied=0
skipped=0
for skill in "${BUNDLED_SKILLS[@]}"; do
  src="${SKILLS_SRC}/${skill}"
  if [ -f "${src}/SKILL.md" ]; then
    cp -R "${src}" "${SKILLS_OUT}/"
    echo "    bundled: ${skill}"
    ((copied++)) || true
  else
    echo "    skipped: ${skill} (no SKILL.md at ${src})"
    ((skipped++)) || true
  fi
done
echo "    skills: ${copied} bundled, ${skipped} skipped"

echo "==> verifying"
echo "    slices:"
find "${OUT}" -maxdepth 1 -mindepth 1 -type d -exec basename {} \; | sed 's/^/      /'
echo "    frameworks:"
find "${OUT}" -maxdepth 2 -name "*.framework" | sed 's/^/      /'

echo
echo "==> done: ${OUT}"
echo "    Swift:     import ${FRAMEWORK_NAME}        // symbols prefixed Mobile* (Go package name)"
echo "    SPM:       .binaryTarget(name: \"${FRAMEWORK_NAME}\", path: \"${OUT}\")"
echo "    resources: .copy(\"${SKILLS_OUT}\")              // skills → copy into Application Support/skills (global/user-level)"
