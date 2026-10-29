# P0.5-B 首批交互：创建、Executor 关联与受管启动

2026-10-29。在 P0.5-A 父任务视图基础上增加最小交互。后端沿用既有 B1/B2 HTTP；本批没有修改生产 Go 代码、Agent Loop、模型配置或数据库。

后续可选执行模型补充见 [受管模型选择](p05b-executor-model-selection.md)。下文记录首批交互的原始范围；该补充新增 `executor_model` 和 capability，需要同步更新 Daemon/Bridge。

## 用户路径

1. 从“协作任务”点击“新建任务”，选择当前连接中已有的 Code-Agent Session，填写目标与逐行验收条件。
2. “创建并关联”先创建 Task，再将所选 Session 作为本地 Executor 关联。不会派发 Turn 或调用模型。计划和 reported Reviewer 仍通过 MCP 关联，不在 GUI 中伪造外部 Agent 身份。
3. Task 详情增加可复制的 Task ID。任务已有 reported 计划、本地 Executor、reported Reviewer，且后端发布 managed/review-gate 能力时，出现“启动受管任务”。
4. 启动表单选择具体计划版本与两个 Participation，明确工作区相对文件路径、验证命令和最大执行轮数（1–5，默认 2，含首轮）。点击确认后才 POST Run；提示模型 API、费用、文件修改以及 Reviewer 仍需回报。
5. 审查继续在 Reviewer 客户端进行。父页面展示等待提示和冻结 Diff；审批继续进入现有执行 Session。没有新增 Review PASS 按钮、代批工具、停止、resume 或 force-resume API。

创建可复用已有关联其他 Task 的 Session；不自动创建新的 Session，不修改 Session 模型。会话候选来自宿主已加载列表，并优先显示当前选中会话；首版未新增独立会话搜索/分页选择器。

## 端口与写入边界

- 独立可选 `CodingTaskWriting` port，通过 DefaultAgentClient → CodeAgentTransport → 同一个 RuntimeHTTPClient 转发。原只读和 RuntimeClient 必选协议保持兼容。
- 仅三个 POST：`/v1/tasks`、`/v1/tasks/{id}/participations`、`/v1/tasks/{id}/runs`。没有 external-agent registration、plan/review reporting、审批或恢复写入口。
- 在发命令前固定完整 POST URL、既有 credential target 解析出的请求和认证头；复制该请求向**同一 origin** GET capabilities 并复核 Server/Store 和操作能力。期间动态 endpoint 变化不会将命令改发另一 Runtime。TLS/禁止重定向/owner auth 沿用原机制。
- 每次启动前重读 Snapshot，核对 Task、计划、参与者、状态与文件/验证范围。已运行、终态、恢复冻结或候选失效任务不提供新启动入口；服务端准入及保护路径仍是最终权威。
- 请求类型没有 model、max_tokens、Thinking、任意 workflow/protected_paths 参数。

## 幂等与不确定结果

- 每个表单操作生成 request_id。显式重试保留第一次发送的完整 payload 和 ID，不因用户后来修改文字而悄悄变更。
- 创建回执已收到但 Executor 关联失败时，显示已创建 Task ID；重试只补关联，关联也使用固定 request_id。
- 启动回执不确定时，保留同一个请求；读操作、轮询、重连及重新打开 Task 不自动发起任何写入。只有保留原请求的显式重试可以再次提交，服务端仍负责去重及恢复边界。
- 本批请求草稿/回执状态仅在当前表单实例内存活，尚无跨 App 进程的客户端命令日志。发生网络错误后若关闭表单或退出 App，须先在 Task 列表/原任务核查后端事实，不宣称能恢复原未确认创建请求，也不自动重放。服务端已存在的 Task/Run/Event 不受影响。
- 本地校验失败不会冻结启动表单，可修改范围后重试；发送后表单锁定原参数。收到不同 Run 的冲突不会改成新请求 ID绕过服务端。

## 离线验证

- 新增 7 项命令状态测试：创建 ACK 丢失保持同一 payload/单次分配，关联失败不重建 Task，身份变化前置拒绝，启动 ACK 丢失保持同一请求且不夹带模型配置，无效范围不派发且可修改草稿，终态/缺能力不提供启动，以及已有 Run 原请求重试与 recovery_required 冻结边界。
- 新增 2 项 HTTP 写入测试：GET identity probe 与 POST 固定原 origin/Runtime 凭据；Store 不匹配或 managed 未 ready 时不发送 POST。
- 连同 P0.5-A 与会话/模型/连接回归，共 57 项 Swift 定向测试通过。未使用第三方模型。
- Talkify MacDirect 与 iOS Simulator 构建通过。Mac 最终增量构建曾被同时进行的 iOS 构建占用 build.db 阻挡，串行重跑通过。

## 真实桌面验收

用户重启后，通过实际 Talkify GUI 完成：

- 新建表单选择原 `GUI_SYNC_TURN_1` 隔离 Session，填写明确禁止执行的测试目标和 3 项验收条件，点击一次“创建并关联”。
- 创建 `ctask_1f1e3c90-b487-4c25-9afa-1fe5e6136208`，目标为 `P05B_GUI_METADATA_20261029 — GUI 创建与 Executor 关联验证；禁止启动执行`。详情自动回显 `active / planning / not_reviewed`、0 轮执行、已关联本地 Executor，以及 `task.created` 和 `participation.attached` 两条事件。
- 新 Task 与原 Task 的 workspace_ref 一致；缺少计划和 Reviewer 的新 Task 隐藏启动入口并提示补齐关联。返回列表后可重新进入，原 Task ID/2 条事件保持不变。
- 在旧 metadata-only Task 中只打开启动配置页：计划 v1、原 Executor、codex Reviewer、默认最多 2 轮显示正确；文件与验证命令为空时提交按钮禁用。随后直接关闭，没有填写范围、打开最终确认或提交 Run。
- 新 Task 的“打开执行会话”跳转回原 `GUI_SYNC_TURN_1`。实际历史仍为原 4 个 Turn，A_RESULT、B_RESULT 和取消原因均可见，没有新增执行。

本次仅保留一个可追踪的测试 Task；没有删除数据、启动 Run、派发 Turn 或调用模型。57 项离线测试与本次 metadata-only GUI 验收不能替代真实受管执行、Reviewer 回报、Finalize 的桌面全链路验收。iPhone/iPad 表单的实际界面与最终启动确认 dialog 也尚未通过本次 GUI 检查。

## 发布与回滚

需要更新 AgentKit 和重建 Talkify；无需升级数据库或重新部署 Go Daemon。仅在实际 owner 连接且能力发现成功后显示写入口。缺少写 port 或 capabilities 时继续只读。

回滚可移除三个表单入口与 CodingTaskWriting forwarding，恢复 P0.5-A 只读页面；保留已创建 Task/Participation/Run 和审查账本。不直接删除 SQLite 数据或回放未知副作用。本批未自动提交/推送，也没有修改既有其他未提交功能。
