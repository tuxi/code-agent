# Agent Wire Protocol v1.3 — Tool Finished Assets

> Status: Phase 1 contract draft.
> Builds on [agent-wire-v1.md](agent-wire-v1.md),
> [v1.1 client tool execution](agent-wire-v1.1-client-tool-execution.md), and
> [v1.2 lifecycle/replay](agent-wire-v1.2-lifecycle-suspend-resume.md).

## 0. Principle

`observation` remains the canonical text transcript. It is still what the model
sees, what users copy, and what audits preserve.

`output` and `assets` are a structured side-channel on the same event. They make
tool results clickable without asking clients to infer files, symbols, or images
from free-form text. The side-channel belongs on `agent.Event`, not only on the
wire mapper, so live WebSocket events and replayed `/events` frames share one
source of truth.

Phase 1 only adds optional fields. Old clients ignore them. New clients prefer
them and keep regex/path parsing as fallback only.

## 1. Workspace Anchor

Conversation list/detail responses should include a workspace object while
keeping existing `workspace_path` fields for compatibility.

```json
{
  "id": "20260701-154050-dc4fbff4",
  "workspace_path": "/Users/xiaoyuan/Documents/work/git/AgentKit",
  "workspace": {
    "id": "agentkit-local",
    "name": "AgentKit",
    "root_path": "/Users/xiaoyuan/Documents/work/git/AgentKit",
    "runtime_cwd": "/Users/xiaoyuan/Documents/work/git/AgentKit",
    "display_path": "AgentKit",
    "kind": "local"
  }
}
```

Clients resolve `workspace_relative_path` against `workspace.root_path` or
`workspace.runtime_cwd`. iOS may not read that path directly, but it can still
scope asset identity and request preview/content from the runtime later.

`workspace://` URIs must percent-encode each path segment. `/` remains a path
separator; spaces, non-ASCII characters, `#`, `?`, and other reserved characters
inside a segment are encoded. Clients should prefer `workspace_relative_path`
for local path resolution and treat `uri` as a portable identifier/display link.

## 2. Event Fields

`tool_finished` gains two optional fields:

```json
{
  "kind": "tool_finished",
  "call_id": "call_02",
  "tool_name": "grep",
  "observation": "...raw transcript...",
  "output": {
    "kind": "search_results",
    "items": []
  },
  "assets": []
}
```

`output` is tool-specific structured data. `assets` is the normalized clickable
index that client UI consumes. Items in `output` may refer to asset ids, but a
client should not need to understand every `output.kind` before it can make basic
links clickable.

Client-executed tools use the same fields on inbound
`agent_input.kind = "tool_result"`. The runtime stores them on the resulting
`agent.Event`, so client-originated tool assets and server-originated tool assets
share the same live/replay behavior.

## 3. Asset Ref

```json
{
  "id": "asset_turn_1_call_02_001_a1b2c3d4",
  "kind": "file_location",
  "uri": "workspace://agentkit-local/Sources/App.swift#L49",
  "display_name": "ConversationState.swift:49",
  "workspace_id": "agentkit-local",
  "workspace_relative_path": "Sources/AgentKit/Features/Conversation/Models/ConversationState.swift",
  "absolute_path": "/Users/xiaoyuan/Documents/work/git/AgentKit/Sources/AgentKit/Features/Conversation/Models/ConversationState.swift",
  "range": { "start_line": 49, "start_column": 1 },
  "preview": "public var streamingText: String = \"\"",
  "mime_type": "text/x-swift",
  "metadata": { "language": "swift" },
  "source_turn_id": "turn_1",
  "source_call_id": "call_02"
}
```

Required fields: `id`, `kind`, `source_turn_id`. `source_call_id` is required for
tool-derived assets. Path fields are optional for non-file assets.

`id` must be stable across live/replay. Recommended shape:

```text
asset_<turn_id>_<call_id>_<ordinal>_<short_hash>
```

The hash should be derived from canonical asset content such as
`kind + workspace_relative_path + range + uri`.

Initial `kind` values:

