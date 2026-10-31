# Case 002-B · 调用链核实与映射修正

2026-10-31。状态：**只读核查**。修正 `4621f7f` 映射中的两处错误结论，并回答 7 个定点问题。

## 0. 两条重要更正

| 原结论（`4621f7f`） | 实际 | 影响 |
| --- | --- | --- |
| "Session 存在 per-workspace `sessions.db`，grants 与归属跨库，无法单事务" | **错**。Daemon 只用一个 store（其启动目录对应的那个），`workspace_path` 只是列。 | 跨库事务问题**消失**，grants 可与 sessions 同库同事务 |
| "request_id 只有事后冲突检测，并发会产生两个 Turn" | **错**。`turn_inputs` 主键 `(session_id, request_id)`，准入在单事务内完成，`turn_accepted` 同事务写入。 | P8 从"新建去重"降级为"已有能力，只需验证" |

两处错误的根因相同：我把 `~/.codeagent/projects/*/sessions.db` 当成了"每 workspace 一库 + 按请求路由"，实际是"每 daemon 一库 + 其他运行时的库并存"。

## 1. 调用链：从启动到 store 选择

```text
codeagentd main.run()
  cwd = os.Getwd()                                    main.go:177
  telemetryStore = runtime.OpenStoreForServer(cwd)     main.go:180
      → storePath(cwd) = ~/.codeagent/projects/<base>-<sha256(abs)[:12]>/sessions.db
  repo = conversation.NewSQLiteRepository(telemetryStore, …)   main.go:204
  handler = server.NewMux(repo, eventStore, executor, opts)    main.go:386
```

`sqliteRepository` 只持有**一个** `store`（`sqlite_repository.go:66-76`），`create()` 直接 `r.store.Save`（`:112`），**没有任何按 workspace 的路由**。

`WorkspaceRegistry`（`runtime/workspace.go:496`）确实按 workspace root 打开**额外**的 store，但它的 `WorkspaceInstance` 提供的是 skills 与 MCP 工具面，不是会话存储路由。这解释了为什么 daemon 同时持有 6 个 `sessions.db` 的读写 fd：那是工作区实例，不是会话库切换。

### 实测证据

```text
daemon cwd                       = /Users/xiaoyuan
Case 001 session 20261030-…      = 存在于 xiaoyuan-32d10d908e8b/sessions.db
该库 session 数                  = 906
该库不同 workspace_path 数        = 58
该库中 Case 001 事件数            = 788
```

**一个库承载 58 个不同 workspace 的 906 个会话**。所以"workspace → 库"的映射不存在；`workspace_path` 是会话的一个属性列。

推论：`~/.codeagent/projects/TimelineKit-*/sessions.db` 是**其他运行时**（CLI 按项目启动、嵌入式宿主、或另一次不同 cwd 的 daemon）留下的库，当前 daemon 只是通过全局索引读取它们。

## 2. 只有 Token Hash 时如何定位库

**不需要全局索引。**

- 一个 daemon 只有一个会话库，grants 放进这个库即可；`SELECT … FROM grants WHERE token_hash=?` 就是单库查询。
- 前提：grant 由**签发它的那个 daemon** 使用。manifest 已固定 `endpoint` + `server_id`，天然满足。
- 跨 daemon（不同 cwd 起两个）时 grant 不共享——这是可接受的边界，写进限制即可，不需要全局索引。

原设计里"grants 落 control.db、归属落 sessions.db"的跨库方案**不再需要**，直接落会话库，单事务。

## 3. Turn 准入的真实原子边界

`internal/session/sqlite/turn_input.go:18 ReserveTurnInput`，单事务内依次：

```text
BEGIN
  checkTurnInputAdmission(tx, input)                     -- 准入校验
  SELECT … FROM turn_inputs WHERE session_id=? AND request_id=?
     命中 → 返回既有记录 + false（幂等重放），COMMIT
  INSERT INTO turn_inputs(…)                             -- 状态 accepted
  INSERT INTO session_events(… 'turn_accepted' …)        -- 同事务
  UPDATE turn_inputs SET accepted_seq=?
COMMIT
```

表约束（实测 `sqlite_master`）：

```sql
PRIMARY KEY (session_id, request_id)
UNIQUE (session_id, turn_id)
```

所以：**request_id 幂等由数据库主键保证，且与 `turn_accepted` 同事务**。并发提交同一 request_id 时，后到者要么命中重放分支，要么撞主键失败，不会产生两个 Turn。

Bridge 的 `LookupAdmission`（`handoff.go:83-108`）扫事件是**客户端侧的事后确认**，不是服务端去重机制；它报 `ambiguous durable admission` 只是防御性检查，正常路径下不会出现两条。

执行启动在 `StartTurnInput`（同文件 `:103`），是**另一个事务**，把 state 从 accepted 改为 running 并保存 session。所以边界划分是：

```text
准入（原子，含 turn_accepted） → 启动执行（另一事务）
```

这意味着"已准入但未启动"是一个真实存在的中间态——正是崩溃恢复要处理的。

## 4. sharedDeviceAuthContextKey 旁路

`auth.go:117-120`：请求上下文带 `sharedDeviceAuthContextKey=true` 时**直接放行**，连 token 都不比对。设置点只有一处：`shared_auth.go:230`，由 `SharedDeviceAuthenticator` 在**设备凭据**校验通过后注入。

含义与风险：

