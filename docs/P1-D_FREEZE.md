# P1-D Freeze Record

Status: frozen
Date: 2026-11-14
Scope: P1-D `inspect_ui_image` to `ui_layout` evidence handoff closeout only

P1-E is not started. `ui_preview` is not implemented or entered. This closeout
does not change the frozen `ui_evidence.v1` or `ui_layout.v1` schemas and does
not add a `ui_layout` activity to AgentKit.

## Final repository commits

| Repository | Local branch | Commit |
| --- | --- | --- |
| code-agent | `main` | `74ea840bb0995e237e746417a9a6c627b8e4adb8` |
| ClientToolsKit | `main` | `4b79e6e9cb6d978ed71b958ab37bd5ea2d791d2e` |
| AgentKit | `main` | `70335573094aa445a37ca538ad1e1a1aefbf8bc0` |
| Talkify | `main` | `380fc01e39a662c10960bed03973ac8db014cfe3` |

code-agent `74ea840` is the P1-D code checkpoint. This freeze record is
committed on top of it. The checkpoint branch `p1d/uid-evidence-handoff` was
merged into local `main` as a fast-forward. Nothing was pushed.

Talkify carries a pre-existing uncommitted local modification in
`Talkify/TalkifyIOS/TalkifyRootViewIOS.swift` (the coding-tasks top inset is
commented out). It predates this closeout and was neither changed nor reverted
here. This closeout did not modify AgentKit source.

## Acceptance

### Live model matrix

- Independent acceptance thread: 8/8 live sessions passed on
  `qwen/qwen3.8-flash`.
- This closeout did not rerun the live model matrix. Live UI screenshots taken
  during those sessions are not execution evidence for P1-D; the executable
  evidence is the deterministic client/runtime contract below.

### Automated checks

Passed in this closeout:

```text
go build ./...
go vet ./internal/tools/uilayout/... ./internal/uicontract/...
go test -count=1 ./internal/tools/uilayout/... ./internal/uicontract/...
go test -count=1 ./internal/runtime/ -run 'TestServeRunBuilderAppliesCapabilityBindingsToClientTools|TestCodingTools'
git show --check 3e6bf0e..74ea840
git diff --check 3e6bf0e..74ea840
git diff --check
```

Positive and negative coverage:

- Layout schema, canonical golden output, overlay rendering, revision
  persistence and revision reuse.
- Evidence digest: matching, mismatched, and declared without evidence.
- `evidence_path`: missing file, directory, absolute path, symlink escape,
  tampered bytes, missing `evidence_sha256`, source asset ID mismatch, canvas
  mismatch, and unknown evidence IDs.
- Revision reuse rejects symlinked or tampered `metadata.json`, `layout.json`,
  and `overlay.png`.
- Invalid input: unknown fields, out-of-bounds bbox, zero-area bbox, duplicate
  node and invariant IDs, unknown policy node, path traversal, and invalid
  artifact ID.
- ClientToolsKit: deterministic evidence path, digest, and asset identity;
  byte-stable repeats; rewrite after evidence content changes; evidence
  directory symlink escape; crop determinism and source linkage; traversal,
  absolute-path, and symlink rejection; degraded Vision fallback.

### Builds

- Talkify `Talkify-MacDirect` macOS build: passed.
- Talkify `Talkify` iOS Simulator build: passed.

Both builds used the local `../ClientToolsKit` and `../AgentKit` packages with
signing disabled and offline package resolution:

```text
xcodebuild -project Talkify.xcodeproj -scheme Talkify-MacDirect \
  -destination 'platform=macOS,arch=arm64' \
  -derivedDataPath /tmp/p1d-dd-mac \
  -disableAutomaticPackageResolution -skipPackageUpdates -skipMacroValidation \
  CODE_SIGNING_ALLOWED=NO CODE_SIGNING_REQUIRED=NO build

xcodebuild -project Talkify.xcodeproj -scheme Talkify \
  -destination 'generic/platform=iOS Simulator' \
  -derivedDataPath /tmp/p1d-dd-ios \
  -disableAutomaticPackageResolution -skipPackageUpdates -skipMacroValidation \
  CODE_SIGNING_ALLOWED=NO CODE_SIGNING_REQUIRED=NO build
```

