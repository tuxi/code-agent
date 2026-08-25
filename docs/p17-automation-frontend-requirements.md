# 自动化（Automation）前端需求文档

状态：需求定义（供 chater / AgentKit 前端实现）
关联文档：`docs/p15-scheduled-tasks-prd.md`（服务端 PRD）、`docs/p16-automation-task-breakdown.md`（拆分）
目标读者：AgentKit SDK（AutomationDashboardView 组件）、chater（宿主 App 交互）

> **对接基线**：本文接口契约已逐条对照 code-agent 服务端实现（`internal/server/automations.go`、
> `internal/automation/types.go`、`internal/automation/scheduler.go`）核对。
> 有两处与旧稿不一致，已按服务端实际行为修正，前端**以本稿为准**：
> 1. `GET /{id}/runs` 的 Run 字段是 **PascalCase**（Go 默认 JSON），非 snake_case（见 §2.3）。
> 2. 服务端**没有**"run 标记已读"端点，`ReadAt` 恒为零值（见 §4.5）。

---

## 1. 背景与目标

code-agent 服务端已完成自动化（Automation / 定时任务）全链路：模型工具（`automation` +
`get_current_time`）、调度引擎（daemon 内 30s ticker）、REST 控制面端点（`/v1/automations`）、
运行历史（`automation_runs`）。**服务端能力已就绪，前端尚未实现 UI。**

前端现状：
- AgentKit：`Sources/AgentKit/Core/Automation/AutomationDashboardView.swift` 是**空壳**（`body` 为空）。
- chater：已接入导航入口（`AgentNavigationDestination.automation` → `AutomationDashboardView`，
  `WorkspaceView.swift:163/271`、`SidebarView.swift:89`）。
- AgentKit 已有完整 HTTP 基建：`RuntimeClient` 协议 + `CodeAgentTransport` + `RuntimeHTTPClient`
  （`buildRequest` / `validateHTTP` / `decodeEnvelope` 模式），新增端点照现有模式扩展即可。

**目标**：把 `AutomationDashboardView` 实现为完整的自动化控制面板 —— 任务列表、创建、编辑、
权限配置、运行历史、触发后的会话跳转，对齐 WorkBuddy「Scheduled Tasks」面板与豆包「进行中」列表。

---

## 2. code-agent 接口规范（对接依据）

### 2.1 通用响应信封

所有非 204 响应都包在信封里（AgentKit 的 `decodeEnvelope` 已处理）：

```json
{ "trace_id": "...", "code": 0, "msg": "success", "data": { ... } }
```

错误时 `code` 非 0、`msg` 为错误描述。DELETE 返回 204 无 body。

### 2.2 端点清单

| 方法 | 路径 | 请求 body | 响应 data |
|---|---|---|---|
| GET | `/v1/automations` | — | `[AutomationDTO]` |
| POST | `/v1/automations` | `AutomationCreateRequest` | `AutomationDTO` (201) |
| GET | `/v1/automations/{id}` | — | `AutomationDTO`（已删除返回 404） |
| PATCH | `/v1/automations/{id}` | `AutomationPatchRequest` | `AutomationDTO` |
| DELETE | `/v1/automations/{id}` | — | 204 无 body |
| GET | `/v1/automations/{id}/runs` | — | `[RunDTO]` |

### 2.3 DTO 定义（服务端 `internal/server/automations.go`）

**AutomationDTO**（GET/POST/PATCH 返回）：

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | string | 任务 id（`auto_<nano>`） |
| `name` | string | 任务名（列表标题） |
| `prompt` | string | 每次运行的指令 |
| `status` | string | `ACTIVE` / `PAUSED` / `COMPLETED`（once 已执行完） |
| `schedule_type` | string | `once` / `recurring` |
| `rrule` | string | recurring 规则，如 `FREQ=DAILY;BYHOUR=16;BYMINUTE=0` |
| `scheduled_at` | string | once 的时间点（RFC3339，可能为空） |
| `timezone` | string | IANA 时区，如 `America/Los_Angeles` |
| `mode_exec` | string | `standalone` / `chat` |
| `session_id` | string | chat 模式回哪个会话；standalone 空 |
| `cwds` | [string] | 目标工作目录 |
| `model_id` | string | 可选模型 |
| `skills` | [string] | 运行时自动加载的 skill |
| `connectors` | [string] | 免确认的 MCP 连接器名 |
| `permission_mode` | string | `full_access` / 空（默认） |
| `created_from_workspace` | string | 创建者会话 workspace（standalone 回退） |
| `last_run_at` | string | RFC3339，可能为空 |
| `next_run_at` | string | RFC3339，可能为空（once 完成后为空） |
| `run_count` | int | 已运行次数 |
| `last_status` | string | 最近一次 run 状态 |
| `created_at` / `updated_at` | string | RFC3339 |

