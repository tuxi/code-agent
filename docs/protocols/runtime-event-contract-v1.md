# Runtime Event Contract v1

> **Status**: Frozen v1.0 — reviewed & approved by Go Runtime team (2026-07-09).
>
> **Merged from**:
> - [`agent-wire-v1.md`](../protocols/agent-wire-v1.md) — base event kinds, envelope, handshake
> - [`agent-wire-v1.1-client-tool-execution.md`](../protocols/agent-wire-v1.1-client-tool-execution.md) — client tool execution, `agent_input`
> - [`agent-wire-v1.2-lifecycle-suspend-resume.md`](../protocols/agent-wire-v1.2-lifecycle-suspend-resume.md) — lifecycle events, `seq`, `attach(since:)`
> - [`agent-wire-v1.3-tool-assets.md`](../protocols/agent-wire-v1.3-tool-assets.md) — `output`, `assets`, `text_annotations`
> - [`agent-wire-v1-approval-three-way.md`](../protocols/agent-wire-v1-approval-three-way.md) — three-way approval
>
> **Role**: 本文件是所有当前生效的 Runtime 事件契约的**单一事实源**。
> 旧文件保留为 version history / changelog。新开发以本文件为准。
> 每个 event kind 标注 `since v1.x`，详细变更历史回到各版本文件查阅。

---

## 1. Protocol Layering

```
agent-core (Layer 1)          agent-server (Layer 2)         frontends (Layer 3)
   agent.Event        ──toWire──▶   wireEvent  ──encode──▶  CLI / macOS / iOS / Web
   (time.Duration,                  (elapsed_ms,
    time.Time,                       RFC3339,
    rich Go types)                   structured JSON)
```

- Layer 1 (core) 不感知传输。`agent.Event` 保留富 Go 类型。
- Layer 2 (server) 是唯一的 wire schema 映射点。所有"如何上线"的决策在此完成。
- Layer 3 (frontends) 只消费本文件定义的 JSON 契约。

---

## 2. Connection & Handshake

### 2.1 WebSocket Endpoints

| Stream | Path | Direction | since |
|--------|------|-----------|-------|
| Conversation | `GET /v1/conversations/{id}/stream` | bidirectional | v1.0 |
| Job (subagent) | `GET /v1/jobs/{id}/stream` | server→client (read-only) | v1.0 |

### 2.2 Hello Frame

连接建立后 server 发送的**第一帧**，一次性协商协议版本：

```json
{
  "type": "hello",
  "protocol_version": 1,
  "server": "codeagent/x.y",
  "capabilities": ["streaming", "thinking", "tool_streaming", "plan_mode", "subagents", "session_resume", "client_tool_execution"]
}
```

| Field | Type | Required | Description |
|-------|------|:---:|-------------|
| `type` | string | ✅ | `"hello"` |
| `protocol_version` | int | ✅ | 当前 = `1` |
| `server` | string | ❌ | 服务端标识，debug 用 |
| `capabilities` | []string | ❌ | 服务端能力声明（since v1.1） |

**Capability 清单**：

| Capability | since | Meaning |
|-----------|-------|---------|
| `streaming` | v1.0 | 支持 `token_delta` 流式输出 |
| `thinking` | v1.0 | 支持 `thinking` 推理文本事件 |
| `tool_streaming` | v1.0 | 支持 `tool_stdout` / `tool_stderr` |
| `plan_mode` | v1.0 | 支持 `plan_proposed` / plan approval |
| `subagents` | v1.0 | 支持 `task_started` / `task_finished` |
| `session_resume` | v1.2 | 支持 `GET /events` 恢复 + `attach(since:)` |
| `client_tool_execution` | v1.1 | 支持 `executor:"client"` + `tool_result` 回传 |

---

## 3. Message Families

一条 WS 同时承载两类消息：

| Family | Direction | Semantics | Examples |
|--------|-----------|-----------|----------|
| **events** | server → client | 已发生，fire-and-forget | `tool_started`、`token_delta`、`todo_updated` |
| **control** | bidirectional, request/response | 等待决定 | `approval_request` / `approval_response` |

---

## 4. Event Envelope

### 4.1 Common Fields (all event kinds)

```json
{
  "seq": 1042,
  "kind": "tool_started",
  "event_id": "evt_a1b2c3",
  "at": "2026-06-24T10:00:00.123Z",
  "session_id": "sess_root",
  "parent_session_id": "sess_child_01",
  "turn_id": "turn_42"
}
```

