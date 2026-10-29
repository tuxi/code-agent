# P0-B2-a 准入与幂等提交

2026-10-28。按 [B2 设计](p0b2-design-and-offline-validation.md) 实施第一批内部能力。没有调用第三方模型，没有替换或重启当前 Daemon，没有改 AgentKit/Talkify、Agent Loop、生成参数或现有 MCP 配置。没有实现 ReviewGate、自动返工、完整启动恢复或父任务 UI。

## 交付范围

| 模块 | 实际变更 |
| --- | --- |
| `internal/collab` | 可选 ManagedRepository/ExecutionGuard、Run/命令/派发/回执 DTO、带稳定错误码的准入错误、工作区/Git 目录身份锚点（Unix；其他平台的受管能力 fail closed） |
| `internal/session/sqlite` | Task schema 1→2；五张新增表及活动 Run/写入者唯一约束；Run+命令+claim+TaskEvent 事务；固定 request_id 的派发 reservation；准入+Turn inbox+accepted event+Attempt 的原子绑定 |
| `internal/conversation` | host-only `AcceptManagedDispatch`；保留目标审批与模型解析；重复投递仅返回原 Turn；统一执行入口保护；未知执行冻结为 recovery_required |
| `internal/collabflux` | 锁定 workflow version/hash；Flux Task 与 command receipt 同事务；同命令去重、冲突拒绝；显式重新入队已知 pending 身份，不调用 Flux Retry |
| B1 投影 | 继续投影 Attempt 生命周期，但不能用旧 Session 终态覆盖受管 Task 阶段、验收和恢复状态 |
| 迁移与测试 | 两库降级保护脚本；临时 SQLite/Git 工作区、脚本执行器、审批阻塞和事务故障回归 |

现有 HTTP/MCP 仍只发布 B1 capability。没有新增 `/runs` HTTP 路由或 Task MCP 工具，也没有注册新协作 DAG。`task_managed_workflow_v1`、`task_review_gate_v1` 保持不可用，避免把内部基础能力包装为完整自动协作。

## 准入边界

1. `PrepareRun` 验证 owner、可执行本地 Participation、reported reviewer、服务端持久化的计划内容/hash，以及固定工作流版本；同一 Task 只有一个活动 Run，同一或嵌套工作区不能被第二个 Run 抢占。
2. Task Store 事务同时写 Run、submit command、workspace claim 和 TaskEvent。Flux adapter 在另一库事务中写 Task/receipt；丢失 ACK 时重投同 command，返回原 Flux ID。ACK 回写 Task/Run/Event 也在单库事务中去重。
3. `ReserveDispatch` 固定消息、可选模型、request_id 和 source ref，不创建虚假的 Turn/Attempt。B2-a 仅允许第一轮；后续 iteration 返回 managed_unavailable，必须等 B2-b ReviewGate。
4. `AcceptManagedDispatch` 不接受任意 prompt、approval mode、max_tokens 或 Thinking 参数，只读取已授权 reservation。新 Turn 在同一 SQLite 事务写入 inbox、匹配的 turn_accepted、精确 Attempt 和 TaskEvent；失败完整回滚。
5. 常规发送、TUI/REPL、跨 Session、Resume 和恢复均经过 TurnExecutor 的公共准入保护。普通 Turn 的临时执行租约关闭了“先通过检查、随后 Run 抢占”的窗口；未创建的子路径通过现有 CanonicalPath 解析，大小写别名按文件身份检查。
6. 活动执行 Session 不能通过换根绕过 claim；Run/Store/Server/epoch/request/工作区或 Git 目录身份不符均拒绝。相同 Turn 继续使用 B1 的全局唯一归属约束。
7. 已有 managed Turn 的重复派发只读回原准入身份，不自动继续执行。未知中断的 inbox 保留，Run/Task 原子转为 waiting/recovery_required；重复恢复不重复产生日志或模型工作。

本批还移除了旧 `AcceptCrossSessionMessage` 对 request 自动设置/清除 `full` 的逻辑。源 Session 的委派许可不再替代目标工具审批；目标会话已有的模式和 Approver 保留。没有审批接收方时按原权限链等待或拒绝，不通过自动提权解决。加载失败时也不会再解引用空 Session。

## 单 Agent 与兼容性

- 普通会话不创建 Task/Run/Event，不启动 Flux，也没有额外模型请求。真实 SQLite 路径新增了临时执行租约的查询、插入和释放，以保证受管 Run 无法抢占在途普通执行；不能声称这部分完全零存储开销。
- 未实现 optional port 的第三方 Repository 维持原行为；受管准入 fail closed。Session 的原审批、工具、模型与推理流程不变。
- `managed_run` 是可选 Snapshot 字段，原 workflow_ref 仍是字符串/null。旧 B1 JSON fixture 的 Go/Swift 检查通过。schema-1 只读 inbox 也能继续读取。
- 旧跨 Session 的自动 full 行为是有意收紧：此前依赖它无人值守写入的流程，现在需要目标既有策略或真实审批，不会静默放行。