**AutomationCreateRequest**（POST body，字段同 DTO，均为可选 + `enabled` bool 可省略）：
```json
{
  "name": "每日科技股行情",
  "prompt": "每天下午4点整理科技行业股票今日行情…",
  "schedule_type": "recurring",
  "rrule": "FREQ=DAILY;BYHOUR=16;BYMINUTE=0",
  "scheduled_at": "2026-08-26T15:00:00-07:00",
  "timezone": "America/Los_Angeles",
  "mode_exec": "standalone",
  "session_id": "",
  "cwds": [],
  "model_id": "",
  "skills": [],
  "connectors": ["github"],
  "permission_mode": "",
  "enabled": true
}
```
服务端校验：name/prompt/timezone 必填；once 需 `scheduled_at`；recurring 需 `rrule`；
chat 需 `session_id`。非法返回 400。

**AutomationPatchRequest**（PATCH body）：所有字段可选，**未填字段保持不变**（workbuddy 语义）。
`enabled`（bool）→ `status`（true=ACTIVE，false=PAUSED）。同字段名即可。

**RunDTO**（`GET /{id}/runs`，`data` = `[]automation.Run`，**Go 默认 JSON，PascalCase 键**）：

> ⚠️ **键名是 PascalCase，不是 snake_case**。`automation.Run` 结构体（
> `internal/automation/types.go`）**没有 json tag**，序列化直接用字段名。前端 Swift 模型
> 的 `CodingKeys` 必须按下表——AgentKit 的 `JSONDecoder()` 默认**按大小写精确匹配**，
> 写错键名会静默解码失败（字段全为默认值），不会报错。

| 字段 | 类型 | 说明 |
|---|---|---|
| `ID` | string | run 独立 id（非会话 id） |
| `AutomationID` | string | 所属任务 id |
| `SessionID` | string | standalone=本次触发新建的会话 id；chat=该次投递的 turn id（跳转会话请用 automation 自身的 `session_id`） |
| `Status` | string | `running` / `succeeded` / `failed` / `skipped` |
| `ReadAt` | string | RFC3339；**零值 = `0001-01-01T00:00:00Z`，即未读**（不会是空串） |
| `ThreadTitle` | string | 会话标题（可能为空） |
| `SourceCWD` | string | 触发时的工作目录（可能为空） |
| `ResultSuccess` | bool | 是否成功 |
| `ResultSummary` | string | 结果摘要（可能为空） |
| `CreatedAt` | string | RFC3339 |

> 额外约束：
> - runs 端点**固定返回最近 50 条**（`store.ListRuns(ctx, id, 50)`），无分页。
> - `ReadAt` / `CreatedAt` 是 `time.Time`，零值序列化为 `0001-01-01T00:00:00Z`。
>   Swift 端判"未读"请用 `ReadAt != "0001-01-01T00:00:00Z"`（或先解码成 Date 再判零值），
>   不要把空串当未读信号。

### 2.4 状态流转

```
ACTIVE ──(enabled:false)──▶ PAUSED
PAUSED ──(enabled:true)───▶ ACTIVE
ACTIVE(once) ──触发完成──▶ COMPLETED（next_run_at 置空，不再触发）
```

### 2.5 触发后的会话

- **standalone**：每次触发 daemon 新建一个会话（workspace = `cwds[0]` 或创建者 workspace），
  该会话出现在 `/v1/conversations` 列表，`Run.SessionID` 可定位。
- **chat**：每次触发回到 `automation.session_id` 指定的会话继续。
- 前端可用既有 `getConversationDetail` / 会话详情流查看触发产生的对话内容。

---

## 3. AgentKit 扩展点（接口对接层）

按现有 `RuntimeClient`/`AgentTransport`/`CodeAgentTransport`/`RuntimeHTTPClient` 四层模式扩展：

1. **模型**（新建 `Core/Automation/` 下）：
   - `AutomationSummary` / `Automation`（对齐 AutomationDTO 字段）
   - `AutomationCreateRequest` / `AutomationPatchRequest`（对齐 JSON）
   - `AutomationRun`（对齐 RunDTO）
