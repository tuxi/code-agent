# P0.5 只读回放宿主实施与验收

2026-10-29。**P0.5-A 核心只读展示阶段性验收通过**：真实离线产物的列表、详情、两轮冻结内容、页面重开与生产/回放切换经用户确认；受控服务暂停造成 HTTP 超时后，用户确认侧栏自行恢复。Task 页超时提示的具体表现、整机网络中断和整 App 重启未单独验收。不以接口成功代替桌面截图或用户确认，不扩大为所有 P0.5 场景或真实模型受管协作通过。

## 最小实现

- `internal/taskreplay`：内存中加载单份完整离线导出；复用 collab 的 Snapshot/EventPage/ReviewBundle 和 server 的 Runtime DTO。没有 Repository、SQLite 连接、Flux service、Provider、执行器或恢复 API。
- `cmd/codeagent-task-replay`：仅绑定随机 IPv4 loopback 端口，256-bit 随机凭据，0700 临时目录、0600 连接文件，最长 12 小时、默认 1 小时，到期/正常退出撤销文件。启动输出仅地址、文件路径和到期时间，凭据不打印。
- HTTP 只开放 Runtime 预检和本 Task 的读取白名单。不发布创建、受管执行或审批能力。除 `/healthz` 的匿名 GET 仅返回 `ok` 外，所有数据读取需要独立 Bearer；匿名/错误凭据 401，已认证的非 GET/Upgrade 请求 403。未知读取路由 404，无 CORS、无写请求代理、无静态目录服务。
- 模型目录是合法的空目录；没有可选模型或外部请求。现有 AgentKit preflight 支持零模型，不必伪造一个可用 Provider。
- TaskSnapshot 原样提供验收结果，不依据 PASS 生成 Accepted；事件按十进制字符串 cursor 分页，持久序列不重编号；每个历史 bundle 按自己的 artifact id 读取。
- 身份保持离线原 Server/Store/Task/Attempt/Turn；Runtime 显示名明确为 `P0.5 Offline Replay (read-only)`。不冒充生产 Runtime，不覆盖 Embedded 连接。

## 原始产物 hash 补齐

旧导出经 MCP 解码为 DTO，内部 RawMessage 的对象字段顺序可能已变化，不能重新序列化并声称得到原 blob hash。新宿主首次尝试已拒绝该输入，没有跳过验证。

仅在现有离线导出测试中增加：只读打开该测试自身临时 sessions.db，按本 Task/artifact 精确读取原 blob 字节，校验原 artifact hash，再以 base64 `bundle_blobs` 导出。无生产数据库访问，无新增产品协议字段要求。已有 Go/Swift fixture 保持不变。

新宿主校验原始字节 hash、DTO 与原字节语义一致、Task/Run/Plan/Attempt/Turn/iteration/cursor/version/hash 绑定和历史 review 引用。缺少原字节、篡改内容、错轮绑定、跨 Task 事件或不完整事件页均拒绝启动。GET bundle 来自已校验的原字节，不读取当前 Git。

新产物：`docs/contracts/p05-replay-export.json`，由脚本模型真实两轮工具执行、MCP、SQLite、Flux、request_changes/pass/Finalize 生成。22 条事件、两份 bundle、Snapshot accepted。

文件 SHA256：`5a6dfb9b3925bc3e30efcd27e94e2903ee151e2a2982b8e17e9ba1e7e9a912cb`。这是导出文件指纹，各 bundle 仍使用各自原有 hash。

## 测试结果