## 实际验证

全部使用本机已有依赖，Go 命令带 `GOPROXY=off GOSUMDB=off`，真实模型与现场 GUI 测试显式关闭。新增用例不请求模型：脚本 TurnRunner 在 Approver 放行后才向临时文件写入标记。

| 检查 | 结果 |
| --- | --- |
| collab、collabflux、SQLite、conversation、此前 B2 探针 | 154 项顶层测试通过 race；SQLite 6.674s，conversation 3.113s，Flux adapter 2.006s |
| controlplane、Session、Session tools、MCP Bridge | 136 项顶层测试通过 race；Bridge 17.240s，真实 API/现场测试关闭 |
| HTTP Task owner/optional-port/协议回归 | 2 项通过 race，1.827s |
| 定向 `go vet` | 通过 |
| `go build ./...` | 通过 |
| 原 Go/Swift 共用 fixture | 通过，Swift 编译缓存放在临时目录 |

合计 292 项顶层测试通过 race，子案例另计。保留了两项独立复现的既有失败：

- `TestSequencingEmitterCapsToolStreamPersistence`：持久化字节预算断言失败。
- `TestTurnExecutor_CancelReleasesSchedulerSlot`：测试 fakeEventStore 的并发读写触发 race。

先运行宽范围测试发现上述问题，再将本批前的七个文件恢复到**独立临时源码副本**复跑，两项都失败。原断言没有修改；最终通过集明确排除这两项，日志单独保留，不能表述为整包无条件全绿。本批未重跑或修复此前其余 Runtime/Flux/Server 基线问题，也未运行整个仓库测试套件。

回归覆盖：两库真实命令/回执往返、ACK 丢失和实例重建、并发重复提交只建一个 Flux Task、receipt/command/Attempt 写入故障回滚、精确 Turn 绑定、审批前不写文件、同 Turn 重复归属拒绝、reported executor 拒绝、普通/TUI/嵌套路径/换根冲突、未知执行不恢复、B1 查询不推进受管状态、增量迁移和受保护降级。

首轮沙箱遇到 Go/Clang 缓存访问限制，Go 使用本机权限离线重跑，Swift 改用临时 module cache。所有最终构建与测试均在最后的生产代码修改后执行。

完整日志、改动前文件副本、隔离基线、摘要和本批独立 Diff 存放于 `/private/tmp/codeagent-p0b2a-20261028/`；临时目录不是长期归档。未提交 Git commit，未推送。

## 迁移与回滚

写入 Store 打开时，在一笔 Task schema 事务中将 version 1 升为 2，保留 B1 身份、数据和 Session 表；只读打开不迁移。Flux receipt 表仅在显式构造 adapter 时创建，不因此启动 Runtime 或 worker。

旧 B1 二进制会拒绝写入 version 2，这是防止旧程序无视 claim 的必要边界。部署时须停止旧实例、确认在途执行处理完毕、备份 Session 和 Flux SQLite（包含 WAL 一致性），再统一切换 Runtime；本批没有代用户执行这些操作。

只有五张 B2-a 表和 Flux receipt 全空、所有执行停止时，才可在维护连接分别使用 [Session 降级脚本](migrations/p0b2a-down.sql) 和 [Flux 降级脚本](migrations/p0b2a-flux-down.sql)。脚本遇到非空数据会失败；必须 ROLLBACK 并保留新版，不能跳过 guard、直接使用 B1 的全量删除脚本或只降级一库。存在 Run/命令时需要后续安全终止与导出方案，不能用删除表代替恢复核对。

## 留给 B2-b / B2-c 的边界

- B2-a 不捕获不可变执行 bundle，不消费 PASS，不创建返工或 Task completed。
- 不自动清理过期 claim/执行租约，不按 TTL 抢占。普通进程崩溃留下的租约会阻止新 Run 抢占该工作区，但不会阻止无受管 claim 的普通会话继续运行；清理由 B2-c 在确认原执行停止后处理。
- recovery_required 没有本批可用的“自动继续”入口；完整跨进程 kill/恢复、审批恢复、控制 fencing 属后续阶段。当前实例重建测试不能代替这些故障验证。
- 工作区锚点保护准入，不能阻止编辑器/旧 CLI/其他未接入进程在 Turn 中途改文件；未实现每次工具操作的强制工作区隔离。
- Flux adapter 不决定阶段、不启动 worker。受管 DAG、命令投递接线、工作流版本存储隔离、通用 Retry 入口的受管限制需要在 B2-b/c 完成后再开放 capability。

下一批 B2-b 建立不可变 bundle 和 ReviewGate，再将新 DAG 的域节点接到这里的准入与提交端口；B2-c 完成恢复与故障注入后才发布自动协作能力。