2. **RuntimeHTTPClient**：新增
   - `listAutomations() -> [AutomationSummary]`（GET `/v1/automations`）
   - `createAutomation(_:) -> Automation`（POST，期望 201）
   - `getAutomation(id:) -> Automation`（GET `/v1/automations/{id}`）
   - `updateAutomation(id:_:) -> Automation`（PATCH）
   - `deleteAutomation(id:)`（DELETE，204）
   - `listAutomationRuns(id:) -> [AutomationRun]`（GET `/v1/automations/{id}/runs`）
3. **AgentTransport 协议**：加上述方法（默认实现 `throw RuntimeHTTPError.unsupported`，对齐现有模式）。
4. **CodeAgentTransport**：实现（转发到 http client）。
5. **RuntimeClient 协议 + DefaultAgentClient facade**：透传。
6. **状态/ViewModel**：`AutomationDashboardViewModel`（`@MainActor`，负责加载/刷新/操作/错误态）。

---

## 4. UI/交互需求（AutomationDashboardView）

### 4.1 布局

对齐 WorkBuddy「Scheduled Tasks」+ Codex「已安排的任务」：

- **顶部**：标题「自动化」+ 搜索框 + 「创建」按钮（右上）。
- **标签页**：全部 / 进行中 / 已暂停 / 已完成（各过滤 `status`，已完成=COMPLETED）。
- **任务卡片**（每行）：
  - 主标题：`name`
  - 副标题：调度描述（人类可读，见 §4.4）+ 工作目录（有 cwds 时）
  - 状态徽标：进行中（ACTIVE）/ 已暂停（PAUSED）/ 已完成（COMPLETED）
  - 右侧：下次触发时间 `next_run_at`（本地化显示）或「已完成」
  - 右键/`•••` 菜单：查看 / 编辑 / 暂停(启用) / 删除
- **空态**：无任务时显示引导文案 + 「创建第一个自动化」按钮。

### 4.2 创建流程（两种模式）

对齐 WorkBuddy（对话式创建）+ Codex（表单式创建）双入口：

**模式 A：对话式（推荐，主入口）**
- 点「创建」→ **把一段引导 prompt 预填进输入框**（对齐 Codex 截图）：
  > "我来帮你创建自动化任务。请告诉我：1) 要做什么（例如"每天下午4点整理科技股行情"）；2) 什么时候运行（每天/每周/一次性+时间）；3) 在哪个工作目录运行；4) 是否需要 full access 权限。"
- 用户用自然语言描述 → 发送 → 模型（已装 `automation` skill）通过 `ask_user` 确认关键信息 → 调 `automation` 工具创建 → 把结果（含任务详情卡片）回复在对话中。
- 这种模式下，**前端不直接调 REST 创建**，而是走对话（复用现有发送消息流）。任务列表靠刷新看到新任务。

**模式 B：表单式（可选）**
- 点「创建」旁边的下拉/长按 → 直接出表单（名称 / Prompt / 调度类型(once|recurring) / rrule 或时间 / 时区 / 工作目录 / 权限）→ 前端调 `createAutomation`。
- 表单字段与 §2.3 对齐，校验与服务端一致（once 需 scheduled_at、recurring 需 rrule）。

### 4.3 详情 / 编辑页

点任务卡片进入（WorkBuddy 编辑页对齐），**未开始（未触发）的任务可完整修改**：

- **基本信息**：Name / Prompt（多行编辑）/ Workspace（选择器）
- **调度**：Schedule Type（Specific Time / Repeats Every）+ 时间选择 + 时区显示（只读提示）
- **权限**（对齐 WorkBuddy "Full access / Connectors"）：
  - 权限级别选择：默认权限 / Full access（`permission_mode=full_access`）
  - 连接器免确认：可多选的 connectors 列表（来自 `/v1/...` 现有 connector 数据源，chater 侧提供）
  - 说明文案："选中后该连接器在任务运行时无需确认"
- **Skills**（可选）：运行时要加载的 skill 列表
- **模型**（可选）：`model_id`
- **Save / Cancel**：Save 调 `updateAutomation`（只传改动字段）
- **删除**：底部「删除任务」→ 确认 → `deleteAutomation`

### 4.4 调度描述人类可读化

前端把 `schedule_type` + `rrule`/`scheduled_at` 渲染成中文描述（供列表副标题）：
- `once` + `scheduled_at=2026-08-26T15:00:00-07:00` → "2026/8/26 15:00 执行一次"
- `recurring` + `FREQ=DAILY;BYHOUR=16;BYMINUTE=0` → "每天 16:00"
- `FREQ=WEEKLY;BYDAY=MO;BYHOUR=9` → "每周一 09:00"
- `FREQ=MINUTELY;INTERVAL=30` → "每 30 分钟"
- 附加 `（America/Los_Angeles）` 时区提示（有 tz 时）
- 解析失败 → 回退显示原始 rrule 字符串，不崩溃

