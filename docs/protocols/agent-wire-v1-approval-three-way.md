# Three-Way Approval (v1.2)

Server 端已就绪（`internal/server/control.go`、`internal/server/approver.go`），
本条文档面向 **iOS（Swift / AgentKit）、macOS（Swift / AgentKit）、Web（JS/TS /
WebSocket）客户端开发者**，说明三态审批的线协议和 UI 交互。

---

## 1. 消息形状

### 1.1 Server → Client（不变）

审批请求的 `approval_request` 没有任何改变：

```json
{
  "type": "approval_request",
  "id": "appr_a1b2c3",
  "session_id": "sess_root",
  "turn_id": "turn_42",
  "tool_name": "mcp__github__list_issues",
  "tool_args": { "owner": "anthropics", "repo": "claude-code" },
  "deadline_ms": 120000       // 仅服务端配置了审批超时时存在，否则缺省 = 无期限等待
}
```

### 1.2 Client → Server（新增 decision + scope）

```jsonc
// 方式 A — 推荐：三态 decision
{ "type": "approval_response", "id": "appr_a1b2c3", "decision": "always", "scope": "local" }
{ "type": "approval_response", "id": "appr_a1b2c3", "decision": "once" }
{ "type": "approval_response", "id": "appr_a1b2c3", "decision": "deny" }

// 方式 B — 兼容：老客户端只发 approved 仍然工作
{ "type": "approval_response", "id": "appr_a1b2c3", "approved": true }
{ "type": "approval_response", "id": "appr_a1b2c3", "approved": false }
```

| 字段 | 类型 | 必需 | 说明 |
|------|------|------|------|
| `type` | string | 是 | `"approval_response"` |
| `id` | string | 是 | 与 `approval_request.id` 关联 |
| `decision` | string | **新，推荐** | `"once"` / `"always"` / `"deny"`。存在时**优先于** `approved` |
| `scope` | string | **新，可选** | `decision = "always"` 时规则落盘的作用域：`"local"`（项目本地，默认）或 `"user"`（全局） |
| `approved` | bool | 兼容 | 老字段。`decision` 存在时被忽略 |

**`decision` 优先级**：`decision` 存在 → 用它；否则回退到 `approved` 布尔。
一个尚未升级的老客户端只发 `approved: true/false`，服务端行为与升级前完全一致。

---

## 2. 作用域（scope）说明

| scope 值 | 落盘位置 | 语义 |
|----------|---------|------|
| `"local"`（默认） | `<workspace 根>/.codeagent/settings.local.json` | 仅本机本项目，已被 gitignore，不透 VCS |
| `"user"` | `~/.codeagent/settings.json` | 本机所有项目 |

省略 `scope` 或不认识的字符串 → 服务端默认 `"local"`。所以老客户端即使开始发 `decision:"always"` 而不带 `scope`，规则也会安全落盘到项目本地。

**UI 建议**：参考 Claude Code 的审批卡，在 "Always allow" 按钮旁边放一个作用域下拉（或分组按钮），默认选中 "Local"。

---

## 3. MCP 工具识别与显示

### 3.1 工具名约定

`tool_name` 的命名规则：

| 前缀 | 含义 | 示例 |
|------|------|------|
| `mcp__<server>__<tool>` | MCP 外部工具 | `mcp__github__list_issues`、`mcp__db__query` |
| 其他 | 内置工具 | `run_command`、`edit_file`、`create_file`、`apply_patch`、`git_commit` |

**建议渲染**：检查 `tool_name.HasPrefix("mcp__")`，解析出 `server`（第一个 `__` 前面）和工具名，在审批卡上显示为：

```
▸ MCP: github → list_issues
```

内置工具则显示原始名。

### 3.2 "Always allow" 的通配符语义

当用户在 MCP 工具的审批卡上选择 "Always allow"，服务端自动将该 **整个 MCP 服务器** 加入权限白名单：

- 工具 `mcp__github__list_issues` → 规则 `mcp__github__*`
- 工具 `mcp__github__create_pr` → 规则 `mcp__github__*`

**一个 server 只需确认一次**，之后该 server 的所有工具均免弹窗。
这个匹配在服务端做（`internal/approve/rules.go:patternFor`），客户端无需关心算法，只需展示提示文案：

```
Always allow all tools from "github"?   [Always allow] [Allow once] [Deny]
```

---

## 4. 审批卡 UI 建议

### 4.1 三按钮布局

```
┌──────────────────────────────────────────────────┐
│ ▸ run mcp__github__list_issues?                  │
│   owner: anthropics                              │
│   repo: claude-code                              │
│                                                  │
│  [Always allow]   [Allow once]   [Deny]         │
│  Scope: [ local ▾ ]                              │
└──────────────────────────────────────────────────┘
```

### 4.2 何时显示/隐藏 "Always allow"

- **总是显示** —— 服务的规则匹配在 "Always allow" 落地前也已经在内存中生效，
  服务端不会重复弹同一个已经匹配的卡（Allowlist 读到已存在规则直接放行）。
  用户再次看到同一 MCP server 的卡只意味着还没点过 "Always"。
- 可选优化：内置工具也可以显示，但内置工具没有 server 聚合语义
  （`edit_file` 的 "Always" 只放行 `edit_file`，不会放行 `create_file`）。

