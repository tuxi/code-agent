# CodeAgent 客户端对接文档 v1

> 对接 `codeagent serve` 暴露的 **Agent Wire Protocol v1**。读者：macOS / iOS / Web 客户端开发。
> 规范源：[docs/protocols/agent-wire-v1.md](protocols/agent-wire-v1.md)。契约 golden：`internal/server/testdata/`。
> 本文只描述**当前真实实现**（P1-A）。文末「v1 限制」列出还没有的东西，请勿提前依赖。

---

## 0. 一分钟跑通

```bash
codeagent serve                       # 默认 127.0.0.1:8787
# 另开一个终端：
curl -s localhost:8787/healthz                                   # -> ok
curl -s -XPOST localhost:8787/v1/conversations -d '{"workspace_path":"/p"}'
#   -> {"id":"20260625-002716-74c7ae7b"}
# 用上面的 id 连 WebSocket：
#   ws://127.0.0.1:8787/v1/conversations/<id>/stream
#   连上后第一帧：
#   {"type":"hello","protocol_version":1,"server":"codeagent/<model>"}
```

启动地址可改：`codeagent serve 127.0.0.1:9000`。默认绑 `127.0.0.1`（仅本机）。

---

## 1. 传输总览

| 层 | 用途 |
|---|---|
| HTTP/JSON | 会话生命周期：建/列/健康检查 |
| WebSocket（每会话一条） | 双向：**出站事件流** + **入站命令/审批** |

一个会话 = 一个 agent 线程（`session_id` 即会话 id）。一条 WS 绑定一个会话，是它的**控制连接**：连接期间该会话的副作用工具向这条连接请求审批；连接断开后，会话恢复"一律拒绝副作用"（无人值守不自动放行）。

---

## 2. HTTP API

基址：`http://<addr>`（默认 `http://127.0.0.1:8787`）。

### `GET /healthz`
存活探针。`200`，body 文本 `ok`。

### `GET /v1/conversations`
列出当前**内存中**的会话。

```json
200  [{"id":"20260625-002716-74c7ae7b"}, {"id":"20260625-002716-defaa979"}]
```
v1 只返回 `id`（model / 消息数 / 时间等 metadata 属于 P1-B）。

### `POST /v1/conversations`
新建会话。

```json
// 请求体（可选；字段见下）
{ "workspace_path": "/Users/you/project" }

// 201
{ "id": "20260625-002716-74c7ae7b" }
```

- `workspace_path`（string，可选）：**协议已冻结、但 v1 服务端忽略**。请客户端现在就按它写 DTO——未来 per-conversation workspace 落地时不需要改字段。
- 空 body（`{}` 或无 body）也接受。

### 历史读取（恢复 Timeline 用）

三个只读接口让客户端**重新打开**一个会话时把 Timeline 补回来（再连 WS 拿增量）。v1 全部从**已记录的事件流**派生，单一数据源。id 同时不在内存、也无任何事件 → `404`。

#### `GET /v1/conversations/{id}/events`
返回该会话**已记录的事件**，重编码成**和 WS 完全一样的 wire v1 形状**（直接喂你的 `WireEvent` 解码器）：

```json
200
[
  {"kind":"turn_started","at":"...","session_id":"...","turn_id":"turn_1","text":"分析项目"},
  {"kind":"tool_started","at":"...","tool_name":"grep","tool_args":{"q":"x"},"step":1},
  {"kind":"model_finished","at":"...","elapsed_ms":866},
  {"kind":"turn_finished","at":"...","text":"项目结构如下"}
]
```
- 与 WS 唯一差异：历史事件**不带 `event_id`**（它在 WS 发送时才盖戳，未持久化）。客户端按可选处理。
- **不含 `token_delta`**（流式增量不持久化）。回放时用 `turn_finished.text` 还原助手最终文本即可。
- 历史 subagent 事件**不带 `parent_session_id`**（同样未持久化）——嵌套关系在 v1 回放里降级。

#### `GET /v1/conversations/{id}/messages`
对话主干，由 `turn_started`(user) / `turn_finished`(assistant) 还原：

```json
200  [{"seq":0,"role":"user","content":"分析项目"},{"seq":1,"role":"assistant","content":"项目结构如下"}]
```
v1 只含 user/assistant；工具/系统消息的全量保真属于 P1-B。

#### `GET /v1/conversations/{id}`
会话概要（由事件派生）：

```json
200  {"id":"...","turn_count":1,"message_count":2,"created_at":"<首个事件>","updated_at":"<末个事件>"}
```
`summary` / `model` 属于 P1-B（需要会话行持久化）。

### `GET /v1/conversations/{id}/stream`
升级为 WebSocket（见 §3）。`{id}` 不存在 → `404`（不升级）。

### 推荐的恢复流程（History + Live）

