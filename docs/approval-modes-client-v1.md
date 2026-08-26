# 权限客户端接入需求梳理（AgentKit / App）

> 目标读者：AgentKit（iOS/macOS Swift）、Web 客户端开发者。回答"权限存哪里、
> 对话/设置里各能配什么、调哪些 API"，并列出后端后续待优化项。

---

## 1. 权限模型全景（先回答"存哪里"）

当前后端有三类权限，**存储位置各不相同**：

| 权限 | 语义 | 存储位置 | 谁来写 |
|------|------|---------|--------|
| **审批档位** `approval_mode`（`ask` / `auto` / `full`） | 未命中规则时怎么办：全部请求批准 / **工作区内文件写入+非网络命令自动，网络命令、MCP 工具、工作区外路径询问** / 全部自动（硬底线仍生效） | **workspace 级**：`<workspace>/.codeagent/settings.local.json` 顶层 `approval_mode`（合并层 user → shared → local，local 最高） | 客户端 `/v1/workspaces/permissions` PUT、TUI `/mode` |
| **逐条规则** `permissions.allow/deny`（glob，deny 优先） | 审批卡 "Always allow" 落盘；命中即免审批/拒绝 | 三层 union：`~/.codeagent/settings.json`（user）+ `<workspace>/.codeagent/settings.json`（shared）+ `<workspace>/.codeagent/settings.local.json`（local） | 审批卡 `approval_response {decision:"always", scope}` |
| **自动化任务档位** `permission_mode`（`ask`/`auto`/`full`，`full_access`=旧别名） | 该任务每次触发时的档位；`""` = 继承 workspace 档位 | **per-task**：`~/.codeagent/automations.db` 的 `automations` 表（不是 settings 文件） | `/v1/automations` create/update |

**关键结论（回答疑问）：**
- **对话详情页设置权限 ≠ 存在 session 表**。会话（session）只通过 `workspace_path` 归属到某个 workspace；对话详情页设置的档位/规则，实际落在**该 workspace 的 `.codeagent/settings.local.json`**，对**运行在同一 workspace 的所有会话**生效（同 workspace 多个对话共享同一档位）。
- **设置中的权限目前是 per-workspace**：后端只有 `GET/PUT /v1/workspaces/permissions/{path}`。**user 全局写端点尚未实现**（读已支持：workspace 没设时 GET 会回落到 user 层），后端待优化项见 §5。
- **自动化任务不受 workspace 档位影响**（除非 `permission_mode=""` 显式继承），它有自己的 per-task 档位。

---

## 2. 客户端四个接入入口

### 入口 A — 对话内审批卡（✅ AgentKit 已实现，2026-08-26 代码核实）

后端：`approval_request`（server→client）→ `approval_response {decision:"once"|"always"|"deny", scope:"local"|"user"}`。
协议：`docs/protocols/agent-wire-v1-approval-three-way.md`。**AgentKit 迁移清单 7 项 iOS 工作已全部完成**：
`OutgoingApprovalResponse` decision+scope（`WireFrame.swift`）、三按钮卡 + scope 下拉（`ApprovalBar`）、MCP server 聚合提示、按 id 去重（`resolvedApprovalIDs`）、`auto_approved` 低调展示（ToolCard bolt 图标）、plan 保持两态。
协议文档 §8 的 ⬜ 标记已过时，应更新为 ✅（AgentKit 侧已更新）。

弹卡行为按档位区分：
- **ask 档**：每次副作用工具调用弹卡（未命中 allow 规则时）。
- **auto 档**：工作区内文件写入 + 非网络命令自动放行（收 `auto_approved` 事件低调展示）；**网络命令、MCP 工具、工作区外路径仍弹卡**。
- **full 档**：全部自动（硬底线仍生效），只收 `auto_approved` 事件。
- **Always allow** 落盘 allow 规则：scope=local → workspace settings.local.json；scope=user → `~/.codeagent/settings.json`。MCP 工具聚合为 `mcp__<server>__*`（一个 server 确认一次）。

### 入口 B — 对话详情页设置权限（新功能）

用户进入对话详情，看到该对话当前生效档位，可切换。

- **获取**：`GET /v1/conversations/{id}` → `workspace_path`（或 `workspace.root_path`）→ `GET /v1/workspaces/permissions/{workspace_path}`
  ```json
  { "scope": "workspace", "path": "/abs/workspace", "available": ["ask","auto","full"], "mode": "auto" }
  ```
  - **scope 恒为 `"workspace"`**（v1 硬编码），`mode` 是合并后的有效值（含 user 层 fallback）。
  - **响应无来源字段**：客户端无法区分「该 workspace 自定义了档位」vs「继承 user 全局」。后端待加（§5）。
  - **URL 编码**：绝对路径按 `/` 自然分段拼到 `/v1/workspaces/permissions` 之后（`/v1/workspaces/permissions/Users/me/proj`），**不要整体 percent-encode**。
- **设置**：`PUT /v1/workspaces/permissions/{workspace_path}` body `{"mode":"auto"}`
  - 只写顶层 `approval_mode`，不碰 allow/deny 规则。
  - **不校验 workspace 存在性**：任意路径都会自动创建 `.codeagent/settings.local.json`（返回 200）。客户端必须传真实 workspace path（从 conversation detail 拿），避免凭空生成文件。
  - 错误：mode 非法 → 400；body 非 JSON → 400。
- **UI**：三选一 radio/分段控件，文案对齐 Codex：请求批准 / 帮我批准 / 完全访问。
- **生效时机**：后端每 turn 重读磁盘，PUT 后**下一个 turn 边界生效**（当前 turn 不受影响）；无需重启、无需重连。
- **注意**：改的是 workspace 档位，**影响该 workspace 的所有对话**——UI 需提示"将应用于此工作区的所有对话"。