| kind | Meaning |
|---|---|
| `file` | A workspace file. |
| `file_location` | A file plus line/column/range. |
| `directory` | A workspace directory. |
| `url` | External URL. |
| `symbol` | Language symbol, usually backed by a file location. |
| `search_result` | Search result group or hit. |
| `diff` | Patch/diff asset. |
| `terminal` | Terminal/log output. |
| `markdown` | Markdown document/snippet. |
| `image` | Image asset. |
| `video` | Video asset. |
| `audio` | Audio asset. |
| `pdf` | PDF document asset. |

In Phase 1, `output.items[].path` is workspace-relative unless the specific
`output.kind` documents otherwise. `assets[]` order should match
`output.items[]` order when there is a one-to-one item/asset relationship, and
`asset_id` is the explicit join key.

## 4. Phase 1 Tools

### `grep`

`output.kind = "search_results"`. Each hit should carry path, optional absolute
path, line, column, preview, and `asset_id`. Each hit should also have a matching
`assets[]` entry with `kind = "file_location"`.

Fixture: [fixtures/tool-assets/tool_finished_grep_assets.json](fixtures/tool-assets/tool_finished_grep_assets.json).

### `read_file`

`output.kind = "file"`. The event should expose one `file` asset for the file
that was read. If the tool result is line-numbered, `output.line_count` (the
number of displayed lines, not necessarily total file lines) and
`output.display_range` may be included.

Fixture: [fixtures/tool-assets/tool_finished_read_file_assets.json](fixtures/tool-assets/tool_finished_read_file_assets.json).

### `list_files`

`output.kind = "directory_listing"`. Each listed entry should carry path,
optional absolute path, display name, `kind = "file"` or `kind = "directory"`,
and `asset_id`. Each entry should also have a matching `assets[]` entry. The
plain `observation` remains the newline-separated listing and keeps directory
trailing slashes for readability.

### `project_graph`

`output.kind = "symbols"` or `output.kind = "references"`, depending on action.
Symbol/reference items should include symbol name, symbol kind, file path, line,
column, preview, and an `asset_id`. Assets use `kind = "symbol"` when the primary
thing is a symbol, or `kind = "file_location"` for plain references.

Fixture: [fixtures/tool-assets/tool_finished_project_graph_assets.json](fixtures/tool-assets/tool_finished_project_graph_assets.json).

### MCP non-text content

MCP protocol compatibility rule: code-agent does not change, extend, or emit a
custom MCP tool-result schema. It consumes standard MCP `CallToolResult.content`
blocks from the MCP SDK. `output.kind = "mcp_content"` is a code-agent internal
derived output kind on `agent.Event`; it is not an MCP spec field and is not MCP
`structuredContent`.

MCP text blocks remain part of `observation`. MCP image/audio/resource blocks
still render as placeholders in `observation`, but Phase 1 preserves derived UI
metadata through `output.kind = "mcp_content"` and `assets[]`.

Binary payloads are not transferred in Phase 1. Image/audio assets carry a
runtime-local `mcp://...` URI plus MIME type and byte count metadata. Resource
links and embedded resources carry their original URI.

MCP-derived assets should include `metadata.source = "mcp"` and
`metadata.mcp_type` with the original MCP content block kind, such as `image`,
`audio`, `resource_link`, or `embedded_resource`.

## 5. Assistant Text Annotations

`turn_finished` may include `text_annotations`, linking ranges in assistant
Markdown back to structured assets discovered earlier in the same turn.

```json
{
  "kind": "turn_finished",
  "text": "Open `App.swift:5` for the important line.",
  "text_annotations": [
    {
      "asset_id": "asset_turn_7_call_grep_001_7156f5c8",
      "kind": "file_location",
      "text": "App.swift:5",
      "start_byte": 6,
      "end_byte": 17,
      "start_utf16": 6,
      "end_utf16": 17,
      "source_turn_id": "turn_7",
      "source_call_id": "call_grep"
    }
  ]
}
```

The assistant `text` is unchanged. Annotations are a UI side-channel and should
not be copied back into model history. `asset_id` joins against the client's
existing `AssetIndex`; clients may use `text` to verify the range before
rendering.

Offsets:

- `start_byte` / `end_byte` are UTF-8 byte offsets in `text`.
- `start_utf16` / `end_utf16` are UTF-16 code-unit offsets for Swift/NSRange
  rendering.
- End offsets are exclusive.