```
打开会话
  → GET /v1/conversations/{id}/events      （拿历史，渲染 Timeline）
  → 连 ws .../stream                        （读 hello）
  → 之后按 §3 接收增量事件
```
即"先补历史、再接实时流"。注意衔接：连上 WS 那一刻起的事件是增量；历史那批没有 `event_id`，实时那批有——客户端以"连接时刻"为界去重，不要依赖 `event_id` 跨两批对齐（v1 无 `seq`，见 §6）。

---

## 3. WebSocket 协议

### 3.1 帧的判别：`type` vs `kind`（最重要）

一条 WS 上有两类 JSON 帧，**靠顶层字段区分**：

- 含 **`type`** → 握手 / 控制帧（`hello`、`approval_request`）。
- 含 **`kind`** → 事件帧（`turn_started`、`tool_started`…）。

二者互斥：事件永远没有 `type`，控制帧永远没有 `kind`。客户端解码第一步就分流。

### 3.2 连接握手

连上后服务端发的**第一帧**必为：

```json
{ "type": "hello", "protocol_version": 1, "server": "codeagent/deepseek-v4-flash" }
```

`protocol_version` **只在握手时给一次**，后续帧不再带版本。客户端据此固定本连接的协议版本。

### 3.3 出站：事件帧（server → client）

公共头（每个事件都有，除非 omitempty 省略）：

| 字段 | 类型 | 说明 |
|---|---|---|
| `event_id` | string | 全局唯一，可用于去重/日志 |
| `kind` | string | 判别式，见下表 |
| `at` | string | **RFC3339 毫秒、UTC**，如 `2026-06-24T10:00:00.123Z` |
| `session_id` | string | 产生事件的会话 |
| `parent_session_id` | string | 仅 subagent 事件：父会话，用于嵌套渲染 |
| `turn_id` | string | 产生事件的 turn，用于把一轮的事件聚到一起 |

各 `kind` 的额外字段：

| `kind` | 额外字段 | 含义 |
|---|---|---|
| `turn_started` | `text` | 用户输入（这一轮的起点） |
| `model_started` | — | 即将调模型 |
| `model_finished` | `prompt_tokens` `elapsed_ms` `err` | 模型返回（`elapsed_ms` 是毫秒） |
| `token_delta` | `text` | **流式文本增量**，高频、不持久化；累加成助手回复 |
| `thinking` | `text` | 推理文本 |
| `tool_started` | `call_id` `step` `tool_name` `tool_args` | 工具开始（`tool_args` 是**结构化 JSON 对象**） |
| `tool_finished` | `call_id` `step` `tool_name` `observation` `err` | 工具结束 |
| `observed` | `call_id` `step` `tool_name` `observation` `failure` | 结果被分类（`failure` 如 `compile`） |
| `auto_approved` | `tool_name` `tool_args` `text` | auto 模式自动放行（`text`=原因，审计） |
| `reflected` | `text` | 收尾自检 |
| `skill_loaded` | `tool_name` `skill_version` | 载入 skill（名在 `tool_name`） |
| `todo_updated` | `todos` | 任务清单变化（见 §3.6） |
| `compacted` | `before_tokens` `after_tokens` `saved_tokens` `summary_chars` `ratio` | 上下文压缩 |
| `turn_finished` | `text` | 本轮最终答复（这一轮的终点） |
| `task_started` | `session_id`(子) `parent_session_id` `text` | subagent 委派开始（`text`=委派 prompt） |
| `task_finished` | `session_id`(子) `parent_session_id` `text` | subagent 结束（`text`=结论） |

> 渲染建议：按 `turn_id` 把一轮的事件聚成一个气泡；`token_delta` 实时拼接成助手文本；subagent 事件按 `parent_session_id` 折叠成子流。

#### 聚合身份（reducer 必读）

UI 的 reducer **不要用 `UUID()` 或 `event_id` 当 key**，否则 replay / 重连 / 重进会话时同一逻辑事件拿到不同 id，导致工具卡重复、`tool_finished` 找不到对应 `tool_started`。用这两个**稳定** key：

- **`turn_id`** —— turn 级聚合。`turns[turnID]`，不要 `turns.last`。每个事件都带，live/历史一致。
- **`call_id`** —— 工具调用级聚合。一次调用的 `tool_started` / `observed` / `tool_finished` **共享同一个 `call_id`**（模型的 tool_call id，服务端保证非空、跨 live/历史/replay 稳定）。工具卡是一个**状态机**：`tool_started` 建卡（running），`tool_finished` **更新同一张卡**（completed），不是 append 两张。

```swift
// 工具卡按 call_id 就地更新，而非每事件新增
struct ToolCall { let callID: String; let tool: String; var status: ToolStatus; var args: JSONValue?; var result: String? }
reducer: tool_started(call_id) -> toolCalls[call_id] = .init(running)
         tool_finished(call_id) -> toolCalls[call_id]?.status = .completed; .result = observation
```

