# P0.5-A：只读协作任务工作区

2026-10-29。范围基于 `p05a-readonly-task-timeline-assessment.md`。本批实现共享 AgentKit 数据通道、父任务呈现和 Talkify 入口，没有新增受管调度或模型调用。

## 实现

- AgentKit `Core/CodingTasks/CodingTaskModels.swift`：独立 Codable/Sendable Task、Participation、Attempt、Artifact、Event、ReviewBundle DTO；开放状态和 JSON payload；十进制字符串游标保持精度。
- `CodingTaskReading` 是可选读取 port。DefaultAgentClient 通过 CodeAgentTransport 转发到原 RuntimeHTTPClient；不增加 RuntimeClient 必选方法，不读 MCP 授权文件。
- 五个 Task GET 使用现有动态 endpoint、credential target、TLS trust 和禁止重定向策略。响应在接收过程中限制到 24 MiB，后台解码。
- `CodingTaskStore` 独立于 Conversation reducer。首次读取 Snapshot 和从 0 开始的完整历史，之后按 cursor 增量读取。当前状态只接受不回退的 Snapshot；旧 review 不将 accepted/invalidation/recovery 状态覆盖。
- 同一视图只运行一个轮询任务，详情正常间隔 2 秒，错误退避至 32 秒；列表 10 秒。页面离开或 App 进入后台取消轮询。重入时重建历史，进程内保留增量状态；没有新建客户端数据库。
- 每轮读取前后复核 Server/Store；连接由现有 WorkspaceStore 生命周期隔离。Task 切换使用 generation 丢弃迟到响应。401/403/404 显示错误，不显示为成功空列表；403 清除读取内容。
- 父任务显示目标、验收条件、模式、状态、阶段、验收结果、参与者、精确 Attempt、执行证据、计划、review verdict/findings、Gate/Finalize 和异常原因。执行轮次按 executor 的真实 Turn Attempt 统计，包括 `reworking`。
- reported 外部参与者只展示其回报。ReviewGate/Finalize 显示为系统里程碑。`task_acceptance=accepted` 才显示最终验收通过；未知 usage/cost/approval 保留未知。
- 点击历史 Review 按该事件的 artifact/version/hash 找冻结 bundle，不使用当前 Git 或 current_bundle 替代。读取时核对 Server/Store/Task、Attempt、Turn、iteration、cursor、Plan、Review 的 Run/epoch 和版本绑定；原始 blob hash 由认证服务端验证，不重新编码 Swift DTO 冒充原字节校验。
- 冻结页面复用纯 Diff 渲染，按需展开 baseline/candidate 的 files/index 内容及 mode/exists。二进制和不存在的文件明确展示，不读取当前磁盘。
- 本地 Executor 跳转先读取精确 Session detail，保留归档、状态和工作区元数据，再走 WorkspaceStore 既有选择路径；不依赖 Session 是否位于会话列表第一页。

## 最小宿主接入

首版使用共享只读 sheet，避免侵入既有 Conversation/Workflow 导航和常驻 Timeline：

- Mac：Sidebar 新增“协作任务”，打开列表 → 父任务 → 冻结版本详情。
- iPhone：抽屉工作区顶部增加同一入口。
- iPad：工作区侧栏底部增加同一入口。
- Mac/iOS 使用同一组模型、投影、读取服务和主体视图。进入本地执行 Session 时关闭父任务 sheet。

列表明确标记“最近创建的任务，最多 200 条”，展示后端 workspace_ref；没有伪造分页或最近活动排序。当前外壳是首版 sheet，不声称已经完成独立三栏父任务导航。

## 验证

- 实际 Agent Runner + 本机脚本 Provider + SQLite + Flux + MCP 在临时工作区完成两轮 edit_file、git diff --check、request_changes → pass → Finalize，生成 `docs/contracts/p05a-managed-task-example.json`。没有第三方模型请求，没有向 Talkify 当前数据库写入模拟 accepted Task。
- Go 新增共享 fixture 检查，确认两轮 bundle、Review 的 Plan/Run/epoch/Attempt 绑定和 accepted 状态。现有真实进程 MCP 全链路/Bridge restart 回归通过。
- Swift 使用逐字相同的 manual/managed fixture；包括首次加载旧审查、重连增量、重复事件、大游标/合法空洞、未知事件、身份切换、403、迟到响应、断网、resync、取消、旧 PASS 与当前 invalidation/recovery、旧归档 Session 跳转以及第一轮冻结文件保持不变的测试。
- 定向 Swift 共 48 项通过，包含本批 13 项 Task 测试和 35 项既有 GUI/会话/连接回归。HTTP 使用 URLProtocol 和内存测试凭据，验证 GET、Runtime credential target、原样 decimal query 及 401/403/404 错误传播。
- Talkify MacDirect 和 iOS Simulator 构建通过。构建使用现有缓存及禁止自动解析/更新依赖选项；已有的 iOS 并发隔离和模型设置 warning 仍保留。
- `git diff --check`：三个仓库均通过。当前批次未提交或推送。

## 桌面只读界面验收

用户重启 Talkify 并告知已打开协作任务后，通过实际 UI 检查：

- Mac 侧栏“协作任务”可打开真实 Task 列表；“最近创建的任务，最多 200 条”范围提示存在。
- 打开既有 `ctask_a55bef04-850c-4d1f-aa32-2a25ae222e10`，目标、验收条件、`collaborative / waiting / planning / not_reviewed` 与 `execution_not_started` 正确显示；执行/返工为 0/0，费用及审批聚合为未知。
- Planner/Reviewer 复用同一 reported 外部 Agent，Executor 对应本地 Session；计划正文及原有 5 条 TaskEvent 展示完整。
- 点击“打开执行会话”，父任务 sheet 关闭，进入原 `GUI_SYNC_TURN_1`；实际会话历史包含 `GUI_DIALOGUE_20261028_A_RESULT`、`GUI_DIALOGUE_20261028_B_RESULT`、3 文件计数和 `已取消 — user_requested`。未发送消息。
- 重新打开协作任务并进入同一 Task，3 个角色、原计划和 5 条历史事件恢复，没有重复卡片。实际截图检查了摘要、参与者和会话按钮的布局。

这是真实 Daemon 的 metadata-only Task，不含受管执行、Review 或冻结 bundle，因此不把本次 GUI 结果扩大成受管两轮审查/历史 Diff 的桌面验收。后者目前通过的是离线真实链路 fixture 与投影测试。真实网络中断及 iPhone/iPad 实际界面仍待验证。

本轮两次 MCP 只读探测仍返回 `local MCP connection is closed; authorize a new connection`。Talkify 自身通过原认证客户端成功读取 Task；当前对话的旧 Bridge 连接需另行恢复，未绕过授权。全程没有派发 Turn 或第三方模型调用。

## 回滚

移除 Talkify 三处入口以及 AgentKit 独立 CodingTasks 模块/两处 HTTP forwarding 即可撤销本批功能。没有新增生产数据库迁移、Task mutation API 或缓存格式；无需回滚数据库、清除 Session、删除 TaskEvent 或撤销 ReviewBundle。Go fixture 和测试可独立保留。

AgentKit/Chater 在本批开始前已有模型展示等未提交修改，均保留，不作为本批新增内容。code-agent 的 OpenCode endpoint 改动已由另一个提交 `1027773` 落地，本批未修改其源码。
