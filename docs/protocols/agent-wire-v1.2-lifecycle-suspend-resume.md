# Agent Wire Protocol v1.2 — 生命周期契约：Suspend / Resume / Reconfigure

> 状态：契约已对齐（AgentKit 2026-07-01 定稿 §8 五项开放议题）。基于 v1/v1.1 增量，不破已有契约。可开工。
> 实现位置：`internal/agent`(loop checkpoint)+ `internal/conversation`(executor / status)+ `internal/embed`(Handle 生命周期)+ `internal/server`(seq / attach)。
> 前置阅读：[agent-wire-v1.md](agent-wire-v1.md)、[agent-wire-v1.1-client-tool-execution.md](agent-wire-v1.1-client-tool-execution.md)
> 对应需求：iOS 端"挂起→标记暂停→前台续跑",替代当前 `stop()`/`restart()`。

---

## 0. 动机：iOS 端侧 runtime 的生命周期 ≠ 进程生命周期

Claude Code / Cursor 是桌面/服务器进程,Manus 是云端跑、手机做瘦客户端 —— 没有谁是 **iOS 端侧常驻 agent runtime**,所以没有现成生命周期模型可抄。我们是纯端侧,runtime 的存活完全受 iOS app 生命周期支配:退到后台可能被挂起(suspend),内存压力下进程可能被杀(jetsam)。当前 host 在 `.background` 调 `agentRuntime.stop()`、`.active` 调 `restart()`,导致**切走即丢正在执行的 turn**,且 restart 换端口逼 WS 全量重连。

本契约的范式选择:**持久化 turn 状态机 + step 级 checkpoint + 前台重放**。这是"进程会被冻结/杀死"环境下的标准做法,与 host 看不到的 runtime 内部状态(turn 跑到哪、能不能续、怎么续)解耦。

### 0.1 现状已具备(纠正一处常见误解)

runtime **不是**只有"在跑/不在跑"。`conversation.TurnExecutor` 在 turn 边界**且在 cancel/error 时**已落盘 —— 见 [executor.go:95-103](../../internal/conversation/executor.go):

```go
// 5. Save — always, even on error/cancel. ... WithoutCancel keeps the save
//    from being aborted by the turn's (or the caller's) cancellation.
if !sess.IsEmpty() {
    e.repo.Save(context.WithoutCancel(parentCtx), sess)
}
```

也就是说,"被取消时丢全部进度"对我们并不成立 —— **cancel 时是存的**。真正的缺口在:**存下来的那一份在某些取消时机下不自洽,无法直接喂回模型**(见 §2)。本契约要补的正是这个缺口,而不是从零造持久化。

### 0.2 现状已具备的基建

| 能力 | 位置 | 说明 |
|---|---|---|
| turn 互斥 + 取消 | [activeturn.go](../../internal/conversation/activeturn.go) `ActiveTurnRegistry` | `BeginTurn` 单 turn/session;`Cancel` 在 step 边界停 turn |
| ctx 取消传播到在途 LLM 请求 | 全 provider 用 `http.NewRequestWithContext` | cancel ctx → 在途 HTTP 流立刻断 → `Suspend()` 可有界返回 |
| turn 边界 + cancel 落盘 | [executor.go:99](../../internal/conversation/executor.go) | `WithoutCancel` 保证 cancel 不打断 save |
| 持久 turn 序号 | [loop.go:123](../../internal/agent/loop.go) `sess.Metadata["turn_seq"]` | status 字段可沿用同一 Metadata 模式 |
| 事件单调序 | `session_events.id AUTOINCREMENT`,按 `(at, id)` 排序 [store.go:412](../../internal/session/sqlite/store.go) | 天然 per-insert 单调量,可作 wire `seq` |
| 事件回放 | [mux.go:394](../../internal/server/mux.go) `eventStore.Replay` | 改成带 `since` 过滤即可 |
| 进程内换 model(turn 边界) | [tui/run.go:118](../../cmd/codeagent/tui/run.go) | Reconfigure 的先例:swap 落在 turn 边界 |

---

## 1. Turn 状态机

每个 turn 的 `status` 持久化到 session(沿用 `sess.Metadata`,字段名 `turn_status`),取值与转移:

