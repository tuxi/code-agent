# R4a Real Backend Preview Turn

`codeagent-preview-r4a` executes a deterministic but real Backend turn through
the production agent loop. It does not use the R3 fixture server or frozen
artifact responses.

The turn uses:

- A durable SQLite session and event stream.
- The built-in server-side `ui_preview` tool.
- Capability binding `ui-adaptation`.
- The real typed `ToolResult.Previews` side-channel.
- The real preview service, immutable revision store, JCS artifact hash, event
  persistence, replay, viewer materializer, and in-process viewer/asset
  resolvers.
- A generated 1200x800 PNG fallback.
- A scripted model provider to make the acceptance run reproducible.

## Run

From the repository root:

```sh
go run ./cmd/codeagent-preview-r4a
```

The command creates isolated temporary workspace, state, and viewer directories,
runs the real turn, verifies the live and replayed `preview_artifact`, verifies
viewer/asset bytes, writes a private trusted viewer file, and opens it with the
system browser.

Useful flags:

```text
-workspace              workspace root
-state-dir              SQLite/runtime state directory
-viewer-dir             private materialized viewer directory
-viewer-html-file       viewer HTML fragment file
-fallback-file          PNG/JPEG/WebP fallback file
-title                  preview title
-summary                preview summary
-artifact-key           runtime namespace key
-preferred-aspect-ratio preferred aspect ratio
-min-height             preferred minimum height
-turn-prompt            dedicated acceptance turn prompt
-final-answer           dedicated completion text
-open=true              open the verified viewer
```

File-backed example:

```sh
go run ./cmd/codeagent-preview-r4a -open=false   -viewer-html-file /path/to/preview.html   -fallback-file /path/to/fallback.png   -title "DreamLog Duo Home"   -summary "File-backed preview"   -artifact-key ui-adaptation:dreamlog-duo
```

Viewer HTML is read and size/UTF-8 validated before execution. External fallback
images are copied byte-for-byte into a controlled workspace assets directory
before the real `ui_preview` tool runs.

For non-GUI CI:

```sh
go run ./cmd/codeagent-preview-r4a -open=false
```

The final JSON record reports the session, turn, capability, producer, artifact,
revision, event sequence, viewer/asset URLs, verified hashes, and viewer path.

## Verify

```sh
go test ./cmd/codeagent-preview-r4a ./internal/tools/preview -count=1
```
