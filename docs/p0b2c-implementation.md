# P0-B2-c：受管协作装配与保守恢复

2026-10-28。本批只做本机离线实现和测试，未替换正在运行的 Talkify Daemon，未调用第三方模型，未修改 AgentKit UI、Agent Loop 或模型生成参数。构建新二进制并不等于已在用户桌面部署或完成真实模型验收。

## 装配与模式

`codeagentd` 通过可选 Repository Port 打开 `collabflux.Service`。它复用同一 Session SQLite、TurnExecutor 和审批系统；另在 Daemon 状态目录的 `managed-collaboration/flux.db` 中保存私有受管 Flux 执行域，不使用普通 `plan_workflow` Runtime 池。没有新建 Daemon 或 Supervisor 服务。

启动顺序为：取得 Store 级独占宿主锁 → 打开私有 Flux 数据库并核对 server/store/owner → 注册 stage → 核对活动 Run、租约和工作区 → 恢复已证明安全的队列身份 → 启动 worker → 发布 ready。锁竞争、身份不符或恢复存储错误时不发布受管能力；普通 Session API 继续可用。单个有歧义的 Run 会被冻结，不必关闭其他安全任务。

| 模式 | 调度权与数据 |
| --- | --- |
| Manual Collaboration | 原有七个 Session MCP 工具保留。调用方决定发送、等待、审查和是否返工；可选关联 B1 Task，不强制经过 Flux |
| Managed Collaboration | Flux 选择有限 DAG 的阶段；通过精确持久 dispatch 准入 Turn，自动等待、捕获、消费已报告审查和有限返工；外部 reviewer 仍是 reported，不托管 Codex 客户端 |

两种模式复用 Task/Participation/Session/Attempt/Event。受管工作区 claim 存在时普通派发不能取得写租约；手工 Task 关联工具也拒绝接管受管 Attempt。调用方提交审查只得到 `recorded`，最终以 CodingTask 的 `task_acceptance=accepted` 为准，不能只看 Flux `succeeded`。

## 持久身份与恢复规则

- Session 库仍是 Task schema **3**；Run 新增可选 `recovery_reason`，Dispatch 新增可选 `expected_workspace_hash`。没有本批 Session 表结构迁移或破坏性数据迁移。
- Store 旁 `.managed.lock` 使用 Unix `flock` 和 `O_NOFOLLOW`。只证明本 Store 的受管宿主互斥，不是工作区文件锁；非 Unix 宿主不启用本能力。
- 私有 Flux 库新增 `codeagent_collaboration_identity`，绑定 server/store/owner。沿用稳定 command、提交 receipt、精确 workflow version/hash、Flux Task 和 Turn 身份。
- Flux task 与 receipt 在 Flux 库同一事务提交；Session 库 ACK 可重放同一个 receipt。没有跨库原子事务或通用 Outbox。ACK 丢失不分配新 command/Run/Task。
- `expected_workspace_hash` 固定未准入 reservation 的候选；重启时只有工作区未变、没有存活执行租约，才允许继续原 reservation。
- 已准入且结果未知的 Turn **绝不自动重跑**。Run 转 `recovery_required`，epoch 递增一次，TaskEvent 同事务保存，claim 和旧执行租约保留。旧 epoch 回调失效，重复恢复不重复增 epoch。
- 已确认完成的 Turn 只观察原 inbox/终态，再捕获产物；不重新发送用户消息。等待审批时重启不自动批准，也不复用旧审批。
- 等待人工处理的已知状态保留。PASS 后候选改变是 `waiting / candidate_invalidated`，不会伪装成未知执行结果；旧 review/bundle 保留，不能据此完成新代码。
- 已完成 Task 重启只读原结果，无新 Turn、修改或里程碑。