### 入口 C — 设置页权限（全局 vs 每工作区）

**现状**：后端只有 per-workspace 端点。Talkify 已有完整设置页（`../chater/Talkify/Settings/SettingsView.swift`，9 个 section，含"配置"文件工作区）；`SettingsDetailView.generalSettings` 中留有未接线的权限 toggle 占位（defaultPermission/autoApproval/fullDiskAccess，`@AppStorage` 本地键，非后端字段）。设置页新增"权限"section，先做"当前 workspace 档位"（复用入口 B 的 API；workspace 取当前活跃会话的），可顺带清理占位代码。

**若要"全局权限"**（用户希望 App 一处设置全局档位）：后端需补 user 级端点（§5 待优化项 1）：
- `GET /v1/permissions` → `{scope:"user", mode:"auto"}`（user 层档位；生效于所有未单独设置档位的 workspace）
- `PUT /v1/permissions` `{"mode":"auto"}` → 写 `~/.codeagent/settings.json`
- 合并语义（后端已支持读）：workspace local > shared > **user** > ask。设置页应同时展示：全局档位 + 当前 workspace 覆盖状态（"使用全局设置"或"自定义"）。

**每工作区权限列表**（可选，设置页做"工作区管理"）：GET /v1/conversations 可列出所有 workspace（`workspace_path` 去重），逐个查/设档位。后端无批量端点，客户端循环调用即可（工作区数量有限）。

### 入口 D — 自动化任务的权限（创建/编辑自动化时）

后端：`/v1/automations` create/update 的 `permission_mode` 字段（canonical `"full"`，`full_access` 为旧别名会被归一；`ask`/`auto` 均接受）。
- **Talkify 表单已有"权限级别"选择器**（`AutomationDashboardView.swift` create/edit 均有），但当前只有 `""`（继承）和 `"full_access"`（旧别名）两个选项。需：① 增加 `ask`/`auto` 选项；② 改发 canonical `"full"`（回显一致）。
- 默认值：文档建议 **full**（无人值守，否则卡审批）；当前客户端默认是 `""`（继承 workspace 档位）——两者语义不同，需产品决策。
- 注意 automation 的档位是 per-task，与 workspace 档位独立；`""` 才会继承 workspace。

---

## 3. API 一览（客户端要用到的）

| 端点 | 用途 |
|------|------|
| `GET /v1/conversations/{id}` | 拿 `workspace_path` |
| `GET /v1/workspaces/permissions/{path...}` | 查某 workspace 有效档位（含 user fallback） |
| `PUT /v1/workspaces/permissions/{path...}` | 设某 workspace 档位 |
| `GET /v1/permissions` | **待后端实现**：查 user 全局档位 |
| `PUT /v1/permissions` | **待后端实现**：设 user 全局档位 |
| `GET/POST/PATCH/DELETE /v1/automations[...]` | 自动化 CRUD（含 `permission_mode`） |
| WS `approval_request` / `approval_response` | 对话内审批（三态 + scope） |
| WS `auto_approved` 事件 | auto/full 档下的自动放行审计展示 |

---

## 4. 建议实施顺序（客户端）

1. **入口 B 对话详情页档位**（ask/auto/full 三选一）——后端已就绪；用 conversation detail 的 workspace_path（detail 当前**不带** approval_mode 字段，需二次请求 permissions 端点）。AgentKit 侧需在 RuntimeHTTPClient 新增 GET/PUT permissions 方法 + DTO + 对话详情 UI。
2. **入口 D 自动化权限字段**——表单已有选择器，更新为：增加 ask/auto 选项、发送 canonical `"full"`（当前发旧别名 `"full_access"`）。
3. **入口 C 设置页**——Talkify SettingsView 已存在（`../chater/Talkify/Settings/`），新增"权限"section，先做"当前 workspace"档位（复用 B 的 API）；全局档位等后端补 user 端点后加。
4. **入口 A 收尾**——三态审批卡已实现，仅剩 QA/回归验证；并更新协议文档 §8 迁移清单 ⬜→✅（AgentKit 侧已更新）。

---

## 5. 后端待优化项（本轮未做，梳理供后续）

1. **user 全局档位端点**：`GET/PUT /v1/permissions`（scope=user，写 `~/.codeagent/settings.json`）。读取链路已通（settings.Load 含 user 层），只缺写端点 + DTO scope 字段扩展（permissionResponse 已有 scope 字段，可扩展 `"user"`）。
2. **GET 响应的来源字段**：当前 `GET /v1/workspaces/permissions/{path}` 的 `scope` 恒为 `"workspace"`、`mode` 是合并后的有效值，**没有来源信息**——客户端无法区分「workspace 自定义档位」vs「继承 user 全局」。需在响应加 `source: "local"|"shared"|"user"|"inherited"`（或在 user 端点实现后由客户端自行对比 user 档位判断）。
3. **allow/deny 规则读接口**：目前只有档位端点，没有查看/删除 allow/deny 规则的 API（审批卡能加，设置页看不了、删不了）。若要设置页管理规则列表，需加 `GET /v1/workspaces/permissions/{path}/rules` 之类。
4. **conversation detail 直接带档位**：可选优化——detail 响应里加 `approval_mode` 字段，省一次请求（客户端已持有 workspace_path 时也可不依赖）。
5. **档位变更的实时通知**：目前 PUT 后下个 turn 边界生效，客户端无事件可订阅变化（多端同步时靠轮询）。可考虑在 WS 事件流加 `permission_mode_changed`。
6. **iOS embedded 宿主**：无磁盘 settings.local.json 概念，档位经 SettingsJSON 注入读取；per-workspace 写在该宿主上不可用（单 workspace，落内存即可）——需客户端确认 embedded 模式下权限 UI 的降级策略。
