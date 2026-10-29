# Codex Desktop / Chater 双 GUI 接入方案

2026-10-27。本轮为源码评估、本机只读探测和离线协议验证；没有实施接入改造，没有配置 Codex MCP、导出令牌、启动第二个 Daemon、发送开发任务或调用模型。当前结论是**可以沿用现有架构接入，但真实双 GUI 链路尚未通过验收**。

## 本机证据与阻塞

| 项目 | 已核实的事实 | 判定 |
| --- | --- | --- |
| Desktop MCP 支持 | OpenAI 官方文档支持本地 STDIO MCP；同一 Codex host 的桌面、CLI、IDE 共享配置 | 产品支持；不是本机已调用的证据 |
| 两个 GUI | 本机 Codex 与 Chater（实际进程名 Talkify）均运行；通过原生 UI 读取到 Chater 会话列表、工具执行和 Diff 卡片 | 既有 GUI 可用；未验证来自本轮 MCP 的新会话 |
| Chater Daemon | 观察时 Talkify PID 4614，应用包内 codeagentd PID 4650；未另起 Runtime | 当前运行实例已定位 |
| 活动连接 | Chater 持久化的 active connection 为 `talkify-local-daemon`，local / bearer，地址 `https://127.0.0.1:52139`；该端口属于上述 Daemon | 保存的连接与进程匹配；尚未取得两端认证后的 server_id 对照 |
| TLS | 对上述 HTTPS 地址使用系统默认 TLS 校验，`GET /healthz` 返回 200 / `ok`；向相同端口发送 HTTP 返回 400，提示需要 HTTPS | 应保留现有 TLS，无需降级 |
| 认证 | 不携带令牌访问 HTTPS `/v1/runtime/info`、`/v1/runtime/capabilities`、`/v1/tasks/capabilities` 均为 401 | 认证生效；401 不证明后面的 B1 路由存在 |
| 当前 Bridge | `New` 只接受 HTTP，WebSocket URL 固定使用 `ws` | **直接阻塞连接现有 Chater HTTPS 实例** |
| Codex 配置 | 全局 MCP 配置未发现 codeagent server；本项目没有 `.codex/config.toml`；当前对话工具目录没有 `codeagent_*` | **真实 GUI 对话尚未加载 Bridge** |
| B1 部署 | 应用包 Daemon 文件中未检出 `/v1/tasks/capabilities`、`coding_tasks` 字面量 | 疑似仍为旧包；需要认证能力查询确认，不能仅凭源码认定运行实例已升级 |
| Task 工具 | Bridge 现有七个工具仅涵盖模型、Session、Turn、证据与取消 | Session 委派不会自动建立 B1 关联 |

地址和 PID 只是本次观察值，不得写成正式默认值。应用包文件 SHA-256 为 `a6c437299a9ec081865d844fece23ddc1dc5dd49d060e293b26849c282a3b525`，大小 25,695,616 字节；这是磁盘文件指纹，不是运行进程内存版本证明。