恢复只针对固定模板的幂等领域 stage。没有调用 Flux 通用 Retry，也不重置已完成节点、Await binding 或 pending edges。尚未满足的 Await 保持 suspended；typed Turn/Review resolver 从持久记录取得结果后，通过 Flux `CompleteAwait` 投递。已完成 Await 的续边在核对后以原 Task ID 重新入队。

私有投递器每 500ms 读取持久 typed binding；它不决定是否返工或验收。当前 Flux 通用轮询默认间隔较长，因此未复用其默认等待周期。API 与后台 command 投递串行化，避免第二个投递者将正常运行误判成未知结果。

模板通过 Flux 原有 Workflow/WorkflowVersion Repository 按 hash 复用不可变版本。没有每次调用 `RegisterWorkflow`：当前依赖会拒绝同进程同名重复注册。1–5 轮的不同 DAG 可以共存，每个 Run 固定 version ID；同请求重试仍由 Session 库 receipt 检查完整输入一致性。

## 首版执行安全边界

能力目录明确返回 `execution_profile=local_foreground_v1`、`automatic_turn_replay=false`、`unknown_side_effects=recovery_required`。

受管 Turn 使用独立的本地工具子集：文件读取/编辑、grep、todo、skill、Git 查询和前台命令。保留具体工具原有路径与审批接口。只在受管副本中关闭 `run_command.background`，不修改普通 Session 的共享工具实例、默认模型或生成配置。客户端工具、外部 MCP 工具、递归工作流和子 Agent 不进入这个执行域。

普通后台 job 在进程启动前取得持久执行租约，在进程树清理后释放；普通工作流/Headless 的每次嵌套工具执行也经过可选工作区 guard，防止它们在原 Turn 结束后进入受管 claim。未装配 guard 的外部 Store 实现保留原接口兼容性，不会凭空获得受管能力。

对已有后台/client/subagent 历史，首版采取保守拒绝：即使日志看似结束，也不把它当作进程已停止的证明。每 Session 的相关历史扫描上限 16 MiB/30,000 条；无法归属现存 Session 的 job/task bracket 会阻止受管准入。该限制可能阻止合法历史工作区，属于当前明确的可用性限制，而非自动清除占用的理由。

所有租约约束只覆盖参与同一 Store 的 Runtime。外部编辑器、其他 Store/非参与进程和命令内部主动脱离管理的任意子进程不是 OS 沙箱可以证明的对象。双快照、Gate、Finalize 会检查候选，但不承诺文件永久不变。不能把本批实现描述为任意脚本的 exactly-once 执行或无限制无人值守恢复。

本批没有 force-resume、强制删租约或审批自动恢复入口。`recovery_required` 需要先由操作者核对执行进程、候选和账本；不能简单清表再重试。完整人工恢复控制协议留待后续。

## API、MCP 与权限

新增 owner-only HTTP 接口：

| 接口 | 作用 |
| --- | --- |
| `POST /v1/tasks/{id}/runs` | 从计划、executor/reviewer Participation、文件及检查命令准备受管 Run |
| `POST /v1/tasks/{id}/runs/{run}/reviews` | 保存严格绑定 Run/epoch/Attempt/计划/候选的 reported review |
| `GET /v1/tasks/{id}/immutable-artifacts/{artifact}` | 读取并校验冻结 ReviewBundle |
| `GET /v1/tasks/capabilities` | ready 后增加 `task_managed_workflow_v1`、`task_review_gate_v1` 和执行边界 |

沿用 owner Bearer；共享设备和匿名客户端不取得这些权限。未装配的宿主保持旧接口；服务失去 ready 时返回不可用，不降级为手工执行。不接受调用方指定 workflow 身份、受保护路径覆盖、Thinking 或输出参数。

MCP 新增 11 个 Task 工具：create_task、register_task_agent、add_task_participation、report_task、link_task_turn、link_task_artifact、read_task、read_task_events、start_task_run、read_review_bundle、review_task_run，均使用 `codeagent_` 前缀。原 Session 工具保持原契约。