- 重生成：`TestExportManagedTimelineFixture`，race，通过；原始字节在该测试中与原 hash 核对。
- 新宿主 5 个顶层测试及输入破坏子测试：真实两轮内容、历史不串位、Snapshot 不从 PASS 推导、完整分页与重复读取、预检、401/403、各种写入/执行/审批/恢复路由拒绝、Upgrade 拒绝、非法 cursor、越权 workspace、损坏原字节/DTO/绑定拒绝。
- 最终 `go test -race ./internal/taskreplay ./cmd/codeagent-task-replay ./internal/collab -count=1` 通过。CLI 无 Go 单元测试，实际构建并启动验证；`go vet ./internal/taskreplay ./cmd/codeagent-task-replay` 通过。
- 独立实际回放进程接受了 32 个本机 HTTP 检查：GET 预检/Accepted/两个 bundle，401、403、非法写请求和末尾空增量均符合预期。证据：`/private/tmp/codeagent-p05-replay-http-evidence.json`，无凭据。
- AgentKit 使用已有构建产物执行 CodingTaskTests/CodingTaskHTTPTests：15 项通过，无依赖安装或更新。包含 Store 身份/权限变化清空、迟到选择响应丢弃、网络失败保留 cursor/resync、重连去重、第一轮冻结版本不被第二轮覆盖，以及 HTTP 凭据 target/401/403/404。它不是本轮新 Swift 构建或真实桌面切服验收。
- `git diff --check` 通过。未提交代码。

中途最终回归曾被自动审批服务因额度无法完成审查而拒绝，并非测试失败；时间跨过其提示的重置窗口后，重新获准运行，上述最终测试、vet、构建已通过。没有改用其他方式绕过审批。

## 服务运行和连接步骤

构建及启动，不安装依赖、不修改生产 Daemon：

```sh
GOPROXY=off GOSUMDB=off go build -o /private/tmp/codeagent-task-replay ./cmd/codeagent-task-replay
/private/tmp/codeagent-task-replay --fixture "$PWD/docs/contracts/p05-replay-export.json" --lifetime 8h
```

终端保持运行；使用程序实际打印的地址和连接文件。文件是临时的，只对本次进程有效，不能写死为产品地址。不要把 `access_token` 发到聊天、日志或截图。

首个服务 17 时许启动、期限 2 小时；用户 22:39 查找凭据时服务已正常退出并清理了文件。已检查：临时目录仍在、凭据不在、进程退出码 0。因此不是路径别名问题；本次增加明确到期输出，保持退出清理机制。

重新启动的实例：`http://127.0.0.1:64495`，期限截至 **2026-10-30 06:40:26 +08:00**。实际已再次核对凭据文件 0600、Task GET 200 且 accepted。新连接文件：

`/var/folders/p2/h8cppqy94j5f2q9xq6v8z7300000gp/T/codeagent-offline-replay-655603428/connection.json`

在 Talkify 的 Runtime Servers 中新增独立连接，名称 `P0.5 Offline Replay`，使用本次 endpoint、Bearer 和该文件的 access_token；测试连接后，选择切换，再打开“协作任务”。不覆盖 Embedded、不复制生产 owner grant、不发送消息。测试服务到期后重新启动会生成新端口和凭据，不能继续使用旧配置。

## P0.5-A 展示验收边界冻结

以下项目通过即可阶段性验收 P0.5-A 展示，不以真实模型受管闭环为附加前提：

1. Talkify 显示真实离线 Task、Planner/Executor/Reviewer、2 轮执行与 1 次返工。
2. request_changes、PASS、ReviewGate/Finalize 分别呈现；Accepted 来自 Snapshot。
3. 第一轮与第二轮审查分别打开各自冻结 bundle、文件内容、Diff 和证据；历史第一轮不显示第二轮内容。
4. 切换生产/回放连接、关闭重开任务页、重建窗口及断线重连后，无跨 Store cursor/Task 混用、重复卡片或旧 PASS 覆盖新状态。
5. 权限失效明确展示，旧响应不污染新连接；服务端独立拒绝所有写请求，UI 不提供虚假的执行能力。

当前：后端/HTTP/客户端单测证据已齐。用户连接后最初反馈“没有任务/有错误提示”；随后提供页面原文：`offline managed task`、`managed_collaboration · completed · completed · accepted` 和对应 workspace_ref。其内容与 fixture 一致，是正确的任务列表而非错误文字，故**列表展示通过（用户确认）**。