| Field | Type | Required | Description |
|-------|------|:---:|-------------|
| `seq` | int64 | ❌ | Per-session 单调递增序号，用于去重/续传游标。since v1.2 |
| `kind` | string | ✅ | Event discriminator，见 §5 |
| `event_id` | string | ❌ | 全局唯一传输去重 token。历史事件可能不带 |
| `at` | string | ✅ | RFC3339 时间戳 |
| `session_id` | string | ✅ | 产生该事件的会话 ID |
| `parent_session_id` | string | ❌ | 仅 subagent 事件：父会话 ID |
| `turn_id` | string | ✅ | 产生该事件的 turn ID |

### 4.2 Field Conventions

- **`elapsed_ms`**：毫秒（不是纳秒）。Go `time.Duration` 默认序列化为纳秒 int64，Layer 2 统一换算为毫秒。
- **`tool_args`**：结构化 JSON 对象（不是 "装着 JSON 的字符串"）。非法 JSON 时降级为 JSON 字符串。
- **零值字段**：一律 `omitempty` 省略。
- **未知字段**：客户端必须忽略（前向兼容）。

### 4.3 `seq` — Replay Cursor (since v1.2)

- 来源：`session_events.id AUTOINCREMENT`（SQLite 自增主键，起始值 1）或 MemoryStore 的 `eventSeq`（起始值 0，首次 `Append` 递增到 1）
- 类型：int64
- 第一个有效事件的 `seq = 1`。不存在 `seq = 0` 的有效事件
- Per-session 单调递增，跨 session 不连续
- `GET /v1/conversations/{id}/events?since=0` — 获取**所有**事件（`since=0` 表示从头开始）
- `GET /v1/conversations/{id}/events?since=N` — 获取 `seq > N` 的事件
- Client 维护 `maxSeq`，每次重连后先用 HTTP 补缺口再放行直播帧

---

## 5. Event Kind Registry

### 5.1 Turn Lifecycle Events

#### `turn_started` (since v1.0)

User input starts a new turn.