Bridge 仍限定于一个显式授权的 Runtime/Store/Workspace/Session。每次 Task 操作复核身份与绑定；不能因为持有 owner Bearer 就读取任意 Task 或另一个本地 Session。Snapshot 查询带 scope，由服务端在序列化计划前拒绝越界；bundle 再验证精确 Attempt/Turn 来源。没有扩大现有本机授权、注册全局 MCP 或自动重发中断请求。

原 MCP SDK 会将 `json.RawMessage` 推断成字节数组，真实调用中暴露了返回 schema 不匹配；Task/Event/Bundle 的外层输出 schema 已改为开放对象，Go DTO 和来源校验仍保留。大 bundle 使用有上限的响应空间，其余请求原限制保持。

## 真实故障矩阵与证据

测试通过原生 stdio MCP 调用独立 OS 子进程中的 HTTP mux、TurnExecutor、Session SQLite 和私有 Flux service；父进程真实发送 SIGKILL，并断言退出信号。不是仅关闭并重开内存对象。它复用生产组件，**不是启动用户桌面的 stock codeagentd 二进制**。

| 故障窗口 | 恢复结果 | 最终修改次数 |
| --- | --- | --- |
| Run 已准备、提交前 | 投递原 command 后完成 | 1 |
| Flux Task/receipt 已提交、Session ACK 前 | 复用原 Task、补 ACK 后完成 | 1 |
| dispatch 已 reserved、Turn 准入前 | 校验候选未变后继续原 dispatch | 1 |
| Turn 已准入、节点输出前 | `recovery_required`，不重跑 | 0 |
| 文件已写、终态前 | `recovery_required`，保留 claim/lease | 1 |
| 真实 RemoteApprover 等待中 | `recovery_required`，旧审批失效，不自动允许 | 0 |
| 精确 Turn 已结束、捕获前 | 观察原终态、捕获后完成 | 1 |
| bundle 冻结、review Await 前 | 使用原 bundle 继续 | 1 |
| review 已保存、调用回执前 | 消费原决定后 Gate/Finalize | 1 |
| Gate 已消费、Finalize 前 | 再校验原候选后完成 | 1 |
| Gate 后文件被外部修改并重启 | `candidate_invalidated`，不验收、不重跑 | 1 |
| 已验收任务重复恢复 | 无新事件/Turn/修改 | 保持 2 轮历史 |

另外用独立 Flux 子进程在 **Await 已提交、无 worker 消费 pending edges** 时 SIGKILL；重开同一数据库后只执行续边，不重复已完成 implement 节点。这是依赖级精确窗口探针，与上表 Daemon 集成测试分开报告。

完整原生 MCP 闭环使用未经修改的真实 `agent.Runner`、进程内脚本 Provider 和真实 read_file/edit_file/run_command：计划 → 第一轮 → 冻结候选 → Bridge 进程重启 → request_changes → 原 Session 第二轮 → PASS → Gate → Finalize → Daemon 重启后重复读取。实际修改 2 次；重复 start 返回原 Run；已完成恢复后事件增量为空。脚本只证明协议/恢复行为，不衡量真实模型质量或 Codex 独立审查能力。精确故障窗口使用可阻塞的脚本 TurnRunner。

还覆盖宿主锁竞争、不同 Store 误用同一 Flux 库、模板 hash 复用、旧 callback、alias 工作区、改动后的未准入候选、手工/受管隔离、前台工具和后台 job 租约。Go/Swift 原 B1 共用 JSON fixture 继续通过；未修改 Swift/UI。

