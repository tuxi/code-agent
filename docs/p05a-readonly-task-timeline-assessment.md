# P0.5-A：最小只读父 Task Timeline 实施评估

2026-10-29。依据当前 code-agent、`../AgentKit`、`../chater` 源码及真实桌面元数据冒烟。本文是可实施设计，**没有实施 Swift/UI 改动**，不代表已有父 Task 页面或 Mac/iOS 界面测试通过。

## 判断与边界

现有后端足以实现首版只读父任务总览，无须再引入调度服务、Task WebSocket、ReviewGate 或 Agent Loop 改动。先完成数据接入与投影，再接最小 UI。P0.5-B 的创建、审批聚合、启动/停止和恢复控制继续排除。

父任务展示目标/验收条件、Manual 或 Managed 模式、状态/阶段/验收结论、角色、各轮执行、计划、审查、冻结产物和系统里程碑。ReviewGate/Finalize 是系统动作，不增加 Agent Participation。reported 外部 Agent 只展示其已回报内容，不伪造模型思考、工具调用或原生客户端生命周期。

`task_acceptance=accepted` 才显示最终验收通过。Turn completed、验证命令 exit 0、review pass/recorded、Flux success 不能单独升级成 Task accepted。`recovery_required`、candidate_invalidated、needs_validation 展示原原因与证据，没有“强制继续”或“恢复执行”按钮。`pending_attention/usage/cost_usd=null` 表示未知，不展示为零或“无审批”。

## 已核实的代码落点

| 当前实现 | 对 P0.5-A 的影响 |
| --- | --- |
| `Core/RuntimeHTTPClient.swift` 统一 envelope、auth、trust、禁止重定向 | 在原客户端增加 GET 读取方法，保留同一认证边界；不自行读取 MCP manifest 或导出凭据 |
| `Core/AgentClientImpl.swift` 的 DefaultAgentClient | 可转发新的可选只读 Task port；复用当前 client，保持动态 endpoint 和 credential target |
| `Core/RuntimeServers/RuntimeServerCoordinator.swift` | client 已按连接注入凭据；切换服务器时更换 Task 数据域，不能只按 task_id 缓存 |
| `Core/RuntimeSessionChannel.swift` / ConversationSupervisor | 只在用户点击本地 Executor 时进入既有 Session 路径；不把通道事件塞进 Task 投影 |
| `Features/Workspace/WorkspaceStore.swift` | 增加独立 Task 选中态/Store，保留已有 Conversation resident timeline 和审批通道 |
| `Navigation/AgentNavigationDestination.swift` / AgentRouterViewModifiers | 新增独立 Task dashboard/detail 导航，不复用 `.workflow` 的 Flux 数字 Task ID |
| Chater `TalkifyMac/WorkspaceView.swift` / SidebarView | Mac 自己的根布局也有 exhaustive switch，必须同步接入口；只改 AgentKit 枚举不足以完成 Mac 接入 |
| `Features/Inspector/DiffInspectorView.swift` | 纯 payload Diff 可复用渲染；当前 DiffDetailView 的文件 Provider 需要改用冻结内容来源 |
| `Core/Workflow/WorkflowStore.swift` | 它处理 Session 的 workflow_* AgentEvent，不是 CodingTask；不把现有 Workflow 控制面板当父任务页面 |

检索未发现已有 CodingTask/TaskSnapshot/ReviewBundle Swift DTO 或 Task HTTP 方法。已有 Swift 中 `task` child-stream 是子 Agent 工具事件，不是 B1/B2 的 CodingTask。Mac 与 iOS 可以共用 DTO、读取服务、投影与主体 SwiftUI View；导航适配由各自宿主完成。

## 五个只读接口与权限

建议增加可选 `CodingTaskReading` port，由 DefaultAgentClient 实现，第三方 RuntimeClient/mock 不必实现新必选方法。接口仅包含：

1. `GET /v1/tasks/capabilities`
2. `GET /v1/tasks?limit=...&workspace_ref=...`
3. `GET /v1/tasks/{id}`
4. `GET /v1/tasks/{id}/events?after=...&limit=...`
5. `GET /v1/tasks/{id}/immutable-artifacts/{artifact}`

Task API 仍是 owner-only，不能因为 App 保存了 bearer 就推断它是 owner。先查能力：401 提示认证，403 提示当前连接无权查看协作任务，404/unsupported 显示 Runtime 暂不支持；不把错误渲染为空列表，不在 App 中放宽服务端权限。

iOS embedded runtime 的二进制版本和 Mac Daemon 不保证同步。本批不升级 XCFramework；同一套 UI 在缺少能力时降级为不可用说明。连接支持 B1 时可显示普通 Task，缺少 managed capability 时不假装可以读取不可变 bundle 或受管状态。Mac/iOS 访问远端 paired/shared-device 时也必须尊重 403，读权限扩展另行设计。

