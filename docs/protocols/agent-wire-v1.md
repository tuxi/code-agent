# Agent Wire Protocol v1

> 状态：v1（正式）。CodeAgent 从 CLI 迈向 CLI / macOS / iOS / Web 多前端架构的第一份传输契约。
> 实现位置：`internal/server`（Layer 2）。core（`internal/agent`，Layer 1）不感知本协议。
>
> **版本链**：v1（本文，基准）→ [v1.1 客户端工具执行](agent-wire-v1.1-client-tool-execution.md) → [v1.2 生命周期 Suspend/Resume/Reconfigure](agent-wire-v1.2-lifecycle-suspend-resume.md)。同一 major 内只增不改（见 §5）。

---

## 0. 分层原则（最重要）

```
agent-core (Layer 1)          agent-server (Layer 2)         frontends (Layer 3)
   agent.Event        ──toWire──▶   wireEvent  ──encode──▶  CLI / macOS / iOS / Web
   (time.Duration,                  (elapsed_ms,
    time.Time,                       RFC3339,
    rich Go types)                   structured JSON)
```

- **core 不感知传输**：`agent.Event` 保留富 Go 类型（`time.Duration`、`time.Time`、结构化字段），**不打 json tag**。
- **wire schema 属于 Layer 2**：`wireEvent` + `toWire(agent.Event) wireEvent` 是唯一的映射点。
- 任何"如何上线"的决定（毫秒化、RFC3339、结构化 args）都在 Layer 2 完成，core 不变。

---

## 1. 连接与握手

每条连接（WS / SSE）建立后，server 发送的**第一帧**是 `hello`，一次性钉死协议版本——逐事件不再携带版本（`token_delta` 是高频流，逐条带版本是浪费）。

```json
{ "type": "hello", "protocol_version": 1, "server": "codeagent/x.y" }
```

---

## 2. 两个消息族

一条 WS 同时承载两类消息：

| 消息族 | 方向 | 语义 | 例子 |
|---|---|---|---|
| **events** | server → client | 已经发生，fire-and-forget | `tool_started`、`token_delta`、`todo_updated` |
| **control** | 双向，request/response | 等待决定 | `approval_request` / `approval_response` |

> SSE 只能承载 events（单向）；control 需要回程，所以 SSE 部署必须额外配一个 POST 回调端点。WS 天然双向，推荐。

---

## 3. Event 信封

### 3.1 公共头（所有 event 都带）

| 字段 | 类型 | 说明 |
|---|---|---|
| `event_id` | string | 全局唯一，用于去重 / replay / 日志定位。由 emitter 盖戳（非 `toWire`）。 |
| `kind` | string | 判别式，见 §3.3。 |
| `at` | string | RFC3339（如 `2026-06-24T10:00:00.123Z`）。 |
| `session_id` | string | 产生该事件的会话。subagent 事件带的是**子会话自身**的 id。 |
| `parent_session_id` | string | 仅 subagent 事件：父会话 id，用于在 UI 里还原 root→child 树。 |
| `turn_id` | string | 产生该事件的 turn。 |

### 3.2 两个一旦写错就很难改的决定

1. **`elapsed_ms`（毫秒，不是纳秒）。** Go 默认把 `time.Duration` 序列化成纳秒 int64，Swift/JS 会读错量纲。Layer 2 统一换算成毫秒。
2. **`tool_args` 是结构化 JSON 对象，不是"装着 JSON 的字符串"。** core 内部存的是 JSON 字符串；上线时发成嵌套对象，客户端可直接 `toolArgs.command`。非法 JSON 时降级为 JSON 字符串，保证帧始终合法。

### 3.3 各 Kind 的字段契约

公共头之外，每个 `kind` 额外有意义的字段：

| `kind` | 额外字段 | 含义 |
|---|---|---|
| `turn_started` | `text` | 用户输入 |
| `model_started` | — | 即将调模型（计时锚点） |
| `model_finished` | `prompt_tokens` `elapsed_ms` `err` | 模型返回 |
| `token_delta` | `text` | 流式文本增量（高频、**不持久化**） |
| `thinking` | `text` | 推理文本 |
| `tool_started` | `call_id` `step` `tool_name` `tool_args` | 工具开始 |
| `tool_finished` | `call_id` `step` `tool_name` `observation` `err` | 工具结束 |
| `observed` | `call_id` `step` `tool_name` `observation` `failure` | 结果被分类（`failure` = FailureType，如 `compile`） |
| `auto_approved` | `tool_name` `tool_args` `text` | auto 模式自动放行（`text` = 原因，审计用） |
| `reflected` | `text` | finalize 自检触发 |
| `skill_loaded` | `tool_name` `skill_version` | 载入 skill（名在 `tool_name`） |
| `todo_updated` | `todos[]` | 任务清单变化 |
| `compacted` | `before_tokens` `after_tokens` `saved_tokens` `summary_chars` `ratio` | 上下文压缩 |
| `turn_finished` | `text` | 最终答复 |
| `task_started` | `session_id`(子) `parent_session_id` `text` | subagent 委派开始（`text` = 委派 prompt） |
| `task_finished` | `session_id`(子) `parent_session_id` `text` | subagent 结束（`text` = 结论） |
| `plan_proposed` | `text` | 模型产出计划，等待用户审批（`text` = plan 内容） |
| `plan_approved` | `text` | 用户批准计划（`text` = plan_id） |
| `plan_rejected` | `text` | 用户拒绝计划（`text` = plan_id） |

