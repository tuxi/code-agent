# Case 002-B · 受限委派授权（安全修订版 v2）

2026-10-31。状态：**设计，未实现**。不修改 Runtime、Bridge 或 Talkify，不调用模型。

v1 的三处判断在本版被修正：Session 可见性不再按 workspace 全开、WS 需要消息级矩阵、撤销对既有连接要有明确语义。

## 1. 范围

让外部 MCP 客户端（WorkBuddy、豆包等）按**具体 Session 与允许动作**委派 Code-Agent，不持有 Owner Token。通用设计，不做客户端专属 Adapter，不做多租户。

## 2. 威胁模型

受限客户端可能被提示注入、可能被误导、也可能主动作恶；它**能读到自己的 manifest**（stdio 子进程必然可读）。因此安全不能依赖"客户端不读凭据"，只能依赖"读到也没用"。

要防的越权：跨 Grant 读取、跨 Session 派发、撤销后继续用、未知路由、越权审批、自提权、重复派发。

## 3. 身份与可见性（v2 修正）

### 3.1 Session 归属到 Grant，而不是 Workspace

v1 的"同 workspace 即可见"被推翻。两个客户端被授权到同一 workspace，**不得**因此看到彼此的历史。

```text
sessions.owner_grant_id   -- 该 Session 由哪个 Grant 创建；Owner 创建的为 NULL
session_delegations       -- (grant_id, session_id, actions, granted_at)
                          -- 仅 Owner 可写，用于把既有 Session 显式授权出去
```

可见性判定（每个请求、每条 WS 帧）：

```text
visible(session, principal) =
    principal 是 Owner                                   → true
    或 session.owner_grant_id == principal.grant_id       → true
    或 存在 session_delegations(session, grant) 且动作匹配 → true
    否则                                                  → false
```

不可见时返回 **404**，不是 403——403 会泄漏"这个 Session 存在"。

对既有 Session 派发，必须由 Owner 先写 `session_delegations`；客户端不能自助申请。

### 3.2 对 Talkify 的影响

零迁移。Talkify 是 Owner，仍然看到该 workspace 全部 Session；受限客户端创建的 Session 也照常出现在侧栏，只是 `owner_grant_id` 非空。审批仍只走 Owner 通道。

## 4. 令牌与 Grants 表

```text
grant_id, token_hash, workspace_id, actions, session_id?,
client_label, issued_at, expires_at, revoked_at?, created_session?
```

动作集：`session.create`、`turn.send`、`turn.read`、`handoff.read`。

**不含** `turn.cancel`、`approval.decide`、`tools.register`、`task.*`、`model.read`、`secrets.*`、`settings.*`。

Token 为不透明随机串（≥32 字节），只存 `token_hash`；明文仅在签发时返回一次，不落日志、不进 URL。

## 5. HTTP 权限矩阵

路由 → 动作表，**表外一律 403（fail closed）**，新增路由默认不开放。

| 路由 | 动作 | 附加校验 |
| --- | --- | --- |
| `POST /v1/conversations` | `session.create` | workspace 必须是 grant 绑定的那个；一个 grant 只允许一个 `created_session` |
| `GET /v1/conversations/{id}` | `turn.read` | `visible()` |
| `GET /v1/conversations/{id}/events` | `turn.read` | `visible()` |
| `WS /v1/conversations/{id}/stream` | `turn.read` | `visible()`；消息级另见 §6 |
| `GET /v1/activity` | `turn.read` | 服务端按可见 Session 过滤，不返回全量 |
| `POST /v1/grants`、`DELETE /v1/grants/{id}` | — | **仅 Owner** |
| tasks / models / secrets / settings / approvals / 其余 | — | **仅 Owner** |

## 6. WebSocket 消息级矩阵（v2 新增）

只校验 HTTP 路由不够：连接建立后，帧才是攻击面。按现有消息类型逐个定义。

### 6.1 入站