| 最终验证 | 结果 |
| --- | --- |
| 11 个受影响包的扩大 race 回归 | 530 个顶层测试及 260 个子案例通过；7 个按原条件跳过；显式排除 11 项已有基线问题 |
| 模板修复后的 Flux 全包与 B2 技术探针 | 无用例排除；19 个顶层测试及 19 个子案例通过，包含十个 Daemon SIGKILL 窗口及新增候选失效/精确 Await 提交窗口 |
| 最后追加的 owner/Bridge scope 回归 | 2 项通过；错误工作区及同工作区的另一个 Session 均在返回计划前拒绝 |
| 构建、静态检查、Swift fixture | `go build ./...`、受影响包 `go vet`、Foundation 共用 JSON 检查通过 |

没有运行全仓库测试或修复此前登记的基线失败。各轮结果存在重叠，不能将测试数量相加当作独立覆盖率。候选失效新测试的初始状态预期经用户确认后校正为 `candidate_invalidated`；随后按真实模型同时检查 reported 计划 Attempt 与唯一执行 Turn，保留未验收、不重跑和身份不变断言，未为测试修改生产 Finalize 行为。

完整测试命令、当前结果和真实日志见 [REPORT.md](/private/tmp/codeagent-p0b2c-20261028/REPORT.md)。[故障进程日志与 Snapshot](/private/tmp/codeagent-p0b2c-20261028/fault-evidence/)只导出测试日志、计数、checkpoint、审批和 Snapshot，不导出连接清单/令牌。

## 发布与回滚

1. 在维护窗口停止新任务和所有写入者；确认没有需要保留的运行中 Turn、工作流或后台进程。备份 Session SQLite 与私有受管 Flux 库，记录 Store/Server 身份；停止所有连接后成套备份，或采用 SQLite 正确的在线备份方法，不单独复制活跃 WAL 的主文件。
2. 使用构建通过的新 codeagentd；保持原 Session DB、Server ID 和授权策略。启动时自动执行上述核对。受管能力仅在 ready 后启用；首次打开创建私有 Flux 目录/身份行/锁文件，Session schema 保持 3。
3. 查询 capabilities/Snapshot 做只读核对，再在隔离工作区验收；重新授权本机 Bridge 仍由用户进行，进程身份变更不绕过既有授权。不要把本批离线成功当作当前 Talkify 已升级。
4. 回滚前停止全部调度和写入。没有活动受管 Run 时可退回 B2-b 二进制，它不装配 managed API/capability；保留两库、claim、epoch、blob、review 和 receipt，不删除受管目录来“解除”错误。
5. 有活动/未知 Run 时不要把旧 worker 接管成普通任务；先人工核对，保留新二进制只读证据，或停机后成套恢复先前备份。不能只恢复一个库，更不能恢复后任意重发历史副作用。
6. B2-c 没有 schema down 脚本需求。若退到 schema 2 或更早，仍须遵守 B2-b 的空数据保护与成套恢复条件；非空审查账本不执行破坏性降级。

本批未提交、推送、安装依赖或操作真实应用。原工作区此前已有改动已保留；[本批完整 Diff](/private/tmp/codeagent-p0b2c-20261028/changes.diff)相对本批开始前副本生成，避免把以前阶段的未提交代码计入本批。

## 整体进度

| 阶段 | 当前边界 |
| --- | --- |
| P0-A / Manual | 原生 Session MCP 与双 GUI 真实协作已验证，继续保留 |
| P0-B1 | Task/Session/Participation/Attempt/Event 和 Go/Swift 契约已实现 |
| B2-a / B2-b | 精确准入、不可变产物、ReviewGate 和有限返工 DAG 已实现 |
| B2-c | 本批装配 API/MCP、保守恢复、故障注入与脚本模型闭环；当前测试结果见报告；尚未在用户桌面部署验收 |
| P0.5 | 下一步最小父 Task Timeline，尚未实现 |
| P1 | 外部 Agent Adapter/客户端生命周期托管、更多执行域后续扩展 |

后端受管链路不能替代 AgentKit 父任务产品层，也不能代表已具备任意中断自动继续的 Collaborative Mode。下一阶段优先 P0.5，继续保留手工和受管两种模式。