- 它是一条**独立认证通道**（设备凭据），与 Owner Token 平行，且等价于 owner 权限。
- Scoped Grant 的 principal 必须**显式排除**这条通道：`principalFromRequest` 应先看 `runtimeOwnerAuthContextKey`，再看 `sharedDeviceAuthContextKey`，两者都视为 owner，**绝不能**把 shared 通道降级为 scoped，也不能让 scoped token 触发它。
- 若将来把设备凭据发给外部客户端，等于绕过整个 grant 体系——这一点必须写进威胁模型。

## 5. Approval ID 是否会进入 scoped 可读的事件

**分两类，结论不同：**

| 路径 | 是否成立 | 依据 |
| --- | --- | --- |
| 审批请求作为 **control 消息**广播给所有 sink | **成立（已证实）** | `approver.go:121-125` 复制 `a.sinks` 全量发送；`ws.go:262-264` 注释 "broadcast to ALL active sinks" |
| 审批 ID 出现在 `/v1/conversations/{id}/events` | **不成立（原判断错误）** | `control.go:22-30` 的 `ApprovalRequest` 是 WS 控制面消息，与 `session_events` 无关；未发现写入事件表的代码路径 |

所以：

- **已证实的泄露路径只有一条**：scoped 连接若被注册为 sink，就会收到带 ID 的审批请求帧。
- ID 形态为 `appr_` + 6 字节随机（`approver.go:690-694`），48 位，无法猜。
- 因此**出站过滤是主要防线，入站校验是兜底**——两者仍都要做，但优先级与我上一版描述相反。

另外注意一个功能性后果：如果 scoped 连接被过滤掉、而 Owner 当时没连，审批请求无人接收，会走到超时默认拒绝。这是"只读派发"必须接受的语义，需要写进文档。

## 6. Owner-only 审批校验能否作为独立小补丁

**可以，且不会回归多设备。**

- `Router` 在 `ws.go:212` 按连接构造，此处 `r` 仍持有认证上下文，可注入 principal。
- 三个裁决入口（`router.go:202-215`）加一道 `if !principal.IsOwner() { reject }` 即可。
- 多设备不受影响：所有 owner 设备经 `WithServerAuth` 或设备凭据通道，principal 都是 owner。
- 当前若尚无 scoped principal，这道判断恒真，**属于 no-op 安全前置**，可以先合入。

出站过滤同理可独立：`RemoteApprover.AddSink` 带上 principal，广播时跳过非 owner sink（`approver.go:121-125`）。

## 7. 修正后的最小实施顺序与数据落点

数据落点统一为**daemon 会话库**（`storePath(cwd)`），不再分库：

| 批次 | 内容 | 依赖 | 关键点 |
| --- | --- | --- | --- |
| P0 | principal 解析（owner / shared-device / scoped） | — | 显式处理 §4 的旁路 |
| P1 | `grants` + `owner_grant_id` + `session_delegations`，落**会话库** | P0 | 与 sessions 同库，单事务 |
| P2 | HTTP 路由动作表（fail closed） | P0 | 中间件不知道路由，需在 mux 层包装 |
| P3 | `visible()` 接入单会话读、事件读、activity 过滤 | P1,P2 | 同库查询，无跨库 |
| P4 | **Owner-only 审批入站**（可独立先行） | P0 | 见 §6，多设备无回归 |
| P5 | 审批出站按 principal 过滤 | P4 | 过滤后无人接收即超时拒绝，需文档化 |
| P6 | 撤销集合 + 关闭连接 | P5 | |
| P7 | workspace 指纹（resolved_path + dev/ino） | P2 | 绑定仍以 `workspace_path` 列判定 |
| P8 | 准入幂等：**验证**而非新建 | — | 已有主键保证，补并发测试即可 |
| P9 | 审计（attempted / outcome 分离） | P2 | 与业务同库可单事务 |
| P10 | manifest `scope` + Chater 签发 UI | P1 | 仓库外 |

与 `4621f7f` 的差异：

- 删除了"跨库事务"这条风险（不存在）。
- P8 从"实现去重"改为"补测试验证已有保证"。
- 新增 P0（principal 要处理 shared-device 旁路）。
- P4 提前，因为它可以独立合入且是 no-op。

## 8. 需要补的测试

| 用例 | 期望 |
| --- | --- |
| 并发提交同一 `request_id`（同一 Session，两个连接） | 只产生一个 `turn_inputs` 行、一条 `turn_accepted`；后到者拿到同一 receipt |
| 同一 `request_id` 不同 `payload_hash` | 冲突错误，不产生第二个 Turn |
| scoped 连接收到审批帧 | 不该收到（P5 后）；收到也不能裁决（P4） |
| owner 多设备同时在线 | 任一设备都能正常裁决（回归） |
| scoped token 走 shared-device 通道 | 必须失败（旁路隔离） |
| 会话库跨 58 个 workspace 的既有数据 | `visible()` 仍按 grant 判定，不因同库而互相可见 |

## 9. 仍未验证

1. `WorkspaceRegistry` 的 `Store` 是否被任何**会话读写**路径使用（目前证据指向仅 skills/MCP）；若有，需重新评估"单库"结论。
2. `checkTurnInputAdmission` 的全部拒绝条件（本次未逐条读完）。
3. 设备凭据通道在外部客户端场景下是否可能被误用（需宿主侧确认）。
4. Chater 是否有授权 UI 骨架（仓库外）。
