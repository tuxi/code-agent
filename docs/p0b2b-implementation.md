# P0-B2-b：不可变产物、ReviewGate 与有限返工 DAG

2026-10-28。本批完成**内部实现和离线验证**，未发布受管工作流能力；未启动或替换运行中的 Daemon，未修改 AgentKit/Talkify、Agent Loop、模型选择或生成参数。模型调用、源码外发、模型费用均为 0。现有 MCP 七个 Session 工具保持原契约。

## 实际实现

| 模块 | 本批变化 |
| --- | --- |
| `internal/collab` | 新增可选 `ReviewRepository`、严格审查绑定 DTO、WorkspaceImage 与 ReviewBundle；B2-a Run 增加可选 review policy、基线引用、当前轮次和候选引用 |
| `internal/session/sqlite` | Task schema 2 → 3；增加不可变 blob、reported review、已消费 gate 三张表；与 Run/Task/Event/receipt 同库事务提交 |
| `internal/collabflux` | 新增版本化 `coding_collaboration_v1`、窄域 stage tool、独立 Turn/Review resolver；不改旧工作流，不启动 worker |
| 迁移 | 新增 `p0b2b-down.sql`；旧 B2-a 回滚脚本增加版本保护，必须先正确退回 schema 2 |

`PrepareRun` 可由可信宿主附带 `ReviewPolicy`，显式指定文件、验证命令和用户配置的额外受保护路径。未附带的旧 B2-a Run 仍只允许第一轮，不获得审查或返工能力。新增端口不加入原 `Repository` 必选接口。

### 不可变候选

准备 Run 时保存基线；精确 Turn 结束且执行租约释放后保存候选。bundle 包含计划绑定、Run/epoch/Dispatch/Attempt/Turn、事件水位、基线与候选 Git HEAD、dirty/untracked 状态、完整工作树及 index 文件字节/模式、文件新增/删除信息、文本 diff、index diff、二进制内容和验证日志/结构化证据。二进制字节在 JSON 中使用 base64；重命名可由前后路径及 Git 状态还原，不依赖易变路径读取。diff 供审查，完整 manifest 字节为权威内容，不承诺二进制 patch 可直接 `git apply`。

首版要求仓库已有有效 commit，不自动为新空仓库提交。文件范围外的可见 Git 变更、受保护路径、符号链接、非普通文件、未合并 index、子模块、HEAD 改变或捕获不稳定均拒绝。每个文件最多 2 MiB，单个 image 的工作树/index 字节合计最多 8 MiB，整个序列化 blob 最多 16 MiB；执行记录最多 2 MiB/10,000 条。文本 diff 每文件最多 10,000 行、每个 patch 最多 4 MiB；超限返回错误，不截断后放行。完整 bundle 的组合上限可能比单个文件上限更早触发。

文件内容通过 Unix `openat/O_NOFOLLOW` 读取；复用现有敏感路径默认集合并合并宿主额外限制。Git 子进程禁用可选锁、fsmonitor、全局配置和替换对象，清除继承的 `GIT_*` 覆盖，配置进程组取消及超时；capture 不运行验证命令、不 add/commit/stash。文件及 Git 身份在捕获前后复核，连续两次 manifest hash 必须一致。

blob、review submission 和 gate decision 均插入后禁止 UPDATE/DELETE。Snapshot/Event 只持有候选引用和摘要，不内嵌大 blob。读取产物仍需本地 owner 身份并验证内容 hash；进程重建后返回原字节。

### 三层验收

1. `SubmitReview` 校验 reviewer Participation、Run、epoch、计划 id/version/hash、Attempt 和候选 id/version/hash。只接受 `pass/request_changes/inconclusive`，只返回 `recorded`。同 request 或 producer submission 重复不产生第二个决定；同键不同内容冲突。
2. `ApplyGate` 从持久化决定读取 verdict，再复核当前候选、执行证据和剩余轮数，返回 `gate_accepted`。证据不足、inconclusive、轮数耗尽或候选改变都进入 waiting。request_changes 被消费不等于任务通过。
3. `FinalizeRun` 再次核对当前候选/epoch、PASS 和验证证据，才事务写 `completed/accepted`、TaskEvent 并释放 claim。候选改变则写 `review.invalidated`，保留旧审查和 blob。最终接受的是指定 artifact，而不是对工作区永久不变的承诺。