```json
{
  "kind": "turn_started",
  "text": "分析 src/auth/ 目录的代码结构"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `text` | string | 用户输入的文本 |

#### `turn_finished` (since v1.0)

Final assistant response. Turn complete.

```json
{
  "kind": "turn_finished",
  "text": "该目录包含以下文件...",
  "text_annotations": []
}
```

| Field | Type | Description |
|-------|------|-------------|
| `text` | string | 最终答复（Markdown） |
| `text_annotations` | array | since v1.3 — asset 链接注解，见 §7 |

#### `turn_paused` (since v1.2)

Turn has been suspended and checkpointed.

```json
{
  "kind": "turn_paused",
  "paused_at": 1750000000
}
```

#### `turn_resuming` (since v1.2)

`ResumeSession` has been called, about to replay.

```json
{
  "kind": "turn_resuming"
}
```

#### `turn_resumed` (since v1.2)

Resume replay complete, turn back to running.

```json
{
  "kind": "turn_resumed"
}
```

#### `turn_failed` (since v1.2)

Turn terminated due to unrecoverable error.

```json
{
  "kind": "turn_failed",
  "error": {
    "code": "auth_expired",
    "message": "Gateway returned 401 — access token may be expired"
  }
}
```

`error.code` is an **open set** — clients MUST handle unknown codes gracefully (show generic error message). Adding new codes is NOT a breaking change.

| Code | Meaning | Host Action |
|------|---------|-------------|
| `auth_expired` | Gateway JWT expired | Refresh token → `Reconfigure` |
| `quota_exceeded` | Gateway quota exhausted | Show quota UI |
| `subscription_required` | Tier insufficient | Show upgrade UI |
| `request_failed` | Generic non-retryable error | Show error message |
| *(any unknown)* | Future error type | Graceful degrade — show generic error |

### 5.2 Model Interaction Events

#### `model_started` (since v1.0)

About to call the LLM. Timing anchor.

```json
{
  "kind": "model_started"
}
```

#### `model_finished` (since v1.0)

LLM returned.

```json
{
  "kind": "model_finished",
  "prompt_tokens": 2500,
  "elapsed_ms": 3200,
  "err": null
}
```

| Field | Type | Description |
|-------|------|-------------|
| `prompt_tokens` | int | Prompt token count |
| `elapsed_ms` | int | Model call duration in milliseconds |
| `err` | string | null or error message |

#### `token_delta` (since v1.0)

Streaming text chunk. **Not persisted** — live WebSocket only, never stored in event log. Reconnect after disconnect will NOT replay `token_delta` frames. The complete text is always available in `turn_finished.text`.

```json
{
  "kind": "token_delta",
  "text": "该目"
}
```

#### `thinking` (since v1.0)

Reasoning/thinking text. **Persisted** — stored in event log with `seq`, recoverable on replay/reconnect.

```json
{
  "kind": "thinking",
  "text": "用户想分析 auth 目录的结构..."
}
```

### 5.3 Tool Execution Events

#### `tool_started` (since v1.0)

Tool invocation begins.

```json
{
  "kind": "tool_started",
  "call_id": "call_42",
  "step": 1,
  "tool_name": "grep",
  "tool_args": { "pattern": "TODO", "path": "./src" },
  "executor": "client"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `call_id` | string | Tool call ID — 聚合 key for `tool_started` / `observed` / `tool_finished` |
| `step` | int | Turn step number |
| `tool_name` | string | Tool identifier |
| `tool_args` | object | Structured tool arguments |
| `executor` | string | since v1.1 — `"client"` means client must execute; absent = server executes |

#### `tool_finished` (since v1.0, extended v1.3)

Tool invocation complete.

```json
{
  "kind": "tool_finished",
  "call_id": "call_42",
  "step": 1,
  "tool_name": "grep",
  "observation": "...raw transcript...",
  "err": null,
  "output": {},
  "assets": []
}
```

| Field | Type | Description |
|-------|------|-------------|
| `observation` | string | Canonical text transcript — what the model sees |
| `err` | string | null or error message |
| `output` | object | since v1.3 — structured tool output，见 §7 |
| `assets` | array | since v1.3 — normalized clickable asset index，见 §7 |

#### `observed` (since v1.0)

Tool result classified.

```json
{
  "kind": "observed",
  "call_id": "call_42",
  "step": 1,
  "tool_name": "compile",
  "observation": "error: undefined reference",
  "failure": "compile"
}
```

#### `auto_approved` (since v1.0)

Tool auto-executed in auto mode (no approval required).

```json
{
  "kind": "auto_approved",
  "tool_name": "grep",
  "tool_args": { "pattern": "TODO", "path": "./src" },
  "text": "auto mode — no human approval required"
}
```

### 5.4 Context Management Events

#### `reflected` (since v1.0)

Self-reflection triggered during finalize.

```json
{
  "kind": "reflected",
  "text": "自我检查：代码风格一致..."
}
```

#### `skill_loaded` (since v1.0)

Skill/instruction loaded.

```json
{
  "kind": "skill_loaded",
  "tool_name": "review-change",
  "skill_version": "1.0"
}
```

#### `compacted` (since v1.0)

Context compression occurred.

```json
{
  "kind": "compacted",
  "before_tokens": 80000,
  "after_tokens": 40000,
  "saved_tokens": 40000,
  "summary_chars": 2000,
  "ratio": 0.5
}
```

### 5.5 Task / Subagent Events

#### `task_started` (since v1.0)

Subagent delegation began.

```json
{
  "kind": "task_started",
  "session_id": "sess_child_01",
  "parent_session_id": "sess_root",
  "text": "search for all TODO markers in src/"
}
```

#### `task_finished` (since v1.0)

Subagent completed.

```json
{
  "kind": "task_finished",
  "session_id": "sess_child_01",
  "parent_session_id": "sess_root",
  "text": "Found 42 TODOs across 18 files"
}
```

### 5.6 Plan Mode Events

#### `plan_proposed` (since v1.0)

Model proposed a plan, awaiting user approval.

```json
{
  "kind": "plan_proposed",
  "text": "# Plan\n1. Step one\n2. Step two"
}
```

#### `plan_approved` (since v1.0)

User approved the plan.

```json
{
  "kind": "plan_approved",
  "text": "plan_abc"
}
```

#### `plan_rejected` (since v1.0)

User rejected the plan.

```json
{
  "kind": "plan_rejected",
  "text": "plan_abc"
}
```

### 5.7 Todo Events

#### `todo_updated` (since v1.0)

Task list changed.

```json
{
  "kind": "todo_updated",
  "todos": [
    { "text": "Analyze auth directory", "status": "completed" },
    { "text": "Identify security issues", "status": "in_progress" },
    { "text": "Write report", "status": "pending" }
  ]
}
```

---

## 6. Control Plane

### 6.1 Approval Request/Response (since v1.0, extended v1.2)

**Server → Client**:
```json
{
  "type": "approval_request",
  "id": "appr_a1b2c3",
  "session_id": "sess_root",
  "turn_id": "turn_42",
  "tool_name": "run_command",
  "tool_args": { "command": "git push" },
  "deadline_ms": 120000
}
```

**Client → Server** (recommended — three-way):
```json
{ "type": "approval_response", "id": "appr_a1b2c3", "decision": "always", "scope": "local" }
{ "type": "approval_response", "id": "appr_a1b2c3", "decision": "once" }
{ "type": "approval_response", "id": "appr_a1b2c3", "decision": "deny" }
```

**Client → Server** (legacy — two-way, still supported):
```json
{ "type": "approval_response", "id": "appr_a1b2c3", "approved": true }
```

| Field | Type | Required | Description |
|-------|------|:---:|-------------|
| `decision` | string | 推荐 | `"once"` / `"always"` / `"deny"`。存在时优先于 `approved` |
| `scope` | string | 可选 | `decision = "always"` 时：`"local"`（项目本地，默认）或 `"user"`（全局） |
| `approved` | bool | 兼容 | 旧字段。`decision` 存在时被忽略 |
| `deadline_ms` | int | 可选 | 仅服务端配置了审批超时时存在；缺失 = 无限等待 |

### 6.2 Plan Approval (since v1.0)

```json
// Server → Client
{ "type": "plan_approval_request", "id": "plan_appr_1",
  "session_id": "sess_root", "turn_id": "turn_42",
  "plan_id": "plan_abc", "title": "Add Auth",
  "content": "# Plan\n1. Step one\n2. Step two" }

// Client → Server
{ "type": "plan_approval_response", "id": "plan_appr_1", "approved": true }
```

---

## 7. Structured Output — v1.3 Extensions

### 7.1 `output` on `tool_finished`

Tool-specific structured data. `output.kind` identifies the shape:

| `output.kind` | Tool | Description |
|---------------|------|-------------|
| `search_results` | `grep` | Search hits with path/line/preview |
| `file` | `read_file` | File content + metadata |
| `directory_listing` | `list_files` | Directory entries |
| `symbols` / `references` | `project_graph` | Symbol/reference data |
| `mcp_content` | MCP tools | MCP content block metadata |

### 7.2 `assets` on `tool_finished`

Normalized clickable index consumed by client UI. Each asset:

```json
{
  "id": "asset_turn_1_call_02_001_a1b2c3d4",
  "kind": "file_location",
  "uri": "workspace://agentkit-local/Sources/App.swift#L49",
  "display_name": "App.swift:49",
  "workspace_id": "agentkit-local",
  "workspace_relative_path": "Sources/App.swift",
  "absolute_path": "/Users/.../Sources/App.swift",
  "range": { "start_line": 49, "start_column": 1 },
  "preview": "public var streamingText: String = \"\"",
  "mime_type": "text/x-swift",
  "metadata": { "language": "swift" },
  "source_turn_id": "turn_1",
  "source_call_id": "call_02"
}
```

Asset `kind` values: `file`, `file_location`, `directory`, `url`, `symbol`, `search_result`, `diff`, `terminal`, `markdown`, `image`, `video`, `audio`, `pdf`.

### 7.3 `text_annotations` on `turn_finished`

Links ranges in assistant Markdown back to assets:

```json
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
```

Offsets: `start_byte`/`end_byte` = UTF-8; `start_utf16`/`end_utf16` = UTF-16 (Swift `NSRange`).

### 7.4 Asset Read API

```
GET /v1/conversations/{id}/assets/{asset_id}/preview   → JSON metadata + preview
GET /v1/conversations/{id}/assets/{asset_id}/content   → JSON text content (workspace text files)
GET /v1/conversations/{id}/assets/{asset_id}/blob      → Raw bytes (binary, HTTP range support)
GET /v1/conversations/{id}/assets/{asset_id}/thumbnail → Reserved (Phase 1 returns 501)
```

---

## 8. Inbound Messages (Client → Server)

### 8.1 `agent_input` (recommended, since v1.1)

```json
// kind: "text"
{ "type": "agent_input", "kind": "text", "text": "分析这个项目" }

// kind: "tool_result"
{ "type": "agent_input", "kind": "tool_result",
  "tool_result": {
    "tool_use_id": "call_abc123",
    "subtype": "result",
    "content": "视频修剪完成：/tmp/output.mp4",
    "is_error": false
  }
}

// kind: "command"
{ "type": "agent_input", "kind": "command", "text": "cancel" }

// kind: "system" — reserved for future use (dynamic rule injection, context patching).
// Clients SHOULD NOT send this kind currently. Servers MAY ignore it if received.
{ "type": "agent_input", "kind": "system",
  "command": "patch_context",
  "command_key": "project_rules",
  "command_value": "使用 Swift 6 规范" }
```

### 8.2 Legacy Messages (still supported)

```json
{ "type": "send_message", "text": "分析这个项目" }
{ "type": "cancel_turn" }
{ "type": "approval_response", "id": "appr_7", "approved": true }
{ "type": "plan_approval_response", "id": "plan_appr_1", "approved": true }
```

### 8.3 Client Tool Registration (since v1.1)

`register_tools` is accepted **between turns** (after `turn_finished`, before next `send_message`). The tool set for a turn is frozen at `turn_started` — mid-turn registration is NOT supported.

After handshake, client may register executable tools:

```json
{ "type": "register_tools",
  "tools": [
    {
      "name": "trim_video",
      "description": "Trim a video file using AVFoundation",
      "input_schema": {
        "type": "object",
        "properties": {
          "url": { "type": "string" },
          "start": { "type": "number" },
          "duration": { "type": "number" }
        },
        "required": ["url"]
      }
    }
  ]
}
```

---

## 9. Event Ordering Guarantees

### 9.1 Within a Turn

```
turn_started
    │
    ├─ model_started
    │     ├─ thinking (may appear)
    │     ├─ token_delta × N
    │     └─ model_finished
    │
    ├─ tool_started ── tool_finished    (server tool)
    │  (or)
    ├─ tool_started(executor:"client") ── [client executes] ── tool_finished
    │
    ├─ (model_started → ... → model_finished) × N  (if multiple tool calls)
    │
    ├─ reflected (if finalize triggers self-check)
    ├─ skill_loaded (if skills loaded)
    ├─ todo_updated (throughout)
    │
    └─ turn_finished
```

### 9.2 Subagent Events

```
turn_started (parent)
    │
    ├─ task_started (child session)
    │     ├─ turn_started (child)
    │     ├─ ... (child's own turn events) ...
    │     └─ turn_finished (child)
    └─ task_finished (child session)
```

### 9.3 Plan Mode

```
turn_started
    │
    ├─ plan_proposed
    │     │
    │     ├─ plan_approved → normal execution continues
    │     └─ plan_rejected → turn ends with plan_rejected
```

### 9.4 Replay Ordering

`GET /v1/conversations/{id}/events` returns events ordered by:
- `session_id` first (root → children in creation order)
- then by `(at ASC, seq ASC)` within each session

---

## 10. Replay & Resume Semantics (since v1.2)

### 10.1 Attach Flow

```
1. Client: GET /v1/conversations → conversation list
2. Client: GET /v1/conversations/{id}/events?since=0 → full history
3. Client builds maxSeq from received events
4. Client: WS GET /v1/conversations/{id}/stream
5. Server: hello frame → backfills events with seq > maxSeq → live stream
```

### 10.2 Reconnect

On WS disconnect:
- Client keeps `maxSeq` (last received seq)
- Reconnect: `GET /v1/conversations/{id}/events?since=maxSeq` → gap events → live stream
- Live stream deduplicates: events with `seq <= maxSeq` are discarded

### 10.3 Client Recovery

```
Client restart:
  GET /v1/conversations → find previous session
  GET /v1/conversations/{id}/events?since=0 → full replay
  GET /v1/conversations/{id}/stream → attach with new since = maxSeq
```

---

## 11. Compatibility Rules

1. **同一 major 版本内只增不改**：可新增 `kind`、新增可选字段。不删除、不重命名、不改语义。
2. **客户端必须忽略未知 `kind` 和未知字段**（前向兼容）。收到不认识的 `kind` 应 no-op，不得 fatal。
3. **服务端必须忽略未知入站消息 type 和未知字段**。
4. **破坏性改动**（改名/删字段/改语义）才升 major。
5. **Golden 文件锁定所有契约变更**：`internal/server/testdata/*.json`。

### 10.4 Job Stream (since v1.0)

Job 子流（`GET /v1/jobs/{id}/stream`）使用**相同的事件信封**（§4），但有以下差异：

- **独立 seq 空间**：Job 在自己的 `session_events` 分区中有独立的 seq 序列，不与父会话混淆
- **只读**：server→client only，不接受 client 发来的消息
- **`parent_session_id`**：指向触发该 job 的父 conversation ID
- **Backfill**：`GET /v1/jobs/{id}/events?since=N` 使用 job 自己的 seq 游标

---

## 12. Review Resolutions (Go Runtime, 2026-07-09)

所有 Draft 阶段标注的 `[REVIEW]` 点已 resolve。

| # | 疑点 | 决议 |
|---|------|------|
| 1 | `turn_failed.error.code` 枚举 | **开放集合**。新增 code 不是 breaking change。客户端必须对未知 code graceful degrade（展示通用错误信息） |
| 2 | `seq` 初值 | 第一个有效事件 `seq = 1`。`since=0` 获取所有事件。不存在 `seq = 0` 的有效事件 |
| 3 | `token_delta` 和 `thinking` 去重 | `token_delta`：不持久化、无 seq、重连时不恢复（最终文本在 `turn_finished.text` 中完整呈现）。`thinking`：持久化、有 seq、重连时通过 `since=maxSeq` 补缺口 |
| 4 | `agent_input.kind="system"` 语义 | v1.1 stub，无实现时间线。预留给动态规则注入/上下文补丁。客户端当前不应发送 |
| 5 | `register_tools` 时机 | 必须在 turn 之间（`turn_finished` → `register_tools` → 下一条 `send_message`）。Turn 中途不支持动态注册——tool set 在 `turn_started` 时冻结 |
| 6 | Job stream 事件格式 | 相同 envelope，独立 seq 空间。Job 有自己的 session + 独立 seq 序列。只读端点

---

## Appendix A: Quick Reference — All Event Kinds

| Kind | since | Category | Persisted | Has `seq` |
|------|-------|----------|:---:|:---:|
| `turn_started` | v1.0 | Turn lifecycle | ✅ | ✅ |
| `turn_finished` | v1.0 | Turn lifecycle | ✅ | ✅ |
| `turn_paused` | v1.2 | Turn lifecycle | ✅ | ✅ |
| `turn_resuming` | v1.2 | Turn lifecycle | ✅ | ✅ |
| `turn_resumed` | v1.2 | Turn lifecycle | ✅ | ✅ |
| `turn_failed` | v1.2 | Turn lifecycle | ✅ | ✅ |
| `model_started` | v1.0 | Model | ✅ | ✅ |
| `model_finished` | v1.0 | Model | ✅ | ✅ |
| `token_delta` | v1.0 | Model | ❌* | ❌ |
| `thinking` | v1.0 | Model | ✅ | ✅ |
| `tool_started` | v1.0 | Tool | ✅ | ✅ |
| `tool_finished` | v1.0 | Tool | ✅ | ✅ |
| `observed` | v1.0 | Tool | ✅ | ✅ |
| `auto_approved` | v1.0 | Tool | ✅ | ✅ |
| `reflected` | v1.0 | Context | ✅ | ✅ |
| `skill_loaded` | v1.0 | Context | ✅ | ✅ |
| `compacted` | v1.0 | Context | ✅ | ✅ |
| `todo_updated` | v1.0 | Todo | ✅ | ✅ |
| `task_started` | v1.0 | Subagent | ✅ | ✅ |
| `task_finished` | v1.0 | Subagent | ✅ | ✅ |
| `plan_proposed` | v1.0 | Plan | ✅ | ✅ |
| `plan_approved` | v1.0 | Plan | ✅ | ✅ |
| `plan_rejected` | v1.0 | Plan | ✅ | ✅ |

> \* `token_delta` is live-only: never stored in event log, never replayed on reconnect. The complete text is in `turn_finished.text`.

---

## Appendix B: Control & Inbound Message Reference

| Type | Direction | since |
|------|-----------|-------|
| `hello` | server → client | v1.0 |
| `approval_request` | server → client | v1.0 |
| `approval_response` | client → server | v1.0 |
| `plan_approval_request` | server → client | v1.0 |
| `plan_approval_response` | client → server | v1.0 |
| `agent_input` | client → server | v1.1 |
| `send_message` | client → server | v1.0 (legacy) |
| `cancel_turn` | client → server | v1.0 |
| `register_tools` | client → server | v1.1 |