### 4.5 Run History（运行历史）

对齐 WorkBuddy「Run History」：
- 详情页内嵌「运行历史」区：按 `CreatedAt` 倒序列出 `AutomationRun`（`GET /v1/automations/{id}/runs`）
- 每行：状态徽标（进行中/成功/失败/已跳过）+ `CreatedAt` + `ResultSummary`（有则显示）
- standalone 的 run：点击跳转到该会话详情（`SessionID` → 既有会话详情流）
- **未读指示**：**服务端 runs 端点当前只读**，没有"标记已读"的 HTTP 端点
  （`internal/server/automations.go` 只有 6 个路由，无 `runs/{id}/read`）。
  判断未读用 `ReadAt` 为零值（`0001-01-01T00:00:00Z`）。`ReadAt` 恒为零值是因为
  调度器写 run 时不填 `ReadAt`（`internal/automation/scheduler.go` 的 `RecordRun` 只填
  `AutomationID/SessionID/Status/CreatedAt`），因此**未读指示本期以本地会话状态为准**，
  `ReadAt` 字段作为服务端预留；不能依赖服务端标记已读。

### 4.6 触发后展示（自动开一轮对话）

- **standalone 触发**：daemon 自动新建会话并投递 turn。前端在会话列表/详情能看到新会话。
  建议：任务详情页对最近一次 run 显示「查看本次运行」→ 跳转该会话。
- **chat 触发**：任务回到原会话。若用户正在该会话，应能看到新消息流实时到达
  （复用现有 WS 事件流）。
- **触发时提示**：任务运行期间显示「自动化运行中，请勿关机或退出客户端」（对齐 WorkBuddy Notice），
  可放详情页顶部或列表顶部横幅，运行中（`Run.Status==running` 或会话 turn 进行中）时显示。

### 4.7 刷新策略

- 进入页面 / 切 tab / 从创建返回 → 调 `listAutomations` 刷新。
- 编辑保存后 → 刷新详情 + 列表。
- 可选：WebSocket 事件订阅（若服务端后续推送 automation 事件）——本期以"动作后 GET"为准。

---

## 5. 交互细节与边界

- **删除确认**：删除是软删除，但前端仍应弹确认（防误触）。
- **暂停 vs 删除**：暂停（enabled:false）保留任务可恢复；删除不可恢复（软删但列表不再显示）。
- **once 已完成**：COMPLETED 任务显示在「已完成」tab，卡片显示完成时间而非下次触发。
- **错误处理**：网络错误/400/404 → 用 AgentKit 现有错误模型（`RuntimeHTTPError`）展示，
  不崩溃；列表加载失败显示重试。
- **时区**：`next_run_at`/`scheduled_at` 都是 RFC3339（含偏移），前端用本地时区渲染。
- **并发**：PATCH 与刷新竞态——保存后以返回的 DTO 为准更新本地状态。

---

## 6. 分阶段建议

| 阶段 | 内容 | 依赖 |
|---|---|---|
| P1 | 列表页（标签+卡片+状态徽标）+ `listAutomations` 对接 | RuntimeHTTPClient 扩展 |
| P2 | 创建（对话式引导预填 + 表单式）+ `createAutomation` | 对话流复用 |
| P3 | 详情/编辑 + 权限配置 + `updateAutomation`/`deleteAutomation` | P1 |
| P4 | Run History + 会话跳转 + 触发提示 | P1 + 会话详情流 |

---

## 7. 验收标准

1. 列表页能加载并展示 code-agent 返回的全部任务（含 ACTIVE/PAUSED/COMPLETED 三态）。
2. 创建：对话式引导能完成一次自然语言创建（经模型 `automation` 工具）；表单式直接调 REST 创建成功。
3. 编辑：未开始任务可修改 name/prompt/调度/权限，保存后列表与详情一致；暂停/启用即时生效。
4. 删除：软删除后列表移除，再次进入显示 404 兜底。
5. Run History：触发过的任务能看到运行记录，standalone run 可跳转会话语。
6. 一次性任务触发后显示「已完成」，不再显示下次触发时间。
7. 权限配置（Full access / connectors）能保存并在任务详情回显。
8. 调度描述人类可读化正确（daily/weekly/minutely/once 各至少一例）。
9. 全程无崩溃、无未处理错误；AgentKit 侧单测覆盖 DTO 解码与描述渲染。
