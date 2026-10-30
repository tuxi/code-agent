# Case 002-B · v2 设计的只读实现映射

2026-10-31。状态：**只读核查，未改任何代码**。基于 v2 设计（`cfdbfc3`）逐项对照现有实现。

核查方式：阅读 `internal/server/{auth,router,ws,control,mux}.go`、`internal/runtime/store.go`、`internal/controlplane/owner.go`、`internal/mcpbridge/{handoff,connection}.go`；只读查询 `~/.codeagent` 下的库结构。未启动 Turn、未调用模型。

## 0. 结论摘要

1. **审批入站没有任何授权**，且代码注释明确说明这是有意为之——v2 要求的两条独立边界里，第二条现在不存在。
2. **认证只有一个布尔标记**（owner / 非 owner），没有 principal，也没有路由级动作表；所有 scoped 强制点都要新建。
3. **workspace 身份就是路径哈希，没有 inode**；同路径换一个仓库仍会命中同一个 store，v2 的指纹校验是真实缺口。
4. **三个库都是 WAL**，`grants` 与 session 归属若分库，无法做到单事务原子。
5. **`control.db` 已有 `session_owners` 表，但语义是 daemon 实例租约**，不是主体归属，命名必须避开。
6. **request_id 是事后检测而非准入去重**，并发提交同一 request_id 可产生两个 Turn。
7. **没有请求级审计**，审计基础设施需从零建；"审计与业务原子性"当前无从谈起。

## 1. 映射表

| # | 设计要求 | 代码位置 | 已有能力 | 缺口 | 风险 |
| --- | --- | --- | --- | --- | --- |
| 1 | 两级 principal（owner / scoped） | `internal/server/auth.go:112-137` | 只比对单个 token，成功后写入布尔 `runtimeOwnerAuthContextKey=true` | 无 principal 结构；无 token 表；无 per-request 授权上下文 | 高 |
| 2 | 路由 → 动作表，默认拒绝 | `internal/server/mux.go:653-1620` | 全部路由集中在 `NewMux` 一处注册，便于加包装 | 认证中间件包住整个 handler，**不知道路由**；需在 mux 层加 per-route 包装或中央表 | 高 |
| 3 | Session 归属 Grant | `internal/controlplane/owner.go:159`（`session_owners`） | 已有表，但字段是 `instance_id` + 心跳租约 | 语义不同；缺 `owner_grant_id`；缺 `session_delegations` | 高 |
| 4 | `visible()` 判定，不可见返 404 | `mux.go:1091,1174`（单会话读）、`ws.go:142`（订阅） | 单会话读会校验 session 存在与 workspace | 无主体维度；同 workspace 内彼此可见 | 高 |
| 5 | WS 入站动作矩阵 | `internal/server/router.go:118-218` | 已有完整消息分发：`send_message` / `cancel_turn` / `approval_response` / `plan_approval_response` / `ask_user_response` / `agent_input`(4 种 kind) / `register_tools` / `invoke_prompt` | 全部无授权判断，任何已认证连接都可发 | 高 |
| 6 | 审批入站独立校验 | `router.go:202-215`、`ws.go:314-338` | `RemoteApprover` 只校验 ID 是否未知/已解决 | **无主体校验**；注释明确 `ownsSessionControl` 门槛被移除，接受"任意活跃连接"的裁决 | **高（最高）** |
| 7 | 审批出站不推 scoped | `ws.go:262-292` | `approver.AddSink` 按连接注册 | 审批**广播给所有 sink**，没有 principal 过滤 | 高 |
| 8 | 撤销后既有连接失效 | `ws.go:294-305`（revision） | 有 revision 概念，但只用于释放 sink | 无撤销集合、无主动关闭 | 中 |
| 9 | 撤销只阻断后续，不取消在跑 Turn | `router.go:98-105`（turn 上下文 `WithoutCancel`） | Turn 生命周期独立于连接，天然满足 | 无需改动，但要在文档与测试里固化 | 低 |
| 10 | workspace 稳定身份 | `internal/runtime/store.go:202-217` | 路径 → `basename-sha256(abs)[:12]` 作为 store key | **无 EvalSymlinks、无 dev/ino**；目录替换后仍命中同一 key | 高 |
| 11 | 单 Grant 单 Session 原子性 | `mux.go:937-984` → `repo.Create` | 创建走单一 repo 调用 | 无"每 grant 一次"约束；无跨库事务（见 §5） | 中 |
| 12 | request_id 并发幂等 | `internal/mcpbridge/handoff.go:83-108` | 通过扫描 `turn_accepted` 事件**事后**判定 | 准入时不去重；并发提交会产出两个 Turn，随后 lookup 报 `ambiguous durable admission` | 高 |
| 13 | 审计（attempted / outcome 分离） | 无 | — | 仓库内无请求级审计；`grep audit` 仅命中 settings/credential/permission | 高 |
| 14 | Bridge 侧 manifest 携带 scope | `internal/mcpbridge/connection.go`（`loadConnection`） | manifest 有 schema_version / endpoint / server_id / store_id / pid / expires_at / access_token | 无 `scope` 字段；Bridge 无法区分 owner 与 scoped | 中 |
| 15 | Bridge 工具面不变 | `internal/mcpbridge/reviewer_tools.go`、`mcp.go` | 已支持按 profile 注册不同工具面 | 无需改动（executor 面复用） | 低 |