官方依据：[Model Context Protocol](https://developers.openai.com/codex/mcp)。配置位于 `~/.codex/config.toml` 或可信项目的 `.codex/config.toml`；STDIO 支持 command / args / env_vars / cwd，默认 startup timeout 10 秒、tool timeout 60 秒。文档支持不能代替实际桌面工具发现与调用验收，也不保证当前这次对话会自动热加载新增工具。

## 连接关系与认证

```text
Codex Desktop 的真实对话
  └─ STDIO → codeagent-mcp（轻量协议进程，不创建 Runtime）
                  ├─ HTTPS / WSS ─┐
Chater / AgentKit ─┴─ HTTPS / WSS ─┤ 同一个 Chater Code-Agent Daemon
                                 └─ Repository / Session / EventStore / Tools / Approval
```

`../chater/Talkify/CodeAgentDaemon.swift` 在启动时生成随机 owner token，以 `CODEAGENT_SERVER_ACCESS_TOKEN` 交给应用包内 Daemon，并通过临时 port-file 获得实际端口；该文件只记录端口。`AppContainer.startDaemon` 将连接登记为 local / bearer，并通过 `RuntimeServerCoordinator.injectBearerToken` 写入现有凭据存储。每次重启令牌会变化。

因此，源码中的 Chater 本地连接使用的是 owner Bearer，不是配对设备 credential；在包含 B1 的正确二进制上，这条身份路径满足 Task API 要求。但本轮未读取 Keychain 或进程环境中的令牌，也未完成 live owner 查询。若用户改选 remote / paired connection，必须重新判断身份，不能把 bearer 这个认证类型直接等同于 owner。

B1 通过 `WithServerAuth` 验证后才注入 owner context；shared-device 不能获得该 context，Task API 应保持 403。不得让请求体选择 actor，不得把匿名请求当 owner，不得为了联调放开 Task 路由。

## 第一步：先打通单 Session 双 GUI

最小改动建议如下；本轮只提出设计，没有修改这些文件。

| 位置 | 最小改动 | 保留边界 |
| --- | --- | --- |
| `internal/mcpbridge/client.go` | 接受数字 loopback 的 HTTP / HTTPS；HTTPS 对应 WSS；沿用正常证书及主机名校验、禁止重定向 | 不接受任意远程地址、不关闭 TLS 校验、不改变模型或工具权限 |
| `cmd/codeagent-mcp/main.go` 及本地启动器 | 从显式授权的连接交接中读取 endpoint / owner credential，核对实例身份，失败即退出 | 不启动、重启或探测猜测另一个 Daemon；令牌不进入 stdout、参数、提示词或配置正文 |
| Chater `CodeAgentDaemon` / `AppContainer` 的连接边界 | 提供明确的“连接本地 MCP 客户端”交接动作；复用当前活动本地连接和凭据，不另建连接系统 | 不更改 Agent Loop、默认模型、Thinking、审批策略或父任务 UI |
| 本机 Codex MCP 配置 | 注册绝对路径的 Bridge 启动器并实际确认工具发现 | 不以 CLI 实验驱动替代桌面对话 |

建议连接交接采用同用户私有目录中的临时 manifest（目录 0700，含令牌文件 0600，拒绝不符的所有者或符号链接）。manifest 包含 endpoint、预期 server_id、实例代次及可选 store_id；凭据只由本地启动器读取。启动器使用认证后的 `/v1/runtime/info` 校验 server_id；需要 Task 时再校验 `/v1/tasks/capabilities` 的 store_id。令牌轮换或实例失效后拒绝旧交接，不自动转移到新实例。Chater 持久化记录目前未填入 serverID，导出前应通过现有认证客户端取得真实身份。

这仍是授予现有 owner 权限，**不是** workspace 范围的委派凭据。Bridge 的单工作区限制不能当作 Daemon 级 ACL。第一次交接应是明确的本地用户动作；本轮没有导出或复制凭据。后续若要受限委派凭据，应另行设计，不能暗中放宽 B1 owner 要求。

本机 Python 默认 TLS 校验已成功；改造后的 Go HTTP / WSS 客户端仍需分别验证系统信任链。若部署环境不信任证书，报告信任错误并停止，通过明确配置的 CA / 证书信任解决，不添加 skip-verify 或 HTTP 回退。

配置形态示意（**启动器尚未实现，未写入本机配置，当前不可直接运行**）：

```toml
[mcp_servers.codeagent_gui]
command = "/ABS/PATH/codeagent-mcp-launcher"
args = ["--connection-manifest", "/PRIVATE/PATH/connection.json",
        "--workspace", "/ABS/PATH/isolated-clean-clone"]
startup_timeout_sec = 20
tool_timeout_sec = 60
enabled = false
```

启动器读取交接后调用现有 `codeagent-mcp --experimental-p0a --runtime-url ... --workspace ...`，仅向子进程传入 `CODEAGENT_BRIDGE_TOKEN`。GUI 通常不会获得另一应用或终端后来生成的环境变量；仅在配置中声明 `env_vars` 不能完成 Chater 随机令牌的交接。完成实现与配置检查后启用，并用桌面实际工具目录验证是否需刷新连接或开启新对话。

首轮使用一个干净的临时克隆，两个 GUI 指向同一个绝对路径。当前主工作区已有大量未提交内容，既不适合作为该实验工作区，也不满足 Bridge 的 clean 检查。首轮只使用 Daemon 自有的文件、Git 和命令工具；不要求依赖 Chater 尚未连接时才注册的客户端工具。

## Chater 列表、实时事件与审批

源码支持的路径是：MCP `create_session` 调用普通 `/v1/conversations`；Chater 通过相同 Repository 的普通列表发现该 Session，再由现有 `ConversationSupervisor` 和 Session-bound channel 连接。没有必要把外部 Codex 会话伪装成本地 Conversation，也没有必要把 TaskEvent 塞进单 Session reducer。

`WorkspaceStore` 仅在侧栏可见且 App 活跃时每 8 秒刷新列表；隐藏侧栏不能要求同样的发现时延，显示侧栏会触发刷新。Supervisor 根据 activity 为运行或待审批 Session 恢复通道，支持通过 detail 获取首屏列表以外的活动 Session。它们是源码路径证据，**本轮尚未用新委派 Session 验证实际刷新与回放**。

建议手工验收时先由 Codex 创建 Session，在 Chater 中确认同一 Session ID / workspace 并打开，再派发第一 Turn。两边都按精确 Turn 观察，不依赖“当前选中会话”或 last_turn。Chater 可以显示自己的真实执行过程；Codex 规划和审查保留在自己的对话中。

待审批时，Bridge 返回 `waiting_for_human`、`terminal=false`、`attention_required` 和 Session / Turn / Approval 引用，不能返回任务完成或代替用户批准。Chater 原有 Approval UI 通过该 Session 的 channel 回答；Server 将待审批请求广播给当前连接，并支持连接后的 pending 重发。用户处理后 Codex 对同一回执继续 wait/read；拒绝、过期、取消都必须按实际状态处理。Codex 自身的 MCP 调用授权与 Code-Agent 的工具审批是两层独立机制。

存在两个首轮限制：

- Bridge 的 Session 归属及 create 重试缓存只在当前进程内；进程重启后不能 attach 原 Session，工作区变脏后重新启动还会触发 clean 检查。本轮的“重连”应明确验收 Chater / WS 的重连回放，不能扩大为 Codex 或 Runtime 跨进程执行恢复。后续需要时单独设计显式 attach 与基线校验，不通过删除 clean 检查来假装恢复。
- 现有 artifacts 是当前工作区 Diff 与命令记录，不是不可变补丁。审查时要核对 Session / Turn、基线与当前 HEAD；若人工另发 Turn 或修改文件，重新取证，不能把旧审查当成当前代码的通过结论。人工作为审批参与者即可完成首轮验证，无需同时引入多人写入协调。

## 第二步：最薄的 B1 Task 关联

此步以同一个运行 Daemon 实际返回 B1 capabilities 为前提。若应用包仍为旧版，只在没有执行中 Turn、完成数据库备份和明确的版本切换安排后升级原应用包；不靠另起新版 Daemon 来展示“同一实例已接通”。本轮没有替换二进制或重启应用。

责任应由 **Codex 调用 Bridge 的显式元数据工具**承担，而不是要求 Chater 从会话内容猜出父 Task，也不在 Daemon 新增后台调度。现有 Session 工具继续可独立使用，Task 关联是可选行为。

最薄的工具面是现有 B1 HTTP 操作的一对一 typed facade：create_task、register_external_session、add_participation、link_attempt、link_artifact、report_plan、report_review、read_task、read_task_events，统一使用 `codeagent_` 前缀。注册、派发与关联均可观察；不把多个 HTTP 提交包装成不存在的原子事务。工具命名与输入需另加 MCP 契约测试，本轮未实现。

调用关系：

1. 认证查询 runtime 与 Task identity/capabilities；不支持 B1 时明确只进入 Session 模式。
2. 登记 Codex external SessionRef，标记 reported。同一个引用可分别关联 planner / reviewer。`native_session_id` 仅在确实已知时填写，它仍是声明，不证明原生 Codex 生命周期。
3. Codex 显式创建 Task、关联角色并回报计划。Code-Agent Session 的 executor 关系使用当前 server_id / store_id / session_id；workspace 必须匹配。
4. 原 `send_to_session` 得到 durable admission 的 `turn_id + request_id + cursor`；用这些精确值 link_attempt，不查询“最新 Turn”。若关联失败，展示未关联回执，只重试关联，不重复派发任务。沿用 B1 晚绑定补投影能力，无须 Outbox。
5. 读取真实 Diff、测试命令结果；artifact 引用必须指向该 Attempt 的精确 `tool_finished/source_seq`。当前 Git Diff 没有对应源工具事件时，不能伪造 source_seq，也不能把 Bridge 当前取到的文件内容称为不可变产物。
6. Codex 回报 review，绑定所审 artifact；同一 Session 下一 Turn 可作为新的返工 Attempt，仍由 Codex明确派发，没有自动循环。
7. 查询 Snapshot / TaskEvent。Task cursor 与 Session cursor 分开；核对 server/store，不能混用。B1 查询会按需补投影，不是 Task WS 推送。

Chater 第一轮不需要 Task DTO 或父任务页面；只显示真实 Session。若随后需要调试性对照，可在现有认证 HTTP 客户端上增加 Task capabilities / snapshot / events 读取与 fixture 解码，但应独立于 Session timeline。两端读取权威 Task 数据仍不代表拥有最终验收引擎：**B1 的 review=pass 只是 recorded，TaskAcceptance 仍为 not_reviewed**。最终审查结论先由 Codex 明确展示，不能把 Task 的阶段状态或 Turn completed 当成验收通过；自动最终验收规则留给后续范围。

## 桌面验收标准

以下均须保留真实调用回执与两端可见证据；本轮尚未完成这些端到端项目。

| 项目 | 通过条件 |
| --- | --- |
| 实际加载 | 在 Codex Desktop 的真实对话中发现并调用 `codeagent_*`；不是 `go test`、`codex exec` 或实验 harness 代调 |
| 同一 Runtime | Chater 与 Bridge 认证后 server_id 一致，B1 时 store_id 也一致；进程观测未出现为联调另起的 Daemon；失效凭据失败关闭 |
| 会话发现 | Codex 创建的普通 Session 在 Chater 活跃可见侧栏经一次轮询/刷新出现，ID 与 workspace 一致；隐藏侧栏恢复后可发现 |
| 双端执行 | 一次小任务的委派消息、工具调用、文件变化、测试结果可在 Chater 查看；Codex 获得相同精确 Turn 的回执与真实证据 |
| 断线回放 | 仅断开/重连 Chater 的 Session 通道，保持 Daemon 与 Bridge 进程；按 seq 补齐并去重，不额外派发 Turn，完成态不回退 |
| 人工审批 | 在临时克隆触发一项无破坏性的真实审批；Chater 用户决定，Codex 等待期间持续非终态，决定后同一 Turn 继续或准确结束；Bridge 没有发 approval_response |
| 审查与返工 | Codex 根据实际 Diff/测试审查并向同 Session 派发第二 Turn；Chater 可见审查消息与真实返工，两个 Turn 不串证据 |
| B1 关联 | Task/roles/精确 Attempt 可查询，晚绑定不重复里程碑，同 Turn 重复归属被拒绝；reported Codex 不获得 managed 控制权 |
| 状态语义 | 工具成功、测试通过、Turn 结束、review recorded、最终验收分别展示或说明；未知/未审查不显示为通过 |

首个真实模型任务仍需单独明确本次 Provider、必要数据外发范围和费用/调用预算；此前单次实验许可不能用于本次桌面任务。模型选择保持可选，默认继承 Runtime；不增加 max_tokens / Thinking 控制或实验代理。

## 本轮验证与实际改动

本轮仅新增本文档，没有修改 Go、Swift、AgentKit UI、Codex 配置、Daemon 配置或数据库，没有创建新 GUI Session。模型请求 0 次；没有第三方模型费用。原生 UI 读取的是既有会话，不作为新协作任务已成功的证据。

在剔除 API 凭据、禁用依赖下载的子进程环境中，重新执行 9 项 top-level 定向 race 测试，全部通过：

- Bridge 6 项：委派与证据、审批与取消、连接/工作区约束、generation controls 边界、attention 非终态引用、断连 attention/异 Session 控制过滤。命令 wall time 7.580 秒，Go 报告 3.879 秒。
- Server 3 项：Task owner/shared-device 与默认 Session 隔离、可选 Repository Port、非 health 路由认证。命令 wall time 2.905 秒，Go 报告 1.706 秒。

未运行 live 模型测试、另起 Daemon 的集成驱动、全仓测试或完整 Swift UI 编译。离线协议通过不计为双 GUI 验收通过。完整命令、返回码与日志位于 `/private/tmp/codeagent-dual-gui-assessment/results.json`、`bridge-contracts.log`、`task-auth.log`。

源码定位：`cmd/codeagent-mcp/main.go`、`internal/mcpbridge/{client,mcp,artifacts}.go`、`internal/server/{auth,tasks,ws}.go`；Chater 的 `Talkify/{CodeAgentDaemon,AppContainer}.swift`；AgentKit 的 `Core/RuntimeServers/{RuntimeServerCoordinator,RuntimeServerRegistry}.swift`、`Core/RuntimeSessionChannel.swift`、`Features/Workspace/WorkspaceStore.swift`、`Features/Conversation/ViewModels/{ConversationSupervisor,ConversationListViewModel}.swift`。

建议下一批只实施第一步的 HTTPS/WSS、连接交接与桌面配置验证；取得真实单 Session 双 GUI 证据后，再增加上述 B1 facade。保持推进顺序：B1 → 双 GUI 联调 → P0.5 最小 Task UI → B2 自动编排。