```
                       Suspend()                    ResumeSession()
   ┌──────────┐  ───────────────▶ ┌─────────┐  ────────────────▶ ┌──────────┐
   │ running  │                   │ paused  │ ◀────────────────── │ resuming │
   └────┬─────┘  ◀─── (已 checkpoint) └────┬────┘  瞬时/网络失败(可重试) └────┬─────┘
        │ 完成                              │ 不可恢复(历史损坏/           │ 重发被打断的 step
        ▼                                   │ 模型拒绝/重试上限)           │ 成功 → running
   ┌──────────┐                             ▼                          │
   │  done    │                        ┌────────┐  ◀──────────────────┘
   └──────────┘                        │ failed │   (仅不可恢复)
                                       └────────┘
```

- `running`:turn 在 `RunTurn` 循环内。
- `pausing`(可选瞬时态):`Suspend()` 已调用、checkpoint 进行中。host 一般观察不到,可并入 `paused`。
- `paused`:已 checkpoint,在途活动已停,历史自洽,等待 `ResumeSession()`。**也是可重试失败的落点**(见 §3.2.1)。
- `resuming`:`ResumeSession()` 已触发,正在从持久化历史重发被打断的 step。
- `done` / `failed`:终态。**`failed` 只在不可恢复时进入**(见 §3.2.1),瞬时/网络失败退回 `paused`,不进 `failed`。

**UI 标签完全由 `turn_status` 驱动**(已暂停 / 恢复中 / 思考中),host 不猜。

> 注:turn 在 runtime 内是**短命对象**(`RunTurn` 返回即销毁,[loop.go:199](../../internal/agent/loop.go)),没有常驻 turn 实体。因此 `paused` 挂在 **session** 上,而非"turn 对象"上 —— 这统一了两种场景:**进程存活的挂起**(in-process resume)与**进程被 jetsam 后重启**(重开 session 见到 `paused`,续跑),二者都收敛到"从持久化历史继续"。

---

## 2. Step 级 checkpoint 与一致性(核心)

### 2.1 缺口

loop 边跑边 append `sess.Messages`(assistant 消息 → 每个 tool result,[loop.go:378/387/519](../../internal/agent/loop.go)),但落盘只在 turn 结束/cancel 时整体发生。危险窗口是**多 tool-call 批次中途被取消**:

[loop.go:392-393](../../internal/agent/loop.go) 在每个 tool call **前**查 `ctx.Err()`。若一轮 model 请求 3 个 tool,跑完第 1 个 result 已 append,此时 Suspend cancel → 函数 return → 落盘的 `sess.Messages` 含一条"请求 3 个 tool 的 assistant 消息",却只有 1 条 tool result。**resume 喂回 API → `insufficient tool messages following tool_calls`,直接报错。**

这破坏了 [store.go:11-14](../../internal/session/store.go) 承诺的不变量(tool-result 永不与其 assistant tool_calls 脱节)。

### 2.2 契约

1. **checkpoint 边界 = 每个 loop 迭代末**(assistant + 完整 tool result 批之后),这是唯一自洽的落盘点。
2. **取消发生在 tool 批次中途时**,runtime 必须先把该批次内**剩余未执行的 tool call 补成 cancelled 占位 result**(凑齐 `tool_call_id` 配对)再落盘,或丢弃该 assistant 消息。前者更优 —— 它天然给出"resume 时只重发那一步"的语义。
3. resume 时,因为持久化历史只会停在自洽边界,**永远不存在半截批次**:要么整批 result 已存(跳过),要么停在某次 model 调用之前(重发那次 model 调用)。

### 2.2.1 crash-safe:checkpoint 必须单事务(host 反向约束,已定)