审批授权与命令成功继续分别判定：correlated `tool_finished + completed + exit 0` 可证明 confirm 策略下实际执行成功，单独 confirm 不算批准或测试成功。缺少最终修改后的验证、未完成工具、未修正参数错误、截断 marker、后台 job 或客户端执行证据都不能自动验收。普通日志中的 `TestTruncate` 或 `job_id` 字样不会被误判成截断/后台任务。

本批还修复了实际离线联调发现的两个边界：`turn_finished` 先于 inbox 终态及租约释放时继续等待；旧轮次恢复回调不能冻结新的返工轮次，也不能重开已验收 Run。

## 有限 DAG 与能力边界

每轮独立展开 `dispatch_N → wait_turn_N → capture_N → wait_review_N → gate_N`，条件边选择 finalize、下一轮 dispatch 或人工干预。`max_iterations` 范围 1–5，包含初次执行；不存在回边或业务 Retry。

派发仅通过 B2-a 的持久 reservation 和 `TurnExecutor.AcceptManagedDispatch`；复用原 Session，保留默认模型与审批。返工必须绑定上一轮已消费的 request_changes 决定，不能靠 iteration 数字或普通 Session send 绕过。每个 stage 和 resolver 校验 Flux Task/version/hash、Run/epoch、节点和轮次。

`coding_task_turn_v1` 读取精确 inbox/终态；`coding_task_review_v1` 只读取持久审查决定。没有 review 就等待。即使有人给 Flux `CompleteAwait` 伪造 pass，下游 gate 也不会据此验收。外部 reviewer 仍是 owner 注册的 reported Participation，不代表客户端生命周期受控或已经证明独立推理。

**仍未装配到服务启动路径**：没有新 `/runs` HTTP/MCP Task 工具、没有发布 `task_managed_workflow_v1/task_review_gate_v1`、没有启动恢复扫描。注册宿主 stage 不会自动启动模型、worker 或新的 Daemon。Flux success 也可能表示流程在 attention 分支结束，不能替代 CodingTask 的验收状态。

## 迁移与回滚

- `ensureTaskSchema` 在同一事务创建新表/触发器并更新版本到 3，保留 Store ID、Session/Turn、B1 和 B2-a 数据。旧 schema 2 写入二进制会拒绝版本 3，不能混用。
- 已有 B2-a Run 的账本保留，但不补造轮次或自动重跑；其执行续跑仍需 B2-c 或人工核对，本批不提供跨进程恢复。
- 未在运行中的用户数据库执行本批迁移；仅 Go 测试临时数据库已迁移。部署前停准入、确认所有执行停止，并备份 Session/Flux 两库。
- 空 B2-b 数据可在维护连接运行 [p0b2b-down.sql](migrations/p0b2b-down.sql) 退到 2，再按需运行 [p0b2a-down.sql](migrations/p0b2a-down.sql) 退到 1。
- B2-b 脚本拒绝删除任何 blob、review、gate 或带 review policy 的 Run；有使用数据时应保留新二进制，或在停止所有执行后成套恢复备份，不手工清表。任何 SQL 错误都必须显式 ROLLBACK。
- 回滚只处理已验证的空增量；Flux 中的 B2-b 任务也不能交给未安装对应 stage/resolver 的旧 worker。

## 验证结果

全部 Go 命令使用 `GOPROXY=off GOSUMDB=off`；无依赖安装。只测受影响内部包与已有技术探针，未运行整个仓库测试套件。