| 消息 | 所需动作 | Scoped 允许 | 说明 |
| --- | --- | --- | --- |
| `send_message` | `turn.send` | 是 | 启动一个 Turn |
| `agent_input` kind=`text` | `turn.send` | 是 | 与 send_message 等价 |
| `agent_input` kind=`tool_result` | `turn.send` | **否** | 只对客户端注册的工具有效；scoped 不注册工具 |
| `agent_input` kind=`command`（`cancel`） | `turn.cancel` | **否** | 取消留给 Owner |
| `agent_input` kind=`system` | — | **否** | 预留/空实现 |
| `cancel_turn` | `turn.cancel` | **否** | 同上 |
| `approval_response` | `approval.decide` | **否** | 审批必须由 Owner 在 Talkify 完成 |
| `plan_approval_response` | `approval.decide` | **否** | 同上 |
| `ask_user_response` | `approval.decide` | **否** | 同上 |
| `register_tools` | `tools.register` | **否** | 否则客户端可向服务端注入工具 |
| `invoke_prompt` | `turn.send` + `prompt.read` | **否**（最小版） | 会以 MCP prompt 渲染成 Turn，超出最小范围 |

越权帧的处理：回 `error` 帧（`code=forbidden_action`）并计数，**不立即关闭连接**（关闭会让错误配置变成拒绝服务）；同一连接连续越权 N 次则关闭（建议 N=10）。

### 6.2 出站（容易被忽略的一半）

出站也必须按 principal 过滤：

- 事件流只推**可见 Session** 的事件。
- **审批请求（control/approval）绝不推给 scoped 连接**——否则客户端可以直接回 `approval_response` 自我批准，入站拦截形同虚设。
- 其他连接的 Session 事件、全量 activity 推送同理。

### 6.3 撤销与既有连接

撤销后：既有 WS 必须失效。实现方式：撤销时把 `grant_id` 放进内存撤销集合并主动关闭该 grant 的所有连接（close code 1008）。读循环每帧先查内存集合（不做 DB 查询），DB 仍是连接建立时的权威来源。

## 7. 撤销语义（v2 明确）

- 撤销**只阻断后续授权操作**，不取消已经准入的 Turn。理由：撤销授权不应成为破坏正在进行的执行的手段。
- 撤销立即影响：新连接、新帧、新 HTTP 请求。
- 撤销不影响：已准入 Turn 的执行、已产生的证据。Owner 若想中止执行，用 Talkify 的取消。
- 撤销后 Token 一律 401（不是 403），避免泄漏授权是否存在。

## 8. workspace 身份（v2 修正）

不能用路径字符串比较。

**服务端稳定 ID**：`projects` 表为每个 workspace 分配 `workspace_id`（UUID），路径只是它的属性。Grant 绑定 `workspace_id`。

签发时同时记录路径指纹：

```text
resolved_path      -- EvalSymlinks 后的绝对路径
root_dev/root_ino  -- 工作区根目录的 (dev, ino)
```

每个请求校验：

| 情况 | 处理 |
| --- | --- |
| 路径被重命名/移动 | `resolved_path` 不匹配 → 拒绝，要求 Owner 重新签发 |
| 符号链接指向变化 | 解析后路径不匹配 → 拒绝 |
| 目录被删除后重建（同路径、不同 inode） | `(dev, ino)` 不匹配 → 拒绝。防止把 Grant 指向另一个仓库 |
| 大小写/`..` 差异 | 规范化后比较，`..` 不允许出现在授权路径里 |

原则：**任何身份漂移都要求重新授权**，不做自动重绑。

## 9. 签发、存储、传递、轮换（v2 补充）

- **签发**：仅 Owner 认证的调用可创建 Grant；返回明文一次。外部 Agent 无任何申请入口。
- **存储**：只存 `token_hash`（加盐哈希）；明文不落盘、不落日志、不进错误信息。
- **传递**：Chater 写两份**不同**的 manifest——Owner 那份含 Owner Token，客户端那份只含 scoped token。客户端进程的 argv/cwd 里不出现 Owner manifest 的路径。
- **无法防住的部分**：同 UID 进程理论上可读任意文件。所以不承诺"读不到"，只承诺"读到也没用"（scoped token 的爆炸半径限于一个 workspace 的可见 Session）。
- **轮换**：TTL ≤ 12h；轮换 = 签发新 Grant + 撤销旧 Grant。注意 Bridge 只在启动时读 manifest，轮换后需要重启 Bridge——这是当前限制，不为此改协议。
- **撤销通道**：`DELETE /v1/grants/{id}` 仅 Owner；签发与撤销都入审计。

## 10. 审计与故障策略（v2 明确）

| 操作类别 | 审计写失败时 |
| --- | --- |
| 敏感写：`session.create`、`turn.send`、grant 签发/撤销 | **fail closed**，拒绝请求并返回 `audit_unavailable` |
| 读：`turn.read`、`handoff.read`、activity | fail open，但响应带 `audit_degraded=true`，并计一次告警 |
| 审计存储整体不可用 | 停止签发新 Grant；既有读继续 |