### 4.3 键盘快捷键

| 键 | 动作 |
|----|------|
| Enter / Space | 确认当前高亮选项 |
| ← → 或 Tab | 切换三个按钮 |
| y / o | Allow once |
| a | Always allow |
| n / Esc | Deny |

---

## 5. 已自动放行的情况（无需弹卡）

以下情况服务端**不会发出** `approval_request`，客户端无需弹卡，
改为接收 `auto_approved` 事件（纯观测、审计用）：

1. **权限规则已存在**：之前已 "Always allow" 过的工具
2. **auto 模式**：`/auto on` 后，工作区内 `edit_file`/`create_file` 自动放行
3. **permissions 配置**：`config.yaml` 的 `permissions.allow` 或 `settings.json` 中已匹配的规则

`auto_approved` 事件形态（纯观测，不阻塞）：

```json
{
  "type": "event",
  "event": {
    "kind": "auto_approved",
    "tool_name": "mcp__github__list_issues",
    "text": "auto-approved by permission rule mcp__github__*",
    "turn_id": "turn_42",
    "session_id": "sess_root"
  }
}
```

收到此事件时客户端可以**在消息流中展示一条低调的确认行**（如 "✓ Auto-approved"），但无需弹卡、无需用户交互。

---

## 6. 断线重连与去重

- `approval_request` 是会话级请求，**不因 WebSocket 断线而被放弃**。
- 客户端重连后，服务端用 **同一 `id`** 重发所有未决的 `approval_request`。
- **客户端必须按 `id` 去重**：已回复过的 `id` 忽略、已显示过的 `id` 恢复卡片而非重复创建。
- 拒绝的 fail-safe 仅发生在：
  - 客户端显式回 `decision: "deny"`（或 `approved: false`）
  - 配置了 `deadline_ms` 且超时
  - 会话被删除（`DELETE /v1/conversations/{id}`）
- **用户断线、关 App、锁屏都不触发拒绝**。隔夜回来仍可批准。

---

## 7. 计划审批（Plan Approval）

**不变**。`plan_approval_response` **仍然是两态布尔**（plans 没有 "always" 语义）：

```json
// server → client
{ "type": "plan_approval_request", "id": "plan_appr_1",
  "session_id": "sess_root", "turn_id": "turn_42",
  "plan_id": "plan_abc", "title": "Add Auth",
  "content": "# Plan\n1. ..." }

// client → server（两态，不变）
{ "type": "plan_approval_response", "id": "plan_appr_1", "approved": true }
```

---

## 8. 迁移清单（按客户端逐个）

### iOS / macOS（Swift — AgentKit）

1. ⬜ `ApprovalResponse` 模型加 `decision: String?` + `scope: String?`，omitempty
2. ⬜ 审批卡 UI 从两个按钮改为三个：Always allow / Allow once / Deny
3. ⬜ MCP 工具（`tool_name.hasPrefix("mcp__")`）：解析 server 名，显示 "Always allow all from X" 提示
4. ⬜ 可选：scope 下拉（local / user），默认 local
5. ⬜ 按 `id` 去重（重连时服务端重发同一 id）
6. ⬜ 收到 `auto_approved` 事件时展示 "✓ Auto-approved" 低调度量行
7. ⬜ `plan_approval_response` 保持两态，不引入 decision

### Web（JS/TS — WebSocket）

同 iOS/macOS，额外关注：
- ⬜ 按钮的 `scope` 下拉在 Web 上用原生 `<select>` 或自定义 popover
- ⬜ 键盘快捷键（y/o/a/n）在 Web 审批卡上也应生效（`keydown` listener）

---

## 9. 回退到老客户端的最小变更

如果时间紧，可以只加一个 `decision` 字段、不改造 UI：

```js
// 最小 diff：把原来的 approved 布尔替换为 decision 字符串
const response = {
  type: "approval_response",
  id: request.id,
  decision: userApproved ? "once" : "deny"
};
// scope 缺省 → 服务端默认 local（对 "once" 无影响）
```

此时用户体验不变（仍然是 yes/no），但线协议已就绪，后续无痛加按钮。

---

## 10. 端到端序列

```
Turn 进行中
  │
  ├─ tool "mcp__github__list_issues" 被评为 side-effecting
  │
  ├─ Allowlist 检查：无匹配规则 → 放行到 RemoteApprover
  │
  ├─ Server: { type:"approval_request", id:"appr_x", tool_name:"mcp__github__...", ... }
  │                                      ↓
  │    Client 渲染三按钮卡，用户点 "Always allow (local)"
  │    Client: { type:"approval_response", id:"appr_x", decision:"always", scope:"local" }
  │                                      ↓
  ├─ Server: approve → 落盘 mcp__github__* → 返回 approved
  │              同时 emit auto_approved 事件（审计）
  │
  ├─ 工具 mcp__github__list_issues 执行
  │
  ├─ 下次同 turn 内又有 mcp__github__create_pr
  │   Allowlist.MatchAllow → 命中 mcp__github__* → 直接 approved
  │   emit auto_approved 事件，**不发 approval_request**，客户端无需弹卡
  │
  ▼ Turn 继续
```