| 检查 | 实际结果 |
| --- | --- |
| 最终受影响包 race + Server Task/identity/model 契约 | 199 个顶层测试、91 个子案例通过；3 个明确需要真实进程/Daemon 的 MCP 测试按原条件跳过 |
| 新边界追加回归 | 旧轮次 callback、已完成 Run、终态/inbox/租约收尾与迁移通过 |
| 真实 TurnExecutor + SQLite + Flux | 脚本执行器实际写隔离 Git 文件、运行本地 `git diff --check`；request_changes → 同 Session 第二轮 → PASS → final acceptance 通过；目标默认模型/ask 审批保持 |
| 负面分支 | 错角色/actor/计划/hash/Attempt/epoch、重复/冲突回报、missing validation、截断、inconclusive、耗尽、取消、伪造 Await、外部变更、活跃 writer、不可变/重启读取及事务失败回滚通过 |
| 重复 race | 核心路径连续 3 次通过，用于验证收尾时序修复 |
| Go/Swift B1 共用 fixture | Foundation 脚本 PASS；未改 Swift/UI |
| 构建与静态检查 | `go build ./...`、受影响包 `go vet` 通过 |

扩大回归另外通过 391 个顶层测试，但有 4 项 Server 失败；在修改前源码的隔离副本中全部复现：`TestProviderStoreUpsertBuiltinFillsModelsFromRegistry`、`TestSharedEnrollmentWaitsForHostAcknowledgement`、`TestSharedEnrollmentTimeoutRevokesPendingStateAndKeepsBootstrapRetryable`、`TestRouterTerminatesEveryPreAcceptFailureWithoutRejectingAcceptedTurns`。没有修改这些测试或实现。之前已独立复现的 `TestSequencingEmitterCapsToolStreamPersistence`、`TestTurnExecutor_CancelReleasesSchedulerSlot` 在本批扩大回归中显式排除，原断言保留。

日志、修改前副本和本批实际 Diff：[本机验证目录](/private/tmp/codeagent-p0b2b-20261028/)、[完整 Diff](/private/tmp/codeagent-p0b2b-20261028/changes.diff)、[验证记录](/private/tmp/codeagent-p0b2b-20261028/REPORT.md)。主工作区本来已有的其他改动未清理、提交或覆盖；本批未推送。

## 尚未完成及整体进度

| 阶段 | 状态 |
| --- | --- |
| P0-A Session MCP 与双 GUI 手工协作 | 已验证真实委派、同 Session 审查返工、用户审批和历史回放；Bridge 仍是单个显式绑定的隔离工作区/Session 实验入口 |
| P0-B1 Task/Session/Participation/Attempt/Event | 已实现，owner-only API 和 Go/Swift 契约保留 |
| B2-a 准入、稳定 command、跨库 receipt | 已实现内部端口 |
| B2-b 不可变候选、门禁、有限 DAG | 本批离线实现完成；未发布生产能力 |
| B2-c | 待启动核对、OS kill 故障注入、未知副作用冻结/准入恢复、worker/resolver 装配和 API/Bridge 能力接线 |
| P0.5 最小父 Task Timeline | 待实现；不属于本批 |
| P1 托管外部 Agent / Adapter | 后续扩展，不作为本批完成条件 |

B2-c 必须在发布前验证：旧后台进程/其他 Runtime 写入者的宿主级静止证明、精确进程与审批恢复、跨库 ACK/入队/Await 各中断窗口、通用 Retry/Resume 路径与受管执行域隔离。当前只检查本 Runtime 的执行租约及精确 Turn 中可见的后台/client/截断信号；不能据此声称任意后台或外部进程已经终止。工作区 claim 也不是操作系统文件锁；外部编辑在双快照/门禁/finalize 检查之间仍有窗口，未来读取后的失效投影仍需 B2-c 接线。没有这些保障时不得打开 managed capability。

因此，最初的“Codex 手工规划/委派/审查 + Code-Agent 执行”已经可用；“持久化受管自动闭环”尚待 B2-c；“在 AgentKit 总览整个任务”尚待 P0.5。不能把本批离线成功等同于自动协作产品已全部完成。