`event_id` 只用于传输层去重/日志，**不参与聚合**（历史事件不带它）。

### 3.4 控制：审批请求（server → client）

turn 进行中，副作用工具（如 `run_command`、`apply_patch`）需要确认时，服务端**主动**推一条：

```json
{
  "type": "approval_request",
  "id": "appr_3f9a1c",
  "tool_name": "run_command",
  "tool_args": { "command": "git push" },
  "deadline_ms": 120000
}
```

- **`id` 是关联键**——回复时必须原样带回。
- ⚠️ **v1 实际行为**：`session_id` / `turn_id` 当前**不下发**（审批器没有 turn 上下文）。因为一条 WS 只服务一个会话，客户端本就知道是哪个会话，用 `id` 关联即可。schema 允许这两个字段，未来可能补上，客户端按可选处理。
- 收到此帧时，**该 turn 已暂停、在等你的回复**。
- `deadline_ms` 内不回复，或连接断开 → 服务端按**拒绝**处理（fail-safe）。

### 3.5 入站：命令 + 审批应答（client → server）

```json
{ "type": "send_message", "text": "把这个模块迁到新 API" }      // 驱动一个 turn
{ "type": "cancel_turn" }                                      // 取消在飞的 turn
{ "type": "approval_response", "id": "appr_3f9a1c", "approved": true }  // 回应 §3.4
```

- `send_message`：一个会话**同一时刻只能有一个 turn**。前一轮未 `turn_finished` 前再发，会被**静默忽略**（服务端按 busy 丢弃，不回错误帧）。请等 `turn_finished` 再发下一条。
- `cancel_turn`：在下一个检查点取消当前 turn，事件流随之停止该轮输出；**不保证**有 `turn_finished`，客户端本地按已取消处理。
- `approval_response`：`id` 必须匹配某条 `approval_request`；`approved` 为 `false` 即拒绝。

### 3.6 Todo 结构

```json
"todos": [
  { "content": "写 wire.go", "active_form": "writing wire.go", "status": "in_progress" },
  { "content": "补 golden 测试", "status": "pending" }
]
```
`status` ∈ `pending` | `in_progress` | `completed`；`active_form` 可缺省。

---

## 4. 两条关键时序

### 4.1 普通一轮

```
client → {type:send_message, text:"..."}
server → turn_started
server → model_started
server → token_delta × N        （流式拼接助手文本）
server → thinking?              （可能有）
server → model_finished
server → turn_finished {text}   ← 本轮结束，可发下一条
```

### 4.2 带审批的一轮（核心闭环）

```
client → {type:send_message, text:"提交并推送"}
server → turn_started
server → tool_started {tool_name:"run_command", tool_args:{command:"git push"}}
server → {type:approval_request, id:"appr_x", tool_args:{...}, deadline_ms:120000}   ⏸ turn 暂停
        ── 客户端弹审批卡片 ──
client → {type:approval_response, id:"appr_x", approved:true}                        ▶ turn 继续
server → tool_finished {tool_name:"run_command", observation:"..."}
server → turn_finished {text:"已推送"}
```

---

## 5. 上线约定 / 易错点

1. **先按 `type` 还是 `kind` 分流**（§3.1）。
2. **`elapsed_ms` 是毫秒**，不是纳秒/秒。
3. **`tool_args` 是结构化 JSON 对象**（`{"command":"..."}`），不是字符串。
4. **`at` 是 RFC3339 毫秒 UTC**。
5. **忽略未知 `kind` 和未知字段**（前向兼容）；版本只在 `hello` 协商一次，别期望逐事件带版本。收到不认识的 `kind` 应 no-op，**不要崩**——服务端新增事件不该让旧客户端挂掉。
6. **审批是阻塞往返**：收到 `approval_request` 即代表 turn 在等你；尽快在 `deadline_ms` 内回 `approval_response`。
7. **一轮一发**：`turn_finished` 之前不要再 `send_message`。
8. **断线重连**：WS 重连只给新 `hello`，会错过断线期间的**实时增量**（尤其 `token_delta`）。但**已记录**的事件可用 `GET /events` 补回（不含 `token_delta`）。无逐事件 `seq` 级精确续传（`seq` 是 v2 预留）。

---

## 6. v1 限制（请勿提前依赖）

