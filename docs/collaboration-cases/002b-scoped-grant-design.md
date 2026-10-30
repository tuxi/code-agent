# Case 002-B · 受限委派授权（最小设计）

2026-10-30。状态：**设计，未实现**。不修改 Runtime、不派发 Turn、不调用模型。本文只定义协议与强制点，不做 WorkBuddy 专属 Adapter，也不扩展多租户。

## 1. 目标与边界

让 WorkBuddy、豆包这类客户端能够**按工作区和允许操作范围**委派 Code-Agent，而不持有 Owner Token。

不做：

- 多租户、组织/角色体系、SSO。
- 客户端专属 Adapter（任何 MCP 客户端用同一套 manifest 即可）。
- OS 级沙箱（见第 9 节，这是两个不同层面的问题）。
- 迁移 Store：受限委派必须落在同一个 Daemon、同一份 workspace 存储上，否则 Talkify 看不见。

## 2. 威胁模型

假设：受限客户端可能被模型误导、可能被注入、也可能主动作恶；它**能读到自己的 manifest 文件**（stdio 子进程必然可读）。因此设计不能依赖"客户端不会去读凭据"，只能依赖"读到也没用"。

要防的越权：

1. 读别的 workspace 的会话。
2. 写/取消/审批不属于自己的 Turn。
3. 调用任务、模型目录、密钥、设置等与派发无关的接口。
4. 在授权过期或被撤销后继续使用。
5. 自行申请更大权限。

## 3. 令牌模型

新增一种 **scoped token**，与现有 Owner Token 并存。Opaque 随机串（≥32 字节），**不是**自包含 JWT——因为必须支持即时撤销。

存储：Daemon 侧新增 `grants` 表（`control.db`），一行一条授权。

```text
grant_id        TEXT PRIMARY KEY
token_hash      TEXT           -- 只存哈希，不存明文
workspace_id    TEXT           -- 规范化后的 workspace 路径或其哈希，与 project store 键一致
session_id      TEXT NULL      -- 可空；非空时该授权只能操作这一个 Session
actions         TEXT           -- 允许动作集合，逗号分隔
client_label    TEXT           -- 例如 "workbuddy"，仅用于审计，不作鉴权依据
issued_at       INTEGER
expires_at      INTEGER
revoked_at      INTEGER NULL
created_session TEXT NULL      -- 该授权创建的 Session，用于归属
```

动作集合（最小）：`session.create`、`turn.send`、`turn.read`、`handoff.read`。

明确**不包含**：`turn.cancel`（取消留给 Owner）、`task.*`、`approval.*`、`model.read`、`secrets.*`、`settings.*`。

## 4. Daemon 侧强制

### 4.1 认证改为两级 principal

现有中间件解析出"是否 Owner"。改为解析出 principal：

```text
principal = owner | scoped{grant_id, workspace_id, session_id, actions}
```

判定顺序：先按 Owner Token 比对（常量时间），不匹配再按 `token_hash` 查 `grants`。查不到 → 401；查到但 `revoked_at` 非空或 `expires_at` 已过 → 401（不是 403，避免泄漏授权是否存在）。

### 4.2 路由 → 动作表，默认拒绝

维护一张表，把每个路由映射到所需动作；**表里没有的路由，scoped principal 一律 403**（fail closed）。这样新增接口不会意外对受限客户端开放。

| 路由 | 动作 | 附加校验 |
| --- | --- | --- |
| `POST /v1/conversations` | `session.create` | body.workspace_path 必须等于 grant.workspace_id（规范化后比较） |
| `GET /v1/conversations/{id}` | `turn.read` | session 必须属于 grant.workspace_id；若 grant.session_id 非空则必须相等 |
| `GET /v1/conversations/{id}/events` | `turn.read` | 同上 |
| `WS /v1/conversations/{id}/stream` | `turn.send` / `turn.read` | 同上；写 `agent_input` 需 `turn.send`，只读订阅需 `turn.read` |
| `GET /v1/activity` | `turn.read` | 只返回该 workspace 的条目（按 workspace 过滤，不是全量） |
| `POST /v1/grants`、`DELETE /v1/grants/{id}` | — | **仅 Owner** |
| 其余全部（tasks、models、secrets、settings、approvals…） | — | **仅 Owner** |

### 4.3 workspace 绑定

不能只比字符串：先 `EvalSymlinks` + `Clean`，再与 grant 里存的规范化值比较。否则 `..`、符号链接、大小写差异都能绕过。绑定校验在**每次**请求上做，不只在创建 Session 时做一次。

### 4.4 撤销

`DELETE /v1/grants/{id}`（Owner）写 `revoked_at`。中间件每次请求查表，因此撤销**立即生效**，无需通知客户端，也不需要重启 Daemon。已建立的 WebSocket 在下次帧或心跳时被断开（实现时在 WS 读循环里复查 principal）。

## 5. 防止越权调用其他接口

三条一起用，缺一不可：