理由：读操作若也 fail closed，审计故障会直接变成拒绝服务；而"未审计的读"风险低于"未审计的写"。

审计字段：`{grant_id, client_label, action, workspace_id, session_id, request_id, result, at}`。

## 11. API 权限 ≠ Executor 权限

Scoped Token 限制的是**客户端能调用哪些 HTTP/WS 动作**，不是 Agent 执行时的能力边界。

不由 Scoped Grant 解决的风险：

- Agent 在 workspace 内读写文件、执行命令、改 Git。
- 命令的网络外发（数据外泄）。
- 资源耗尽（长任务、大文件）。
- 提示注入导致"合法但有害"的操作。
- 审批被误批（Owner 自己点错）。

对应缓解在别处：workspace 绑定、审批策略、OS 级沙箱、网络策略。**不能把受限 Token 描述成沙箱**，也不能用它替代审批。

## 12. 最小负向测试矩阵（v2 新增）

| 用例 | 期望 |
| --- | --- |
| 跨 Grant 读取（A 读 B 创建的 Session） | 404，事件流无推送 |
| 同 workspace 但未授权的既有 Session | 404 |
| 跨 Session 派发（grant 绑定 S1，向 S2 发） | 404，无 Turn 创建 |
| 撤销后 WS 写入 | 连接被关闭（1008），帧未生效 |
| 撤销后 HTTP 请求 | 401 |
| 未知路由（如 `/v1/secrets`） | 403，且不触发任何副作用 |
| 越权审批（`approval_response`） | `forbidden_action`，审批状态不变 |
| 自提权（scoped 调 `POST /v1/grants`） | 403 + 审计记录 |
| 重复 `request_id` 同 payload | 返回同一 receipt，不产生第二个 Turn |
| 重复 `request_id` 不同 payload | 409 |
| 过期 Token | 401 |
| 目录被替换（同路径不同 inode） | 拒绝，要求重新授权 |
| 一个 grant 创建第二个 Session | 拒绝（只允许一个 `created_session`） |
| `turn.cancel` | 403 |

## 13. 主要风险（设计后仍存在）

1. **审批仍在 Owner 手里**，受限客户端无法推进需要审批的操作——有意设计，但会让"无人值守派发"卡在审批上。
2. **Bridge 轮换需重启**，长连接场景会中断。
3. **同 UID 可读文件**，scoped token 仍可能被本地其他进程读走；爆炸半径受限但不为零。
4. **单会话限制**可能不够用（一个客户端连续做多个任务），届时需要"多会话 Grant"策略，本版未设计。
5. **审计 fail open 的读**在故障窗口内不可追溯。
6. **workspace 内 Agent 能力不受限**（第 11 节）。

## 14. 最小实现顺序

依赖关系决定顺序，前四步不做完不具备安全验收条件。

| 顺序 | 内容 | 依赖 | 备注 |
| --- | --- | --- | --- |
| 1 | `grants` 表 + 签发/撤销接口（仅 Owner） | — | 无此步无法产生受限 Token |
| 2 | principal 解析 + HTTP 路由动作表（默认拒绝） | 1 | fail closed 的基础 |
| 3 | Session 归属（`owner_grant_id`、`session_delegations`）+ `visible()` | 2 | v2 的核心修正 |
| 4 | WS 入站矩阵 + 出站过滤（审批不推给 scoped） | 3 | 少了出站过滤，入站拦截可被绕过 |
| 5 | 撤销时关闭既有连接（内存撤销集合） | 4 | 语义完整性 |
| 6 | workspace 指纹（resolved_path + dev/ino）校验 | 2 | 可与 3 并行 |
| 7 | 审计 + fail closed/open 策略 | 2 | |
| 8 | manifest `scope` 字段（Bridge 只展示与核对） | 1 | Bridge 无新工具 |
| 9 | Chater 授权 UI 与双 manifest 生成 | 1 | 仓库外 |

第 1–4 步完成即可跑第 12 节的负向矩阵；第 5–8 步补齐后才算可对外。

## 15. 未决

1. 撤销后是否允许 Owner 主动取消在跑的 Turn（本版：只由 Owner 手动取消）。
2. 一个 Grant 是否允许多个 Session（本版：一个）。
3. `model.read` 是否开放给 scoped（派发需要确认模型存在；本版未开）。
4. Chater 是否有可复用的授权 UI 骨架（未查）。