零值字段一律 `omitempty` 省略。

**聚合身份（reducer key）**：一次工具调用的 `tool_started` / `observed` / `tool_finished` **共享同一个 `call_id`**（模型的 tool_call id，服务端保证非空、跨 live/历史/replay 稳定）。UI 以 `call_id` 定位**同一张工具卡**就地更新（running → completed），而不是每事件新增一张。`turn_id` 是 turn 级聚合 key（恒在）。**不要用 `event_id` 当聚合 key**——它是每次发送的传输层去重 token，历史事件不带。

---

## 4. Control plane

### 4.1 审批（v1）

审批本质是 turn 半途的**阻塞往返**，直接对应 core 的 `Approver.Approve(toolName, input) bool`。

```json
// server → client（turn 阻塞，等回答）
{ "type": "approval_request", "id": "appr_7", "session_id": "sess_root", "turn_id": "turn_42",
  "tool_name": "run_command", "tool_args": { "command": "git push" }, "deadline_ms": 120000 }

// client → server
{ "type": "approval_response", "id": "appr_7", "approved": true }
```

- `id` 关联请求与响应。
- **客户端断线或超 `deadline_ms` → 默认拒绝**，对齐 core 里"nil Approver 一律拒"的 fail-safe 规则。
- auto 模式**不发** `approval_request`，改为发 `auto_approved` 事件（纯观测）。

#### 计划审批（v1.1）

与工具审批相同的阻塞往返模式，但 payload 是完整计划：

```json
// server → client（plan 等待审批）
{ "type": "plan_approval_request", "id": "plan_appr_1",
  "session_id": "sess_root", "turn_id": "turn_42",
  "plan_id": "plan_abc", "title": "Add Auth",
  "content": "# Plan\n1. Step one\n2. Step two",
  "deadline_ms": 120000 }

// client → server
{ "type": "plan_approval_response", "id": "plan_appr_1", "approved": true }
```

### 4.2 入站消息（client → server，已实现）

同一条 WS 上，客户端可发以下消息驱动会话：

```json
{ "type": "send_message", "text": "把这个模块迁到新 API" }   // 驱动一个 turn
{ "type": "approval_response", "id": "appr_7", "approved": true }  // 见 §4.1
{ "type": "plan_approval_response", "id": "plan_appr_1", "approved": true }  // 计划审批响应
{ "type": "cancel_turn" }                                    // 取消在飞的 turn
```

实现要点（`internal/server`）：

- 入站解码/路由由 **transport 无关的 `Router`**（`internal/server/router.go`）承担，不挂在 WS 上——SSE / 本地桥 / iOS 内嵌 runtime 都能复用同一套 command/control 分发。`Router` 把 `send_message`/`cancel_turn` 路由到 `CommandTarget`、`approval_response` 路由到 `ApprovalResolver`，两个平面去往不同子系统。
- `Router` 把 `send_message` **派发到独立 goroutine** 跑 `SendMessage`——否则喂帧的读循环会被阻塞的审批卡死，`approval_response` 读不进来，整个往返**死锁**。
- 一条连接对一个会话同时有两个写者（事件流 + `approval_request`），而 WS 只允许单写者，所以帧写出经一把互斥锁串行化。
- 连接接管会话期间挂上自己的远端审批器；断开时恢复 deny-all，无人值守的 turn 不会自动放行。

### 4.3 预留（同一信封，后续版本）

`switch_model`、`goal_start`（client → server）——目前在 TUI 里已是 channel，未来归并到 control plane。

---

## 5. 版本与兼容

- **版本在 §1 握手时协商一次**。
- 同一 major 内**只增不改**：可新增 `kind`、新增可选字段。
- **客户端必须忽略未知 `kind` 和未知字段**（前向兼容）。收到不认识的 `kind` 应 no-op，**不得** fatal——否则 server 新增 `kind`（如 `memory_hit`）会让旧客户端全挂。
- 破坏性改动（改名 / 删字段 / 改语义）才升 major。

### 5.1 `seq`（v2 预留）

每会话单调递增序号 `seq`，用于 WS 重连恢复（`resume_after_seq=1024`）。v1 不实现，但信封为其留位。**v1.2 落地此预留**（复用 `session_events.id` 自增值作 `seq`，新增 `attach(since:)` 重放，见 [v1.2 §4](agent-wire-v1.2-lifecycle-suspend-resume.md)）。

---

## 6. 契约锁定（golden files）

Protocol v1 的所有上线形状都被 golden 锁定，任何字段变化都会让 CI 直接 diff 报出，确保 macOS / iOS / Web 客户端长期稳定：

- **出站事件**：`internal/server/testdata/*.json`（`tool_started`、`todo_updated`、`compacted`、`model_finished` 等）。
- **入站命令 + 审批 RPC**：`internal/server/testdata/messages/*.json`（`send_message`、`cancel_turn`、`approval_request`、`approval_response`）。

重新生成（仅在有意改契约后）：`go test ./internal/server -run Golden -update`。