## 2. WS 审批：两条边界只有一条存在

v2 要求"入站校验"和"出站过滤"是两条独立强制边界。现状：

**出站**：`claimSessionControl`（`ws.go:262-292`）把每个连接注册为 sink，注释写明 *"approvals are broadcast to ALL active sinks, not just the latest"*。所以过滤是可行的，只是没做。

**入站**：`revisionApprovalResolver.Resolve`（`ws.go:328-334`）直接透传给 target，**不做任何主体判断**。`ws.go:314-327` 的注释解释了为什么移除 `ownsSessionControl` 门槛：多设备场景下按连接 revision 判定会导致合法审批被丢弃。

这意味着：**只要知道 approval ID，任何已认证连接都能裁决**。而 approval ID 会出现在事件流里（`observed` / control 事件），scoped 客户端读得到自己可见 Session 的事件，就拿到了 ID。

所以 v2 的"两条独立边界"不能只做一半。正确做法不是恢复 revision 门槛（会打回多设备问题），而是加**主体维度**：只有 owner principal 才能 resolve；所有 owner 设备都是 owner，多设备不受影响。这个门槛与 revision 门槛正交。

## 3. 过期/撤销与历史保留

- Session 历史存在 `sessions.db`，与 grant 生命周期完全解耦——撤销不会删数据，Owner 仍可见。这一点现状即满足。
- 但"新 Grant 访问旧 Session 必须显式 delegation"没有承载物：`session_owners` 是实例租约，不能复用。需要新增 `owner_grant_id` 列（或独立表）与 `session_delegations`。
- 注意：sessions 表在 per-project `sessions.db`，而 grants 若放 `control.db`，就是跨库（见 §5）。

## 4. 单 Grant 单 Session 与 request_id 并发

**单 Grant 单 Session**：`POST /v1/conversations` 直接调 `repo.Create`，没有"该 grant 已创建过 Session"的检查，也没有对 `created_session` 的条件更新。实现时需要一条原子写：`UPDATE grants SET created_session=? WHERE grant_id=? AND created_session IS NULL`，用受影响行数判定，而不是"先查再写"。

**request_id**：`LookupAdmission` 是**读事件流事后判定**，不是准入去重。两个连接同时提交同一 request_id，可能各自准入一个 Turn；之后 lookup 发现两条 `turn_accepted`，直接返回 `ambiguous durable admission`——即"检测到冲突，但冲突已经发生"。

这是 v2 测试矩阵里"重复 request_id 同 payload → 返回同一 receipt"无法达成的根因。需要准入路径上的原子去重（唯一索引或事务内 `INSERT ... ON CONFLICT DO NOTHING` + 回读），且必须在 sessions.db 内完成，才能与 `turn_accepted` 同一事务。

## 5. 跨库事务一致性

实测三个库的 `journal_mode` 都是 **wal**：

```text
control.db   wal
index.db     wal
sessions.db  wal
```

SQLite 的跨库原子提交只在**非 WAL** 下成立。因此：

- `grants`（control.db）+ Session 归属（sessions.db）**不能**放在一个事务里。
- 可行方案：把 `grants` 与 `session_delegations` 放进 **sessions.db**（与归属同库，单事务），control.db 只保留实例租约这类跨 workspace 的数据。代价是每个 workspace 一份 grant 表，跨 workspace 的 grant 需要多库写入——但那本来也不是最小设计要支持的能力（一 Grant 一 Workspace）。
- 若坚持放 control.db，则必须接受"最终一致 + 补偿"：签发先写 grants，再写归属，失败回滚靠幂等重试。这会引入 v2 想避免的中间态。

**建议**：grants 落 sessions.db。