1. **默认拒绝**：动作表之外的路径对 scoped 一律 403，包括将来新增的路径。
2. **禁止自提权**：`POST /v1/grants` 只接受 Owner principal；scoped 调用直接 403 并记审计。不存在"申请更多权限"的接口。
3. **响应侧过滤**：`/v1/activity` 这类聚合接口必须按 workspace 过滤，不能返回全量再靠客户端不看。过滤在服务端做。

补充：scoped token 不放进 URL、日志、错误信息；manifest 里也不回显明文。

## 6. Owner 签发

最短路径是 Chater 增加一个动作：「授权委派（受限）」——选择 workspace、动作集合、有效期，生成 manifest 文件（内含 scoped token）。

- 签发接口需要 Owner 认证，外部 Agent 无法调用。
- 生成的文件沿用现有 manifest 格式，新增 `scope` 字段（`grant_id`、`actions`、`workspace_id`、`expires_at`），便于 Bridge 与用户核对。
- 同一 workspace 可同时存在多条授权，互不影响；撤销单条不影响其他。

## 7. Talkify 观察与审批

关键点：**受限委派发生在同一个 Daemon、同一个 workspace 上**，所以：

- 客户端创建的 Session 就是普通 Session，按 workspace 落进同一个 `sessions.db`，Talkify 侧栏照常出现，无需迁移或复制 Store。
- 审批事件照常广播到 Talkify；scoped token 没有 `approval.*`，**无法代答审批**。
- 用户看到的 Session 归属与今天一致，只是多了一条 `created_by_grant` 的元数据用于审计。

这也是为什么不采用"另起一个隔离 Daemon"：隔离实例不会出现在 Talkify，观察与审批就断了。

## 8. 绑定、归属、审计与重复派发

- **Session 归属**：`grants.created_session` 记录该授权创建的 Session；仅作审计，不作为可见性依据（可见性按 workspace）。
- **审计**：每条 scoped 请求记 `{grant_id, client_label, action, workspace_id, session_id, request_id, result}`。签发与撤销同样入审计。审计写入失败时应拒绝请求还是放行，需在实现时明确——建议拒绝（fail closed）。
- **重复派发**：Daemon 已有 `request_id` 幂等（fork 冲突返回 409；turn 输入的 `request_id` 作为幂等键）。实现时需确认它覆盖"两个连接同时提交同一 request_id"的情形；若不覆盖，补一层准入去重，这是跨进程唯一可靠的防线。
- **会话复用**：grant 若绑定 `session_id`，只能对该 Session 派发；未绑定则可创建新 Session，但每个 grant 只允许一个 `created_session`，避免一个授权无限开新会话。

## 9. API 权限 ≠ Executor 权限（重要）

scoped token 限制的是**客户端能调用哪些 HTTP/WS 动作**，不是 Executor 执行时的能力边界。

一旦 Turn 被准入，Agent 在 workspace 内可以读写文件、跑命令、改 Git——这些由 Executor 的工具集与审批策略决定，**不受 token 的 actions 约束**。所以：

- 拿到 `turn.send` 的客户端，可以让 Agent 在 workspace 内执行破坏性操作。
- 想限制"能做什么"，要用 workspace 绑定 + 审批策略 +（如需）OS 级沙箱，那是另一个层面的工作。
- **不能把受限 Token 描述成沙箱**，也不能用它替代审批。

这条必须写进任何对外说明里，否则会给人错误的安全感。

## 10. 最小实现清单（供后续估成本）

| # | 改动 | 位置 | 量级 |
| --- | --- | --- | --- |
| 1 | `grants` 表 + 签发/撤销接口（仅 Owner） | `internal/server`、`control.db` | 中 |
| 2 | principal 解析 + 路由动作表（默认拒绝） | `internal/server/auth.go`、`mux.go` | 中 |
| 3 | workspace 规范化与归属校验、`/v1/activity` 过滤 | `internal/server` | 小 |
| 4 | WS 帧级复查 principal | `internal/server/ws.go` | 小 |
| 5 | manifest 增加 `scope` 字段（Bridge 只做展示与核对） | `internal/mcpbridge/connection.go` | 小 |
| 6 | Chater 授权 UI 与文件生成 | 宿主 App（本仓库外） | 中 |
| 7 | 审计字段与写入 | `internal/server` | 小 |

Bridge 侧**不需要**新增工具：现有 executor 面已经够用；只是它拿到的 token 变成受限的。

## 11. 未决问题

1. 撤销后正在运行的 Turn 是否立即取消？建议**不取消**（避免授权撤销变成破坏执行的手段），只拒绝后续请求。
2. 一个 grant 是否允许 `turn.cancel`？建议不允许，取消由 Owner 在 Talkify 操作。
3. `model.read` 是否开放？派发需要知道模型是否可用；建议开放只读目录，但只返回该 provider 的模型列表。
4. 审计写入失败时的取舍（建议拒绝请求）。
5. Chater 是否有可复用的授权 UI 骨架（未查）。
