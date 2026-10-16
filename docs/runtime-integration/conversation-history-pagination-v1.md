# Conversation History Pagination v1（历史对话分页接口 + 客户端改造设计）

> 状态：Draft v1，待评审
> 参与方：code-agent(Runtime/后端) + AgentKit(macOS/iOS 客户端)
> 背景：ConversationDetailView 在长会话下看不到早期历史

## 0. 问题

AgentKit 加载历史走 `GET /v1/conversations/{id}/events?since=0`。code-agent 在 `since=0`
时用 8MB payload 预算只返回最新的尾部，更早的事件被丢弃，仅通过响应头
`X-Codeagent-Replay-Truncated: 1` 提示——客户端当前忽略该头，于是早期历史静默丢失。

目标：分页 + 下拉加载更多——首屏只加载最新一页，向上滚动按需加载更早的页，
能翻到全部历史，且不破坏断线续传。

## 1. 已核实事实

- `session_events(id INTEGER PRIMARY KEY AUTOINCREMENT, session_id, turn_id, kind, at, payload)`
  （`internal/session/sqlite/store.go:142-149`）。`id` 跨会话全局自增。
- `/v1/conversations/{id}/events` → `loadEventsSince`（`internal/server/mux.go:1117-1140,1773-1798`）：
  - `since==0` 且支持 EventBudgetStore → `SessionEventsSinceBudget(id,0,8MB)`（`store.go:782-821`），
    newest-first 累加 payload，超过预算即丢更早的，`truncated=true` → 响应头
    `X-Codeagent-Replay-Truncated: 1`（`mux.go:1788-1790`）。
  - `since>0` → `ReplaySince`（`store.go:766-773`），**无预算**（增量回放刻意不设限，避免客户端漏 gap）。
- 单会话分区内排序 = `id ASC`（=seq ASC），跨会话 id 有洞但会话内严格递增 → **裸 seq 可作分页游标**。
- ⚠️ 契约 `docs/protocols/runtime-event-contract-v1.md` §9.4 写的「session_id 优先分组再 (at,seq)」
  与实际实现不符，本设计以代码为准，并需同步修正 §9.4。
- 子流 bracket（job/task 的 started/finished）双写进父会话分区、盖父会话 seq
  （`internal/runtime/jobs_sink.go:148-158`）→ 父会话 `/events` 属单一 session_id，可按 `turn_started` 对齐。

## 2. 接口设计（v1，向后兼容）

保持 `/events` 现有裸数组响应不变（`writeJSON(w, r, 200, frames)`，`mux.go:1139`），
新增查询参数 + 响应头。新增参数不影响老客户端。

### 2.1 查询参数

| 参数 | 语义 |
|------|------|
| `since=N`（现有） | 返回 `seq > N` 的事件，升序，**不设上限**（断线补缺口）。保持不变。 |
| `tail=N`（新） | 首屏：返回最新的、turn 对齐的约 N 条事件，升序。 |
| `before=C`（新） | 返回 `seq < C` 的更早一页（校准后约 `limit` 条），升序。`C` 取自上一页响应头 `X-Codeagent-Oldest-Seq`。 |
| `limit=N`（新） | 一页最大事件条数的软上限（默认 200）。`tail`/`before` 生效；`since` 可选生效。 |

约束：`since` 与 `before` 互斥，同时出现返回 400；`tail` 仅在没有 `since`/`before` 时生效。
无任何参数（`since=0`）保持现状（8MB 预算 + truncated 头），保证老客户端行为不变。

### 2.2 响应头（新增）

| 头 | 语义 |
|----|------|
| `X-Codeagent-Oldest-Seq: <n>` | 本页最小 seq。下一页用 `before=<n>`。 |
| `X-Codeagent-Has-More-Older: 0|1` | 是否存在比本页最小 seq 更早的事件（`EXISTS(SELECT 1 ... id < n)`）。 |
| `X-Codeagent-Replay-Truncated: 1`（现有） | 因字节/条数硬上限导致本页仍被截断（罕见，仅单个 turn 超大时）。 |

响应体仍是 `[WireFrame]` 数组（帧含 `seq`，`mux.go:1132-1137`）。

### 2.3 turn 对齐（关键）

- 一个 turn 从 `turn_started` 起、到下一个 `turn_started` 前止。
- `tail`/`before` 返回的最老一行必须是 `turn_started`；若取满 `limit` 后最老行不是
  `turn_started`，**丢弃该页前面这个不完整的 turn**（它会在下一页完整返回），
  这样客户端 prepend 时不会出现半截 turn。
- `has_more` 按「丢弃后最小 seq 之前是否还有行」计算，丢弃不影响正确性。
- 硬上限：单个 turn 超过 `limit` 或字节上限时允许截断并置 `truncated=1`（极端情况兜底）。
- 边界：会话最前面可能有不属于任何 turn 的事件（如 turn_inputs）。当已到最老端时，
  这些前导事件应在最后一页完整返回，不得被无限丢弃。