## 6. workspace 身份绑定位置

绑定点只有一处：`runtime.storePath`（`store.go:202-217`），`abs = filepath.Abs(root)`，`key = basename + "-" + sha256(abs)[:12]`。

由此推出的实际行为：

| 场景 | 现状 | v2 要求 |
| --- | --- | --- |
| 路径改名/移动 | 新 key → 新库，旧会话"消失" | 拒绝并要求重新授权（现状更糟：静默换库） |
| 符号链接访问 | 不同 key → 不同库 | 解析后比较 |
| 同路径删除重建（换仓库） | **同一 key，同一库** | 用 `(dev, ino)` 拒绝 |
| 大小写差异（大小写不敏感卷） | 可能不同 key | 规范化 |

注意：`storePath` 没有任何 inode 信息，所以"目录替换"这条风险当前是**真实存在**的，不是理论问题。

## 7. 审计与业务原子性

仓库内没有请求级审计设施（`grep -rl audit internal/` 只命中 settings / credential / permission / event，均为无关语义）。

因此 v2 的"敏感写 fail closed、读 fail open"目前无法落地为"审计与业务同事务"，因为**没有审计表**。实现时必须先决定：

- 审计写在哪：若与业务同库（sessions.db），可用单事务保证"业务成功 ⇔ 审计成功"；
- 若审计独立（推荐单独 audit.db 或 append-only 文件），则必须显式两阶段：先写 `operation_attempted`，业务提交后写 `operation_outcome`，并接受"attempted 无 outcome"的中间态作为可观测信号——这正好对应用户要求的 attempted/outcome 分离。

**不能**声称"业务写成功但审计失败时报告 fail closed"——那是自相矛盾。正确的表述是：审计预写失败 → 不执行业务；业务已提交而 outcome 写失败 → 记录缺口并告警，不谎称已回滚。

## 8. Chater / Talkify 的实际接口

本仓库只提供 daemon 的 HTTP/WS 接口（`mux.go` 注册的 `/v1/*`），**不包含 Chater/Talkify 的授权或审批 UI**。仓库内没有任何 grant 签发入口、没有审批界面代码。

所以：

- "Owner 签发授权"当前**没有可调用的产品接口**，需要宿主 App 新增。
- 审批能力存在，但走的是 WS 的 `approval_response`（面向会话），不是面向授权的管理接口。
- 设计文档里不应假设已有 UI；实现前需要宿主侧确认。

## 9. 最小实现拆分（按依赖与可验证性）

| 批次 | 内容 | 依赖 | 可验证 |
| --- | --- | --- | --- |
| P1 | `grants` 表 + `owner_grant_id` + `session_delegations`，**全部落 sessions.db** | — | 单库事务可测 |
| P2 | principal 解析 + 路由动作表（fail closed） | P1 | 未知路由 403、跨 grant 404 |
| P3 | `visible()` 接入单会话读与事件读 | P2 | 跨 grant 读取 404 |
| P4 | WS 入站矩阵 + **审批主体门槛**（owner-only resolve） | P2 | 伪造 `approval_response` 被拒 |
| P5 | WS 出站过滤（审批不推 scoped） | P4 | scoped 连接收不到审批请求 |
| P6 | 撤销集合 + 关闭连接 | P5 | 撤销后写入被拒、连接 1008 |
| P7 | workspace 指纹（resolved_path + dev/ino） | P2 | 目录替换被拒 |
| P8 | 准入级 request_id 去重 | P1 | 并发同 request_id 只产生一个 Turn |
| P9 | 审计 attempted/outcome 两阶段 | P2 | 审计故障下的行为可复现 |
| P10 | manifest `scope` + Chater 签发 UI | P1 | 端到端签发→派发 |

P1–P5 完成即可跑通 v2 第 12 节的大部分负向用例；P6–P10 补齐才具备对外条件。

## 10. 未验证 / 需实现前确认

1. 多 workspace 的路由分发位置：`codeagentd` 用单一 `repo` 构造 mux（`main.go:386`），但运行时打开了多个 project store。per-request 如何切库未在本次核查中确认，直接影响 P2/P7 的落点。
2. `sharedDeviceAuthContextKey` 旁路（嵌入式宿主）在 scoped 模式下如何处理——它绕过 token，必须显式排除。
3. `RemoteApprover` 是否记录"哪个连接提交了裁决"，决定 P4 的实现成本。
4. 事件流是否真的携带 approval ID（推断为是，未逐字段确认）。
5. Chater 是否有可复用的授权 UI 骨架（仓库外）。