iOS 侧硬约束(#1):**resume 的正确性不能依赖 `Suspend()` 跑完** —— jetsam / 后台窗 underrun 可能在 `Suspend()` 执行到一半(甚至 SQLite 写到一半)直接 SIGKILL 进程。因此迭代末 checkpoint 必须:

- **单条事务、commit-or-nothing**:被 guillotine 时落盘要么是上一个自洽边界、要么这条事务整体未提交,**永不半截**。
  - **现状核对**:`Store.Save` 已是单事务(`BeginTx`…`Commit`,[store.go:183-245](../../internal/session/sqlite/store.go)),SQLite 保证原子提交 —— 消息集的"永不半截"**今天就成立**,§2 只是把这次 `Save` 从 turn 边界移到**每迭代末**调用。
  - **待补(真 to-do,非既定)**:sqlite 包**当前未开 WAL**(全包无 `journal_mode`/`synchronous`/`busy_timeout` pragma,默认 DELETE rollback-journal)。为抗 jetsam mid-write 并支持"写时并发读事件",需显式加 `PRAGMA journal_mode=WAL` + `synchronous=NORMAL` + `busy_timeout`。列为 §9 落地项。
  - **边界澄清**:resume 正确性依赖的是 `Save(sess)`(消息集,单事务);`session_events`(事件日志)是**另一条独立事务**,其半截只影响 UI 重放的完整度,不影响会话可续性。

于是 `Suspend()` 退化成一个**纯优化**(更快断网络 + 干净置 `paused`),而非正确性依赖。配套 host 行为:host 给 `Suspend()` 挂 **2s watchdog** —— `beginBackgroundTask` → `Suspend()` → 不管返回与否,2s 到就 `endBackgroundTask`;因为正确性已由每迭代末的原子 save 兜底,提前放手是安全的。

### 2.2.2 host 侧依赖:iOS 文件保护等级(已定,B — 否则 WAL 架空)

§2.2.1 的前提是"设备被 jetsam / 锁屏时后台仍能原子落盘"。iOS 上有一个专属前提会**直接架空**它:DB 文件的 Data Protection class 若是 `Complete` 或 `CompleteUnlessOpen`,**设备一锁屏文件即被加密不可写**,`Suspend()` 在后台的那次 checkpoint 写会失败,WAL 白开。

- **要求**:DB 及其边车文件设为 **`NSFileProtectionCompleteUntilFirstUserAuthentication`**(首次解锁后即可写,后台/锁屏可写)—— 这也是 Core Data + WAL 在 iOS 上的标准组合。
- **落点(host 侧 to-do)**:DB 在 Application Support(`AgentRuntime` 的 `dataDir`,对应 Go 侧 [embed/server.go:129](../../internal/embed/server.go) 的 `DataDir` → `SetStoreBaseDir`)。host 需**显式核/设**该属性,不用系统默认。
- **WAL 运维提醒**:开 WAL 后有 `-wal` / `-shm` 边车文件。任何 DB 拷贝 / 迁移 / 备份必须带上它们 —— 尤其注意 Go 侧现有两处文件操作:一次性迁移 `copyFile`([runtime/store.go:123](../../internal/runtime/store.go))与损坏隔离 quarantine rename([runtime/store.go:66](../../internal/runtime/store.go)),开 WAL 后都需连带处理边车文件(否则迁移出一个缺 `-wal` 的库 = 丢未 checkpoint 的尾部数据)。

> **关于"挂起时正在调 LLM"**:这是最干净的情形,无需特殊处理。assistant 消息要 `complete()` 返回后才 append([loop.go:378/387](../../internal/agent/loop.go)),所以挂起在 LLM 流里 = 历史未变,resume 直接重发那次 model 调用,丢的只是半截 partial token。**真正需要处理的是 tool 批次中途**(§2.1),不是 LLM 调用。

### 2.3 影响范围(给两边交代)

`RunTurn` 与 `TurnExecutor` 是**所有前端共享**的执行入口([executor.go:14](../../internal/conversation/executor.go))。§2 的修复落在共享层,对 CLI/TUI 是**净收益** —— 它同时修了二者现在 Ctrl-C 后历史可能半截不自洽的隐性 bug。唯一可观察变化是每轮 DB 写更频繁(本地 SQLite,可忽略)。

---

## 3. 生命周期动词:`Suspend()` / `Resume()` / `Reconfigure()`

挂在 `embed.Handle`([embed/server.go:81](../../internal/embed/server.go)),**iOS-only 增量**;CLI(`codeagent serve`,进程级生命周期)与 TUI 不需要,一行不改。

### 3.1 `Suspend(ctx) error` — 有界、快速返回

host 在 `beginBackgroundTask` 宽限窗内调用。语义:

1. 停止接收新活。
2. 对所有 in-flight turn 调 `ActiveTurnRegistry.Cancel`(已存在,[activeturn.go:104](../../internal/conversation/activeturn.go))→ ctx 取消 → 在途 provider HTTP 请求立刻断(全 provider 用 `http.NewRequestWithContext`,已验证)。
3. 按 §2.2 落盘自洽 checkpoint,`turn_status = paused`。
4. flush DB。
5. **不试图把 turn 跑完** —— 窗口太短,干净落盘才是目标。

**返回时限契约(已定,#1)**:目标 **≤ 2s**,上限 **3s**,且必须设计成**即使只有 ~1s 也能完成**(cancel 在途 ctx 是瞬时的,剩下就一次事务写)。iOS 后台窗不是常量(`beginBackgroundTask` 无文档化固定值,系统按热/电量/历史动态给,热压/低电可能仅几秒;`backgroundTimeRemaining` 进后台初期返回哨兵大值不可信),故**不把窗口时长当调度依据**。host 给 `Suspend()` 挂 2s watchdog 强制收工(见 §2.2.1)。因为正确性由 §2.2.1 的原子 save 兜底,`Suspend()` 是优化而非正确性依赖 —— 被 watchdog 提前放手、或被 jetsam 中途砍掉,都不破坏可续性。

### 3.2 `ResumeSession(ctx, sessionID) error` — 显式 per-session 续跑(已定,#4)

**契约修订(相对初稿)**:不做"`Resume()` 扫全部 paused 并 fire"的全局自动续跑,改为 **host 显式指定会话** `ResumeSession(ctx, sessionID)`。由 host 决定对哪个会话、何时续。语义:该 session 置 `resuming` → 从持久化历史续跑被打断的 turn → 完成后 `running`→`done`,并发 `turn.resumed` 事件。

理由(host 侧分流,host 能可靠区分):

| 场景 | host 判定 | 策略 |
|---|---|---|
| 切 app 回来(suspend→thaw,同进程) | 单例 `server != nil` 且端口活 | **静默自动续跑**,且只续用户正在看的那个会话 → host 立即对当前会话调 `ResumeSession` |
| jetsam 冷启动(可能隔很久) | 冷启动 + `server == nil` + DB 有 `paused` | **不自动开火**。会话内显示「上次任务被系统中断 · 中断于 X 分钟前 · 继续」,**用户点按才** `ResumeSession` |

冷启动不自动开火:隔了未知时长、烧 token、iOS 工具面会写文件/改 workspace,用户不在场时静默重放不合适。切 app 高频且用户"没离开",必须零摩擦自动。

**需要 Go 提供的两样**:
1. **`paused_at`(unix ts)写进 `sess.Metadata`** —— host 算 staleness、渲染「中断于 X 分钟前」。
2. **按 `turn_status == paused` 列出会话** —— 复用现有 list + status 字段即可,**不新增 API**(status/paused_at 已在 Metadata,list 投影带出即可)。

实现续跑需要一个**不追加新 user 消息**的入口。现 `RunTurn` 硬 append user message([loop.go:215](../../internal/agent/loop.go)),不能直接复用 —— 新增 `Runner.ResumeTurn(ctx, sess)`:跳过 append、直接进 for 循环、用已存历史接着调模型。纯增量,不改 `RunTurn` 现有行为。**边界:host 只调 `ResumeSession`(embed.Handle 层);`ResumeTurn` 是 Go 内部 fan-out,host 永不直接碰。**

**幂等**:`Suspend()`/`ResumeSession()` 都必须幂等。短暂后台(OS 未真正冻结,turn 仍在跑)时,`ResumeSession()` 对已 `running` 的 session no-op;重复 `Suspend()` 对已 `paused` 的 session no-op。

### 3.2.1 resume 失败分类(已定,A — §2 之上的前置语义)

iOS 上**最高频的 resume 失败是瞬时网络**:刚回前台、WiFi/蜂窝还没起来,`ResumeSession` 重发 model 调用即失败。若一律转 `failed`(终态),会把明明可续的 turn 打死、用户白丢任务。故失败分两类,`ResumeTurn` 据此决定往哪转:

| 失败类 | 判定 | 转移 | host 行为 |
|---|---|---|---|
| **瞬时/可重试** | 网络错误、provider 5xx/超时、ctx 取消(又被挂起) | **退回 `paused`**,不进 `failed` | 网络恢复或用户再点「继续」时重试 `ResumeSession` |
| **不可恢复** | 持久化历史损坏、模型明确拒绝该历史、达到重试上限 | `failed`(终态) | 显示失败,不再自动重试 |

判定**直接复用现成的导出分类器** `model.IsRetryable(err)`([resilient.go:238](../../internal/model/resilient.go))—— 注释明说它就是"给更高层用同一套 transient-vs-permanent 策略"(timeout/5xx/网络 → 可重试;auth/bad-request → 永久)。`ResumeTurn` 映射:`IsRetryable → paused`;非 retryable(或历史损坏/模型拒绝)→ `failed`。**重试上限**由 runtime 记在 `sess.Metadata`(如 `resume_attempts`),超限即使 retryable 也升 `failed` —— 防止一个永远失败的历史被 host 无限点「继续」。

**`resume_attempts` 成功即清零**:一次续跑成功(turn 回到 `running`→`done`)必须把计数归 0。否则同一 session 后续正常的 suspend/resume 会继承旧计数,早早误升 `failed`。计数只在"连续失败"语义下累加。

### 3.2.2 stranded-`paused`:静默 thaw 路径缺重试驱动(已定,评审补)

§3.2.1 的"退回 `paused` 等重试"隐含一个触发器,但它**只存在于冷启动路径**(那里有「继续」按钮)。§3.2 的**同进程 thaw 是静默自动续跑,故意不显示「继续」入口** —— 于是这个高频序列会 strand:

> 回前台 → host 静默 `ResumeSession` → WiFi 刚回还没就绪(1–2s,高频)→ `IsRetryable` → 退回 `paused` → **静默路径无「继续」按钮、无其它驱动** → turn 卡 `paused`,直到用户碰巧再切一次前后台。

契约给静默路径补一个重试驱动(host 侧,**首选**):

- host 订阅网络可达性(`NWPathMonitor`),`.satisfied` 恢复时,对当前会话中仍处 `paused` 的 turn 重放一次 `ResumeSession`。对用户完全无感,契合 thaw"零摩擦"初衷。
- 降级备选:静默路径**首次** transient 失败后也显示「继续」入口(退化成冷启动 UX)。

对 Go 侧无新接口 —— `ResumeSession` 幂等 + `paused` 可重入已覆盖;这是纯 host 触发策略,落在步骤 3(`ResumeSession` 绑 scenePhase)时实现,不阻塞 §2。

### 3.3 `Reconfigure(secrets, model) error` — 热加载,替代 `restart()`

现 `StartServer` 每次 `net.Listen("127.0.0.1:0")` 拿新 ephemeral 端口([embed/server.go:177](../../internal/embed/server.go))→ 端口 churn → 逼 WS 全量重连。`Reconfigure` 在 **不掉 server、不换 port** 的前提下换 provider+model:

- provider/model 集中在 `ServeRunBuilder`([embed/server.go:260](../../internal/embed/server.go)),`Reconfigure` 在 builder 外加锁替换。
- 换 model 落在 turn 边界(**已有先例**:TUI 的 `/use` 也是 turn 边界 swap,[tui/run.go:118](../../cmd/codeagent/tui/run.go))。running 中调用则 defer 到下一 step;paused 中调用立即生效。

设置页改 API key / model 走 `Reconfigure`,不再有端口 churn 与全量重连。

**粒度锁定(已定,#5)**:签名就 `Reconfigure(secrets, model)`,**不扩**。换 workspace 会动 FS 根、换 profile 会重建 tool registry/sandbox,重建面太大 → 走**全 restart**。host 侧换 workspace/project 是 `ProjectsStore` 的另一条路,承受得起重启。

### 3.4 `Stop()` 退化

`Stop()`([embed/server.go:96](../../internal/embed/server.go))只在**真正销毁**时用:用户显式退出、收到内存告警(jetsam 前兆)。**后台不再调。**

新生命周期:**进程内 start 一次(幂等)→ 后台 Suspend / 前台 Resume → 改配置 Reconfigure → 极少数 Stop。**

---

## 4. 事件 seq 与 `attach(since:)` 重放

### 4.1 seq 来源

`EventRecord` 现无 wire 级 seq;`MessageView.Seq` 只是投影时数组下标(不稳定,[mux.go:241](../../internal/server/mux.go))。**复用** `session_events.id AUTOINCREMENT`(按 `(at, id)` 排序,[store.go:412](../../internal/session/sqlite/store.go))作为 per-session 单调 `seq`,暴露到每条 wire 事件:`(conversation_id, seq)`。

**类型/语义锁定(已定,#3)**:
- wire `seq` 类型 = **int64**(iOS arm64,Swift `Int` 即 64-bit;host 显式存 `Int64`)。
- wire `seq` = **裸 `session_events.id`**,不是 `(at, id)` 复合。故 `since` 过滤是干净的 `WHERE session_id=? AND id > ?`。id 全表递增(跨 session)无妨:单会话子集内仍单调,且 jetsam 重启后不重置(值持久在 DB 文件)。`SessionEventsSince` **就是这个语义**。

### 4.2 attach(since)

给回放路径([mux.go:394](../../internal/server/mux.go) `eventStore.Replay`)加 `SessionEventsSince(sessionID, sinceSeq)`,WS attach 带 `since_seq` 参数:runtime 回放 `seq > since` 的事件后转入实时流。host 侧每会话维护 `lastSeq`。

### 4.3 显式生命周期事件

新增事件 kind(纯增量):`turn.paused` / `turn.resuming` / `turn.resumed` / `turn.failed`。UI 据此切"已暂停 / 恢复中 / 思考中"。

---

## 5. tool 幂等/结果可查

resume 时若某 tool 在挂起瞬间正在执行(goroutine 被冻结),thaw 后可能跑完也可能被杀。契约:runtime 需知某已派发 tool call **是否已落盘 result** —— 有→用缓存;无→重发或标 failed。

**对 iOS 基本免单**,两点:
1. §2 的 checkpoint 选在迭代末 → **永不持久化半个批次** → resume 不存在"半截 result"。
2. iOS 是 **sandboxed profile,非幂等的 subprocess 工具压根没注册**([registry.go:62/84](../../internal/runtime/registry.go) 的 `AllowsSubprocess()` 门)。iOS 工具面 = filesystem + go-git + web + skills + todo,基本覆盖写/可重试;`run_command`/`bash` 这类"重跑有副作用"的不存在。

> **强一致保证限定在被挂起的 host(iOS)**。Mac(非 sandboxed)有真实 exec 的 `run_command`/git,"挂起→thaw→重发"对它们不安全 —— 但 Mac 长驻进程不会被 OS 挂起,现实里不触发,故 Mac 维持现状。

---

## 6. 对各前端的影响汇总

| 改动 | 落点 | CLI | TUI | Mac(非 sandbox) | iOS |
|---|---|---|---|---|---|
| §1 turn status | `sess.Metadata` 增量字段 | 透明 | 透明 | 透明 | 驱动 UI 标签 |
| §2 checkpoint + 批次自洽 | **共享** `RunTurn`/executor | 受益(修 Ctrl-C 半截 bug) | 受益 | 受益 | 续跑前提 |
| §3.2 `ResumeTurn` 入口 | 新增方法,不动 `RunTurn` | 零影响 | 可选用于 `/resume` | 可选 | 核心(经 `ResumeSession` 调) |
| §3 Suspend/ResumeSession/Reconfigure | `embed.Handle` | **不存在** | 不需要 | 可忽略/仅退出用 | 核心 |
| §4 seq + attach(since) | event store + wire | 透明 | 透明 | 透明 | 重连重放 |
| §5 tool 幂等 | tool 实现 | 现状 | 现状 | 现状(不挂起不触发) | 基本免单 |

**净判断:** 生命周期动词是 iOS-only 增量,共享层(status/checkpoint/seq)是语义无害增量写。Mac/CLI/TUI 无回归;CLI/TUI 反而白捡一个 cancel 一致性修复。

---

## 7. 分工清单(可直接贴给两边)

| Go runtime 侧 | Swift / AgentKit 侧 |
|---|---|
| 持久化 turn `status` + `paused_at`(`sess.Metadata`)+ 生命周期事件 | 后台不再 `stop()`;`beginBackgroundTask` 窗内调 `suspendRuntime()`,挂 2s watchdog |
| §2 每迭代末原子 checkpoint + cancel-mid-batch 自洽(补 cancelled result) | 同进程 thaw:立即对当前会话 `ResumeSession`;冷启动:列 paused、渲染「继续」入口、点按才 `ResumeSession` |
| **开 WAL**:`journal_mode=WAL`+`synchronous=NORMAL`+`busy_timeout`;`copyFile`/quarantine 连带 `-wal`/`-shm` 边车(§2.2.2) | 每会话维护 `lastSeq`(Int64) |
| `Runner.ResumeTurn` + resume 失败分类(`IsRetryable→paused`,否则 `failed`,§3.2.1)+ `resume_attempts` 成功清零 | `turn.paused/resuming/resumed/failed` → UI 态 |
| — | `NWPathMonitor` 可达性恢复 → 对仍 `paused` 的当前会话重放 `ResumeSession`(§3.2.2 stranded 修复) |
| — | **设 DB 文件保护等级** `NSFileProtectionCompleteUntilFirstUserAuthentication`(§2.2.2,否则锁屏 WAL 白开) |
| `embed.Handle.Suspend/ResumeSession(id)/Reconfigure(secrets,model)`(有界、幂等) | 设置页改用 `reconfigure(secrets:model:)` 取代 `restart()` |
| start 幂等 guard;`Reconfigure` 替代端口 churn | scenePhase 改 ensure/suspend/resume(删 start/stop);host wrapper 别名 `suspendRuntime`/`resumeRuntime`(避 Swift `resume()` 命名冲突) |
| 事件带 seq(裸 `session_events.id`)+ `SessionEventsSince(WHERE id > ?)` | 按 `turn_status==paused` 列会话(复用 list,无新 API) |

---

## 8. 开放项结论(AgentKit 已定,2026-07-01)

1. **`Suspend()` 返回时限 — 已定**:目标 ≤ 2s,上限 3s,须能在 ~1s 内完成。**核心不是数字,是"resume 正确性不依赖 Suspend 跑完"** → 反向给 Go 加硬约束:每迭代末 checkpoint 必须**单事务 crash-safe**(见 §2.2.1)。host 挂 2s watchdog。
2. **verb 命名 — 已定**:wire/Go 侧 `Suspend`/`ResumeSession`/`Reconfigure`/`ResumeTurn` 全 OK。host 侧加别名 `suspendRuntime`/`resumeRuntime`/`reconfigure(secrets:model:)`/`ensureStarted`(避开 Swift `URLSessionTask.resume()`/`Continuation.resume()` 命名冲突)。host 只调 `ResumeSession`,`ResumeTurn` 是 Go 内部 fan-out。
3. **`seq` 类型 — 已定**:int64,= 裸 `session_events.id`,`since` 过滤 `WHERE session_id=? AND id > ?`(见 §4.1)。
4. **jetsam 冷启动 — 已定(结构性修订)**:**不做 blanket auto-resume**。改 `Resume()` 全局扫为显式 `ResumeSession(ctx, sessionID)` + `paused_at` 元数据。同进程 thaw = 静默自动续当前会话;冷启动 = 列 paused、渲染「继续」入口、用户点按才续(见 §3.2)。
5. **Reconfigure 粒度 — 已定**:仅 `(secrets, model)`,不扩;换 workspace/profile 走全 restart(见 §3.3)。

> 净结构性变更一处:#4 把 `Resume()`(全局自动)→ `ResumeSession(id)`(显式)+ `paused_at`。其余为确认或收紧数字。契约据此定稿,§2/§3/§4/§7 已同步。

**终稿评审补两处真实缺口(AgentKit 复读发现):**

- **A. resume 失败语义**:瞬时/网络失败退回 `paused`(可重试),`failed` 只留给不可恢复 —— 否则回前台网络未起就把可续 turn 打死。复用 `model.IsRetryable`。见 §1 状态机 + §3.2.1。**这是 §2 之上 `ResumeTurn` 的前置语义,不定则失败往哪转无法编码。**
- **B. iOS DB 文件保护等级**:DB 须设 `NSFileProtectionCompleteUntilFirstUserAuthentication`,否则锁屏文件加密不可写,后台 checkpoint 失败,§2.2.1 的 WAL 被架空。host 侧 to-do。见 §2.2.2 + §7。顺带发现 Go 侧 `copyFile`([runtime/store.go:123](../../internal/runtime/store.go))/quarantine([runtime/store.go:66](../../internal/runtime/store.go))开 WAL 后需连带边车文件。
- **C. stranded-`paused`(评审二轮补)**:A 的"退回 paused 等重试"触发器只在冷启动路径(有「继续」按钮),静默 thaw 路径无驱动 → 回前台网络未就绪会卡死。host 用 `NWPathMonitor` 可达性恢复驱动重试(§3.2.2)。另 `resume_attempts` 成功须清零(§3.2.1)。均 host/Go 各自小改,不阻塞 §2。

---

## 9. 落地次序(契约已定,可开工)

1. **§2 先行**:每迭代末原子 checkpoint + cancel-mid-batch 自洽 + **开 WAL/synchronous/busy_timeout** 且 `copyFile`/quarantine 连带边车(§2.2.1/§2.2.2)。独立有价值(修 CLI/TUI Ctrl-C 半截 bug),可单独上;不被 host 侧策略阻塞。host 侧并行做 DB 文件保护等级(§2.2.2 B),两边汇合前各自可测。
   - **Go 侧状态:已实现(本分支,待评审)**。WAL/synchronous=NORMAL/busy_timeout 经 DSN `_pragma` 落在 [sqlite/store.go `open`](../../internal/session/sqlite/store.go);边车随迁移/隔离([runtime/store.go `copyDB`/quarantine](../../internal/runtime/store.go));cancel-mid-batch 补齐 + 每迭代末 `Checkpointer`([agent/loop.go `RunTurn`](../../internal/agent/loop.go));serve 路径经 `RuntimeContext.Checkpointer` → `repoCheckpointer`(WithoutCancel,best-effort)接线。CLI/TUI `Checkpointer` 留 nil,行为不变。测试:`TestCancelMidBatchLeavesResumableHistory`、`TestCheckpointerCalledPerToolIteration` 等。**host 侧文件保护等级仍 pending。**
2. **`Runner.ResumeTurn`(含失败分类 §3.2.1)+ §1 turn_status + `paused_at`**:建立在 1 的自洽历史之上。
   - **Go 侧状态:已实现**。`ResumeTurn`(drive() 抽取,不追加 user)+ `turn_resumed/paused/failed` 事件;`session` 的 turn_status/paused_at/resume_attempts 助手 + Meta 投影;executor 的 running/done/paused 状态转移、`Resume` 失败分类(`IsRetryable→paused` 计数、超 5 次/不可恢复→`failed`、取消→`paused`)、`ReconcileInterrupted` 冷启动归一。
3. **`embed.Handle.Suspend/ResumeSession(id)`**:包 1+2,绑 scenePhase;host 挂 2s watchdog。
   - **Go 侧状态:已实现**。`ActiveTurnRegistry.SuspendAll`(取消+有界等待)+ `WasSuspended`;`ServeRunBuilder.Reconfigure`(RWMutex 热切);`Assemble` 返回 Runtime bundle;`Handle.Suspend`(有界 2s)/`ResumeSession`(异步)/`Reconfigure`;`mobile.Server` 三个绑定方法 + Stop 文档改为仅销毁。iOS 已打包集成。
4. **§4 seq + attach(since)**:消灭重连全量重放。
   - **Go 侧状态:已实现**。event `seq` = 裸 `session_events.id`,`RecordEvent` 回传 seq;`SessionEventsSince`/`ReplaySince`;`sequencingEmitter` 持久化即回填 live 事件 seq(与 replay 一致);wire `seq` 字段 + `GET …/events?since=<seq>` 增量重放。host 每会话记 `lastSeq`,重连调 `getEvents(since:)`。测试:`TestMuxGetEventsSince`、`TestSessionEventsSeqAndSince`、`TestSequencingEmitterStampsLiveSeq`。`Reconfigure(secrets,model)` 已在步骤 3 落地(消灭端口 churn)。