## 两项必须正确处理的数据约束

**Snapshot 不是完整审查历史。** 它包含当前 Task、Participation、Attempt、artifact refs、当前 ManagedRun，但不包含历次 GateDecision/Review findings 列表。后者目前在 `review.recorded`、`review.gate_consumed` 等 TaskEvent payload 中。

因此不能首次取 Snapshot 后立刻从它的水位开始读新事件，否则旧轮次审查与返工会消失。也不能把旧事件回放到当前 Snapshot 上让最终状态回退。应维护两份投影：当前权威 Snapshot，以及由完整 TaskEvent 历史构建的时间线索引。

**列表是有界的最近创建列表。** 现有 `/tasks` 最大 200，没有分页游标，也没有列表 updated_at。首版可以明确显示“最近创建的任务，最多 200 条”、服务端顺序和手动刷新，不能称为全部任务或最近活动排序。`workspace_ref` 是后端稳定标识，不能拿目录 basename 或原始路径直接代替。首版先获取有界列表，再按真实 WorkspaceRef 分组；若以后要求跨全部工作区完整枚举，再单独扩充列表协议。

## 投影与重连算法

身份键为 `(connection/auth context, server_id, store_id, task_id)`。Task、Session、Flux 三种 cursor/ID 彼此独立；TaskSeq 是 Store 全局序列，某个 Task 的 seq 跳跃合法，不可要求 seq 每次加一。TaskRevision 用于 Task 内一致性判断。所有 decimal string 保留精度，禁止先转 Double/JavaScript Number。

首次打开：

1. 校验能力与 server/store，取 Snapshot，记下水位 W 和 revision，立即显示权威摘要。
2. 没有可信本地历史缓存时，从 `after=0` 分页读取 TaskEvent。建立历史卡片索引，不用旧事件覆盖当前摘要。
3. 以 event_id 去重并按 decimal task_seq 排序；记录 applied cursor。补齐历史前标明“历史加载中”，不能将缺少 review 显示为从未审查。
4. 若读取期间已有超过 W 的事件，补取最新 Snapshot；只接受相同身份且 revision 不回退的结果。异步请求带连接代次/页面代次，旧服务器迟到响应直接丢弃。
5. 进入增量轮询，活动页面建议 2 秒，有错逐步退避；App 后台/页面退出停止轮询。没有 Task WS，不能声称通过已有 Session socket 收到了 TaskEvent。

重新进入：加载相同身份下的 Snapshot + 历史缓存，拉增量并去重，再核对最新 Snapshot。只存 cursor 而没存对应事件/投影会丢历史，不能跳过重放。`resync_required`、cursor 超前/非法或身份变化时，重新获取 Snapshot 并重建历史；不重发任何创建、派发或审查命令。未知事件保存通用卡片和原 kind，不因 enum 解码失败丢掉整页。

持久历史缓存不是第一版前提：可以每次进入从 0 重建任务里程碑，在进程内保留增量 Store。未来落盘必须按连接身份隔离并采用已有隐私存储策略；这与后端持久事件账本不同，不另建任务事实源。

## 卡片语义

| 事实来源 | 显示 |
| --- | --- |
| reported plan artifact + planner Participation | Planner 的计划和验收条件，来源标明 reported |
| 精确 Session Attempt | Executor、iteration、Turn 状态、执行证据摘要；有真实 local SessionRef 才显示“打开会话” |
| review.recorded | Reviewer 的 verdict/findings、对应 Attempt 及 artifact/version/hash；`recorded` 不是最终通过 |
| review.gate_consumed | 系统 ReviewGate：pass / repair / attention 与原因；即使 event 带 reviewer participation_id，也不显示为 Reviewer 发起的新工具调用 |
| 下一轮 Attempt | 只有实际 reservation/admission/Attempt 存在才显示正在执行的返工；可说明已授权返工，但不捏造已开始的第二轮 |
| task.accepted | 系统 Finalize：最终匹配的冻结产物；用 Snapshot acceptance 再核对 |
| review.invalidated / run.recovery_fenced / run.attention_required | 系统异常/人工处理卡片；保留历史 PASS，但不再显示当前候选已验收 |

“执行轮数”只统计执行 Attempt，reported planning/review Attempt 不计入；“返工次数”来自实际第二轮及以后执行，不能用全部 attempts.count - 1。角色属于 Participation，同一 Agent 可担任多个角色，不固化 Codex=Planner/Reviewer。

现有 Snapshot.available_actions 主要是 B1 提示，可能仍出现 submit_plan，不能将其自动映射成 P0.5-A 的控制按钮。首版只读入口限导航、查看计划/审查/冻结文件和刷新；审批仍由原 Session UI 处理。

## 不可变 Review Inspector