用户随后针对详情和两轮冻结产物的核对明确回复：**“详情与两轮冻结内容均正确”**。核对项包含 accepted、2 轮/1 次返工、request_changes/pass 与系统里程碑，两轮 main.txt 候选分别为 `candidate turn_18e2f6aaecbf3d60_1` 和 `candidate turn_18e2f6ab413dbba0_2`。据此记录为用户桌面验收证据，不声称 assistant 已独立截图。

对“关闭重开协作任务 → 切回原 Daemon 核对原 Task → 切回 64495 回放 → 核对 accepted、两轮与历史内容”的步骤，用户明确回复：**“全部正确，没有重复或串数据”**。页面重建与跨 Runtime/Store 切换通过（用户确认）；没有要求重启 Talkify，因此不扩大为整 App 重启验收。

只读核对 Talkify secret-free Runtime 注册记录，当前 active connection 确实指向本次 64495 回放端点，显示名为用户填写的“codex测试”；没有读取 Keychain 或生产 token。不是只保存连接而没有激活。

为定位 GUI/HTTP 边界，在 AgentKit 新增 opt-in `CodingTaskReplayLiveTests.swift`：只接受显式环境变量提供的本机 fixture-only connection 文件，真实 DefaultAgentClient → HTTP → CodingTaskStore 全链路读取。重新构建并运行，1 项通过，读取 accepted、2 轮、22 条事件和两份原始产物；再次刷新不重复事件。Token 只在内存 credential store 中使用，不输出。此项是实际客户端网络集成测试，仍不替代窗口截图。

桌面工具多次超时，重建工具会话后仍超时；未绕过工具使用其他 UI 自动化。限定的 Talkify 错误日志查询也因 20 秒超时终止，未得到诊断证据。第一版不要求离线 Session 工具 Timeline 跳转。

### 受控不可用与恢复

用户明确准备观察后，只读核对 connection 文件的 endpoint、fixture_only、PID，并以 `ps` 核对该进程为当前 UID 的 `/private/tmp/codeagent-task-replay-20261029`；不以进程名模糊匹配，不触碰生产 Daemon。

对该测试进程发送 SIGSTOP，35.0 秒后在 finally 中 SIGCONT 恢复。暂停期间真实 HTTP 探测超时；恢复后 Snapshot 与暂停前完全相同，仍 accepted、cursor=22，after=22 的增量事件为 0。没有模型、Turn、审批或任务重放。原证据保存于 `/private/tmp/codeagent-p05-replay-outage-evidence.json`，仓库记录见 [恢复验收记录](investigations/p05-replay-outage-20261029.json)。

用户的实际观察原文：**“sidebar 中出现了【正在加载对话...】，然后恢复了”**。据此确认真实桌面的侧栏在服务恢复后自行恢复；不推断用户看到了 Task 页超时 banner，也不将服务暂停/HTTP 超时称为物理断网或 TCP reset 测试。

核心展示验收到此收敛：Accepted 和历史产物准确、两轮不串位、重开/切服不混用、只读由服务端强制、受控不可用后 HTTP 状态和侧栏恢复。完整网络故障矩阵、整 App 重启、权限错误的实际视觉提示可作为后续专项，不与真实模型执行质量混为一项。

## 返回生产与删除测试连接

1. 用 Talkify 现有 Runtime Servers 选择原 Embedded Runtime；这只是客户端切换，不关闭或取消原 Daemon 工作。
2. 确认原 Task 列表与原 Session 历史恢复后，删除名为 `P0.5 Offline Replay` 的非活动连接。现有连接管理会删除它自己的凭据；不触碰生产 token、Store 或历史。
3. 在启动回放服务的终端按 Ctrl-C，或等待明确期限到达。服务正常退出会删除本次 connection.json。不要杀同名生产 Daemon，不删除生产授权。
4. fixture/报告可保留以便重建。无需数据库回滚、清表、删除租约或恢复生产快照。

本批不调用第三方模型、不修改 Agent Loop/模型参数、不创建 macOS 用户、不解除 `recovery_required`、不修改 Talkify 生产权限或 UI。原 Store 的恢复问题与真实模型执行隔离仍为独立验收项。