Phase 1 annotation is intentionally conservative. The runtime annotates exact
file/path references derived from structured assets, such as
`workspace_relative_path`, `display_name`, and `path:line`. It does not broadly
annotate plain symbol names in prose.

Line mention annotations are also allowed when they can be resolved safely to a
known `file_location` asset. Supported forms include:

- `第 109 行`
- `第109行`
- `line 109`
- `L109`
- `109 行`

To avoid turning ordinary numbers into links, the runtime should only annotate a
line mention when one of these is true:

- the current turn's structured assets reference exactly one file; or
- the assistant answer has a nearby annotation for the same file/path.

The annotation still points at the concrete line asset via `asset_id`; no new
protocol shape is needed.

Markdown table line-number cells are treated as line mentions when the table
header contains a line column, such as `行`, `行号`, or `line`. If the same row
contains a file/path column, the runtime resolves the line number against that
file; a cell such as `同上` may inherit the previous row's file. A line cell may
contain multiple comma-separated line numbers, such as `16, 70, 119`, and each
number may become its own annotation. Shortened display paths with `...` may be
resolved by basename only when that basename is unique among the turn's
structured assets. Numeric cells in tables without a line-number header are not
annotated.

## 6. Out of Scope For Phase 1

- Runtime `openAsset`.
- Full MCP image/audio/resource preview or binary transfer.
- Large file transfer.
- Streaming/progress assets for long-running tools.

These can be layered on once live/replay parity for `tool_finished.assets` is
proven.

## 7. Runtime Asset Read API

The server may expose a minimal read API derived from persisted `agent.Event`
assets. This does not require a separate asset store: the runtime replays the
conversation event log, finds the requested `asset_id`, and resolves file assets
against the conversation workspace anchor.

```http
GET /v1/conversations/{conversation_id}/assets/{asset_id}/preview
GET /v1/conversations/{conversation_id}/assets/{asset_id}/content
GET /v1/conversations/{conversation_id}/assets/{asset_id}/blob
GET /v1/conversations/{conversation_id}/assets/{asset_id}/thumbnail?max_px=512
```

`preview` returns JSON with the asset, small text content when available, and
metadata for non-file or non-text assets. For `file_location`, preview may return
a line window around `range.start_line`. For MCP-derived image/audio/resource
assets, preview returns placeholder/metadata only in Phase 1.

For local non-text workspace assets, `preview` returns metadata instead of bytes.
The response includes `mime_type`, `size_bytes`, and `metadata.media_url`
pointing at the `blob` endpoint. `metadata.thumbnail_url` may also be present;
clients should treat it as optional.

`content` returns JSON text content for workspace-scoped text files. It rejects
paths outside the workspace, directories, and non-text assets. Content is capped
and reports `truncated = true` when the file exceeds the Phase 1 limit.

`blob` returns the raw workspace file bytes for local assets. It is intended for
images, video, audio, PDFs, and other binary files. The server resolves the asset
through the persisted event log, enforces that the path stays inside the
conversation workspace, rejects directories, sets `Content-Type`, `Last-Modified`
and `ETag`, and streams with HTTP range support (`Accept-Ranges: bytes`,
`206 Partial Content`, and `Content-Range` for valid `Range` requests). Metadata-
only assets such as MCP resources without local bytes return `415 Unsupported
Media Type` until a binary fetch/store layer exists.

`thumbnail` is reserved for lightweight media previews. Phase 1 may return
`501 Not Implemented`; clients should fall back to `blob`.

These endpoints are intentionally not `openAsset`: clients still own platform UI
behavior such as macOS Inspector or iOS sheet presentation.

## 8. Client Behavior

Clients should:

1. Decode `output`, `assets`, and `text_annotations` on every event, especially
   `tool_finished` and `turn_finished`.
2. Build an `AssetIndex` keyed by asset id and scoped by conversation/workspace.
3. Render tool cards from structured assets when available.
4. Fall back to conservative regex/path parsing only when no structured asset is
   present.
5. Treat `event_id` as transport identity only. Do not use it for asset ids.

Click handling for Phase 1:

- macOS: open file/location in Inspector.
- iOS: open file/location in a preview sheet through the runtime/workspace layer.
- Unknown asset kind: show a generic preview using `display_name`, `preview`, and
  `uri` when present.