- **无鉴权**。默认绑 `127.0.0.1`。原生客户端（mac/iOS，无 `Origin` 头）可直连；浏览器跨源需服务端配置 origin 策略（v1 CLI 未暴露）。
- **会话对象是内存态**：`POST` 建的会话只在内存里，`GET /v1/conversations`（列表）和 `/stream` 只认内存中的会话；**服务端重启后无法在列表里发现、也无法续跑**（resume = P1-B）。
- **事件已落 SQLite**：`GET /events` / `/messages` / `/{id}` 经已记录的事件读历史——**知道 id 的话，跨重启仍可读历史**（但读完不能再 attach WS 续跑，见上条）。
- **会话 metadata 极简**：列表/创建只返回 `id`；详情只有计数+时间戳（`summary`/`model` = P1-B）。
- **审批请求暂不带 `session_id`/`turn_id`**（§3.4）。
- 入站 `switch_model` / `goal_start` 为**预留**，尚未实现。

---

## 7. 版本与兼容

- 版本在 `hello` 协商一次（当前 `protocol_version: 1`）。
- 同一大版本内**只增不改**：可能新增 `kind`、新增可选字段。客户端**必须**容忍并忽略未知项。
- 破坏性改动才升大版本。
- 字段形状以 golden 为准：出站事件 `internal/server/testdata/*.json`，入站/审批 `internal/server/testdata/messages/*.json`。CI 对这些做 diff，所以这些 JSON 就是你 DTO 的事实来源。

---

## 8. Swift DTO 草样（Codable）

> 一个"宽结构体 + 可选字段"对应事件信封即可；也可按 `kind` 再分派。Web/TS 同理。

```swift
// ── 出站事件 ──
struct WireEvent: Decodable {
    let kind: String
    let at: String
    let eventId: String?
    let sessionId: String?
    let parentSessionId: String?
    let turnId: String?
    let step: Int?
    let toolName: String?
    let toolArgs: JSONValue?        // 任意 JSON 对象，见下
    let observation: String?
    let failure: String?
    let skillVersion: String?
    let todos: [Todo]?
    let text: String?
    let promptTokens: Int?
    let elapsedMs: Int?
    let beforeTokens, afterTokens, savedTokens, summaryChars: Int?
    let ratio: Double?
    let err: String?

    enum CodingKeys: String, CodingKey {
        case kind, at, step, observation, failure, todos, text, ratio, err
        case eventId = "event_id", sessionId = "session_id"
        case parentSessionId = "parent_session_id", turnId = "turn_id"
        case toolName = "tool_name", toolArgs = "tool_args"
        case skillVersion = "skill_version", promptTokens = "prompt_tokens"
        case elapsedMs = "elapsed_ms", beforeTokens = "before_tokens"
        case afterTokens = "after_tokens", savedTokens = "saved_tokens"
        case summaryChars = "summary_chars"
    }
}

struct Todo: Decodable {
    let content: String
    let activeForm: String?
    let status: String              // pending | in_progress | completed
    enum CodingKeys: String, CodingKey { case content, status, activeForm = "active_form" }
}

// ── 握手 / 控制（按 type 分流后解码）──
struct Hello: Decodable { let type: String; let protocolVersion: Int; let server: String?
    enum CodingKeys: String, CodingKey { case type, server, protocolVersion = "protocol_version" } }

struct ApprovalRequest: Decodable {
    let type: String; let id: String
    let toolName: String?; let toolArgs: JSONValue?; let deadlineMs: Int?
    let sessionId: String?; let turnId: String?     // v1 可能缺省
    enum CodingKeys: String, CodingKey {
        case type, id, toolName = "tool_name", toolArgs = "tool_args"
        case deadlineMs = "deadline_ms", sessionId = "session_id", turnId = "turn_id" }
}

// ── 入站（client → server，Encodable）──
struct SendMessage: Encodable { let type = "send_message"; let text: String }
struct CancelTurn: Encodable { let type = "cancel_turn" }
struct ApprovalResponse: Encodable { let type = "approval_response"; let id: String; let approved: Bool }

// JSONValue：解码任意 JSON（tool_args 是对象）。用你项目里已有的 AnyCodable，
// 或一个最小的递归枚举即可。
```

分流入口（伪代码）：

```swift
let obj = try JSONSerialization.jsonObject(with: data) as? [String: Any]
if let t = obj?["type"] as? String {       // 控制/握手
    switch t {
    case "hello": …
    case "approval_request": showApprovalCard(decode(ApprovalRequest))
    default: break
    }
} else if obj?["kind"] is String {         // 事件
    render(decode(WireEvent))
}                                           // 其它：忽略（前向兼容）
```

---

## 9. 最小连接示例（流程）

```
1. POST /v1/conversations {workspace_path}      → 拿 id
2. 连 ws://host/v1/conversations/<id>/stream
3. 读第一帧 → 校验 type=="hello" && protocol_version==1
4. 起读循环：按 §3.1 分流 → 渲染事件 / 弹审批卡片
5. 发 {type:send_message,text:...} 驱动一轮
6. 收到 approval_request → 回 {type:approval_response,id,approved}
7. 收到 turn_finished → 允许下一轮
8. 断开 = 放弃该控制连接；该会话副作用恢复拒绝态
```