### 2.4 服务端实现要点

- 新增 store 方法 `SessionEventPage(ctx, sessionID, beforeSeq int64, maxRows int, maxBytes int64) ([]EventRecord, bool /*hasMoreOlder*/, bool /*truncated*/, error)`：
  `ORDER BY id DESC LIMIT maxRows(+turn 对齐回退)`，累加 payload 受 maxBytes 约束，最后反转为升序。
- handler：解析 `tail/before/limit`，分派到新方法或既有 `ReplaySince`；设置响应头。
- 性能：评估 `before` 查询计划，必要时新增索引 `idx_session_events_session_id(session_id, id)`。
- golden 测试：新增 `/events?tail=` 与 `?before=` 的响应快照 + 响应头断言，
  以及 turn 对齐（不返回半截 turn）与 `has_more` 的用例。

### 2.5 能力协商

`hello.capabilities` 可选新增 `history_pagination`，客户端据此启用「下拉加载更多」；
缺失则退回现状（可提示「早期历史可能不完整」）。

## 3. 客户端（AgentKit）改造要点

### 3.1 共享层

- `AgentEventBatch`（`Sources/AgentKit/Core/ChildStream.swift:20`）增加可选
  `hasMoreOlder: Bool`、`oldestSeq: Int?`（默认 false/nil，向后兼容）。
- `AgentTransport` 增加 `getOlderEventBatch(conversationID:before:limit:)`（默认 unsupported）。
- `RuntimeHTTPClient.getEvents` 增加 `before/limit/tail` 变体并读取响应头。
- `ConversationViewModel.fetchHistory`：首屏改用 `tail=<pageSize>`，`since` 续传游标仍取 `nextSince`（最大 seq）。
  新增 `loadOlderHistory()`：`before=oldestSeq` 取更早一页 → `engine.importOlderHistory(events)`。
- `RuntimeEngine` 增加 `importOlderHistory`：**不要** reset 现有 graph/cache（会打掉所有 contentVersion）。
  把更早事件归约进一个**独立** graph，投影为 turns，作为 `backfillPrefix` 前插；
  `RuntimeSnapshot.turns = backfillTurns + liveTurns`。直播 graph 与 since 游标完全不受影响。

### 3.2 macOS Web 端

- `main.tsx` `applyOperations` 增加 `prependTurns`；`ConversationWebUpdate.Operation` 增加对应 kind。
- `ConversationWebDocumentDiffer`：识别 prepend（`old.turns == new.turns.suffix(old.count)`），
  发 `prependTurns` 而非退化成 `reset`（当前前缀不匹配即 reset，`ConversationWebDocumentDiffer.swift:281-288`）。
- JS 在接近顶部时通过现有 bridge 上报「reachedTop」（尊重 pin 状态，`main.tsx:491-694`）；
  复用 §12.3 unpinned anchor 保持滚动位置。

### 3.3 macOS 原生（AppKit NSTableView）

- `replaceRows`（`MacNativeChatTimeline.swift:652-688`）增加 prepend 检测（old 是新数组的后缀），
  返回「头部插入 N 行」，走 `NSTableView.insertRows(at:)` + 补偿 `contentView` 滚动偏移，
  而非 `reloadAllRows`（避免清空 heightCache 与全量重测）。

### 3.4 iOS

- `TurnTimelineView`（`TurnTimelineView.swift:94-135`）顶部加 sentinel + `onAppear` 触发加载；
  用 `ScrollViewReader` 记录插入前顶部可见 turn 的 id/offset，插入后回到同一锚点。
  SwiftUI LazyVStack 头部插入锚定需实机验证。

## 4. 兼容性

- 老客户端不受影响：默认（无参数）行为不变；新增参数/响应头被忽略。
- 老服务端不受影响：客户端先探测 `history_pagination` capability 再启用新路径。
- 契约文档 `runtime-event-contract-v1.md` 需补充 §4.3/§9.4 修正与新参数/响应头说明。

## 5. 分阶段

- P1（后端）：`tail/before/limit` + 响应头 + turn 对齐 + has_more + 索引 + golden 测试。
- P2（客户端共享层）：信封字段、transport、engine 前缀分区、首屏 tail、loadOlderHistory、capability 探测。
- P3（客户端渲染）：macOS 原生 → iOS 的 prepend + 锚定。
- P4（客户端渲染）：macOS Web 的 prepend operation + differ + reachedTop。
- P5（可选）：窗口化/淘汰，真正给内存设上限（分页本身不降低稳态内存）。

## 6. 验收标准

- 长会话（事件 payload > 8MB）：首屏只加载最新一页；向上滚动可逐页加载直至 turn 1；无静默丢失。
- 断线重连行为不变（`since=maxSeq` 补缺口）。
- prepend 不触发全量重建：Web 端 DOM/滚动/选区保留；AppKit 端已有行 identity 保留；iOS 不跳动。
- 服务端：turn 对齐（不返回半截 turn）、`has_more` 正确、golden 快照通过。