用户点某次 Reviewer 时，从该 review 的 artifact binding 读取对应历史 bundle，不使用 `ManagedRun.current_bundle` 替代旧轮次，更不能重新查询当前 Git Diff。

缓存键至少包含 server/store/task/artifact_id/version/hash。复核响应的 Run/epoch/Attempt/Turn/Plan 与选中引用；已认证服务端负责验证保存的 blob hash。不要将 Swift DTO 用 JSONEncoder 重新编码后计算 hash 冒充 Go 原字节 hash，编码顺序/空值/RawMessage 格式并不等价。

文本 Diff 使用 bundle.patch/index_patch；文件内容只读 bundle.baseline/candidate 的固定 bytes/mode/exists。复用 DiffArtifactBody 等纯渲染组件，必要时新增 FrozenBundleContentProvider；不能使用指向当前工作区的 FileContentProvider。文件路径当作数据展示，不自动执行、打开任意外部 URL 或回退读取磁盘。二进制/删除文件显示相应事实，超限/网络失败保留错误，不用实时内容填补。

完整 bundle 响应有界读取、后台解码，按需展示文件，避免主线程一次展开全部 bytes。B1 的 mutable/reported artifact 另标明来源，不能包装成“冻结 Diff”。先支持查看历史版本；apply/restore/check-out、审批、提交 Review 均不属于 A。

## 最小改动范围

| 位置 | 拟议改动 |
| --- | --- |
| AgentKit `Core/CodingTasks/` 新模块 | Codable/Sendable DTO、开放状态/JSON payload、decimal cursor、可选读取 port、独立 Store 和纯历史投影 |
| RuntimeHTTPClient / AgentClientImpl | 五个 GET 方法及原客户端 forwarding，复用当前 auth/trust/dynamic endpoint |
| AgentKit `Features/CodingTasks/` | 任务列表、父 Timeline、计划/审查详情、冻结 ReviewBundle 内容查看 |
| AgentNavigationDestination / AgentRouterViewModifiers / WorkspaceStore | 独立任务导航及状态，local Executor 跳转现有会话；不改 Conversation Reducer |
| Chater Mac WorkspaceView / SidebarView 与 iOS 路由入口 | Mac 三栏与 iOS NavigationStack/regular 布局适配，共用数据和主体视图 |
| AgentKitTests + 共用 fixtures | 协议、身份隔离、历史重建/去重/重连、系统里程碑、冻结内容与 UI 状态回归 |

建议在现有工作区增加“协作任务”只读入口，与普通 Workflows 并列，而不是将父 Task 塞进现有单 Agent TurnCard。Mac 用列表—Timeline—Inspector；iPhone 用列表→详情→产物，iPad regular 复用分栏。保持现有普通会话默认入口和草稿行为。

先实施 DTO/读取/投影并通过 fixture，再接列表和父 Timeline，最后接冻结 Inspector 与会话跳转。当前评估没有修改这些文件；具体 UI 实施仍是下一批。

## 验收矩阵与回滚

需要在实施时补齐以下实际证据，不能用本轮冒烟替代：

- 同一份 Go/Swift JSON 覆盖 B1 manual、B2 两轮 repair→pass→accepted、candidate_invalidated、recovery_required、unknown/null/大 cursor；managed fixture 从离线脚本模型链路导出，不向真实 Daemon 写伪造 accepted 任务。
- 首次打开已有 accepted Task 能显示旧 Review/findings，重连重排/重复页不重复卡片，旧事件不让当前 acceptance 回退；切换 Store 或 auth 后旧请求不能污染页面。
- Session 跨 Task 复用但 Task/iteration/Turn 不串；同一外部 handle 的多角色正确；report 不伪装 native 工具轨迹；系统卡片不伪装新 Agent。
- 第一轮 Reviewer 的冻结 Diff 在第二轮修改后仍保持第一轮 bytes；错误来源/hash 绑定、404、无权读取、二进制/删除文件和超限均能明确显示。
- App 真正只发送五种 GET；查看/刷新/重连不发 send、start_run、review、approval 或 resume；没有未知恢复按钮。
- macOS 与 iOS 各完成构建及实际导航/断网/重建回放，普通 Session、审批与模型选择回归不受影响。缺少能力和 403 是明确不可用态，不伪装空任务列表。

回滚只读 UI 时关闭入口并移除独立模块/适配，不回滚服务端数据库或删除 TaskEvent/ReviewBundle。Task 本地缓存可独立失效后从权威 API 重建。现有 Session 页面、MCP Manual/Managed 和后端审查账本继续保留。

本轮 [Desktop Smoke Test](p0b2c-desktop-smoke.md) 已提供同一生产 Daemon 的元数据与重连证据；P0.5-A/P0.5-B/P1 仍分别是只读呈现、受支持交互、外部 Runtime 生命周期托管三个后续阶段。