Do not pass `-configuration` to the MacDirect scheme: it overrides the
scheme's `Debug-MacDirect` configuration and fails the `TALKIFY_MAC_DIRECT`
compile-time distribution check. The iOS Simulator build used its scheme's
`Debug` configuration.

### Swift test environment note

SwiftPM `swift test` for ClientToolsKit could not run on this host: the Xcode
CoreML compiler sandbox denied reading
`yolov8n.mlpackage/Data/com.apple.CoreML/weights/weight.bin` before test
execution. This is the same environment failure recorded in P1-C. All
ClientToolsKit sources and the two updated test files were typechecked against
the prebuilt modules, and a standalone harness covering the evidence path
passed. The Talkify Xcode builds in this closeout did compile the same CoreML
resource successfully; only the SwiftPM `swift test` path is blocked. Full
XCTest remains to be rerun in an environment where that SwiftPM path works.

## Evidence handoff chain

1. `inspect_ui_image` writes the canonical `ui_evidence.v1` document to
   `ui-evidence/<sourceHash>/evidence.json`, where `<sourceHash>` is the
   16-hex prefix of the source SHA-256.
2. The tool content returns `evidence_path`, `evidence_sha256` (SHA-256 of the
   file bytes), and the evidence source `asset_id`. The structured output stays
   a pure `ui_evidence.v1` document; evidence identity is carried by a
   `asset_role=evidence` file asset.
3. Identical evidence bytes are not rewritten. Changed evidence is atomically
   replaced, so a later `ui_layout` call cannot bind to a stale file.
4. `ui_layout` accepts `evidence_path` or inline `evidence`, never both. With
   `evidence_path` it requires `source.evidence_schema=ui_evidence.v1` and a
   non-empty `source.evidence_sha256` matching the file bytes.
5. `ui_layout` also requires the layout source asset ID and source SHA to match
   the evidence, validates the evidence canvas against the source image, and
   resolves every node and assumption `evidence_ids` reference against the
   evidence elements.

## Revision idempotency and overlay

- The canonical layout is stored under
  `.codeagent/assets/ui-layout/<artifactID>/revision-NNNNNN/` with
  `layout.json`, `overlay.png`, and `metadata.json`.
- The default artifact ID is `layout_<sourceSHA[:16]>`.
- An identical layout, source image, evidence digest, canvas, and overlay
  reuses the existing revision. Tampered revision artifacts are not reused and
  force a new revision.
- The overlay is a PNG bounded to 2048 pixels on the long edge. Observed nodes
  use green borders and inferred nodes use amber borders. It is returned as a
  `layout_overlay` image asset.

## Security boundaries

- `source_path` and `evidence_path` are workspace-relative, forward-slash
  paths. Absolute paths, traversal, control characters, and managed-worktree
  paths are rejected.
- Existing path components are resolved through symlinks before containment is
  checked. The same longest-existing-prefix rule is applied by the client
  `ToolWorkspace`, closing an intermediate-symlink escape when the final file
  does not exist yet.
- Evidence files must be regular files. The asset root is canonicalized and a
  symlinked asset root is rejected.
- No absolute paths are returned, no image bytes are stored in JSON, and no
  user files are deleted.
- OCR text, accessibility labels, and DOM text remain untrusted data and are
  never executed as instructions.

## Known limitations

- The live matrix used `qwen/qwen3.8-flash`.
- DeepSeek multi-step client-tool continuation is deferred to P2.
- AgentKit has no dedicated `ui_layout` activity.
- Live UI screenshots are not execution evidence for this P1-D closeout.
- `ui_preview` is not entered.
