# 双 GUI 接入第一批实现

2026-10-28。实现 HTTPS/WSS、Chater 显式连接授权、Bridge 身份核对及撤销。没有修改 Agent Loop、模型配置、审批规则或 B1 Task 模型，没有新增 Task MCP 工具或父任务 UI。

当前实测结论：**当前 ChatGPT/Codex Desktop 对话 → 原生 MCP 工具 → Chater 同一 Daemon → Talkify 侧栏与空会话打开已通过**。尚未执行真实 Turn，工具执行、实时事件、审批、返工及重连回放均未验收。模型请求 0 次。

## 已实现

- Bridge 接受数字 loopback 的 HTTP/HTTPS，HTTPS 对应 WSS；正常校验证书与主机名，禁止重定向，不回退到 HTTP。
- `--connection-manifest` 与 `--runtime-url / --token-env` 互斥。连接文件必须是当前用户所有、单硬链接的普通 0600 文件，直接父目录必须为当前用户所有的 0700 目录；拒绝符号链接、超大文件、未知字段、错误版本、过期授权及已停止的 Daemon。
- 读取授权后核对 `/v1/runtime/info` 的 server_id；包含 store_id 时还要核对 owner-only `/v1/tasks/capabilities`。不一致、认证失败或 TLS 失败均停止，不另起 Runtime。
- `--check-connection` 只打印 endpoint / server_id / store_id / identity_verified，不输出凭据，不创建 Session、查询模型或提交 Turn。
- Bridge 每次请求前与每秒检查授权文件。撤销、替换、过期或 Daemon 退出会关闭原 Bridge 的观察/控制连接，不跟随新授权，不取消已经准入的任务。新授权需要重连 MCP。
- Chater MacDirect 新增 **Code-Agent MCP → 授权本地 MCP 连接… / 撤销本地 MCP 连接** 菜单。仅当前活动连接为已就绪本地 Daemon 时可授权；在界面确认后使用原 Daemon owner token，12 小时有效。身份查询使用默认 TLS 信任、禁用代理及重定向；查询期间切换活动服务器或 Daemon 重启会拒绝交接。
- 文件位于 `~/Library/Application Support/Talkify/CodeAgentMCP/connection.json`；不把令牌放入配置、参数、日志、剪贴板或 MCP 输出。Daemon 正常退出/崩溃清理本实例文件；清理失败时 Bridge 仍会根据 PID/期限及实际认证失败关闭访问。这里不是工作区级委派凭据，也没有改变服务器认证。

连接文件只交给本机 Bridge，不能附加到对话、上传或加入版本控制。示例 fixture 的凭据是显式假值；真实文件不作为测试产物收集。

## 本机安装与启用

Bridge 已构建到 `build/codeagent-mcp`。为本项目新增 `.codex/config.toml`，仅包含二进制、连接文件和隔离克隆路径；最初为 `enabled=false`，用户在 Chater 授权后已启用。Codex CLI 的定向 `mcp get codeagent_gui --json` 已解析为 enabled / stdio / 20 秒启动超时 / 60 秒工具超时；没有更改全局 Codex 配置。这不是 GUI 工具加载的证明。

隔离仓库：`/private/tmp/codeagent-gui-workspace-__aaks0i/repo`，来自本地 Git clone，无远程访问、初始 clean。该仓库只含当前已提交历史，不包含主工作区未提交的修改。

首次检查时观察到 Chater PID 43455 / Daemon PID 43499，活动地址 `https://127.0.0.1:56270`；这些是后续重启前的历史值。应用包和 `../chater/build/codeagentd` 均已检出 B1 路由标记；两份文件哈希不同，不能仅凭文件路径声称逐字节一致。最终以授权后从实际运行实例返回的身份与能力为准。

用户已重启 Chater，并在新菜单中点击授权。最终实测时，Bridge 的 `--check-connection` 通过 Go 系统 TLS 校验与 owner API 查询，返回：

```json
{"endpoint":"https://127.0.0.1:57973","server_id":"srv_31ff12dc-6485-453d-a565-53da1fe86f03","store_id":"cstore_16a5c66010fa0840a975cb9ff13173e8","identity_verified":true}
```

这确认当前实例提供 B1 owner 能力；该地址也与 Talkify 保存的活动连接一致。当前对照结果在 `/private/tmp/codeagent-dual-gui-implementation/gui-session-runtime-check.json`，不含令牌或 authentication 字段。

重现与后续步骤：

1. 在没有需保留的执行中任务时重启构建后的 Chater。
2. 从新菜单明确授权本地 MCP 连接。授权是 owner 级别，范围大于一个工作区。
3. 用 `build/codeagent-mcp --experimental-p0a --connection-manifest <私有文件路径> --workspace <隔离仓库> --check-connection` 验证实际身份。
4. 启用项目 MCP 配置，在 Codex Desktop 刷新 MCP；必要时仅重启 ChatGPT，再确认七个 `codeagent_*` 工具实际出现。此时保持 Talkify 运行。
5. 仅创建空 Session 并在 Chater 确认相同 Session ID / workspace。此步无需模型调用；真实执行、审批、返工仍需另行明确本次模型与数据外发授权。

启动顺序已经由实际失败和成功记录确认：Talkify 重启会清理授权文件，Bridge 在没有授权文件时先退出，桌面日志出现 `initialize response / connection closed`。先在 Talkify 重新授权，再仅重启 ChatGPT 后，当前项目对话成功加载七个 MCP 工具。此前曾怀疑配置作用域，后续已证明**项目级配置足够，不需要全局注册**。

曾准备将同一配置迁移到全局注册，自动批准审查以“扩大所有项目及未来会话的 owner 访问范围、超出此前本次联调授权”为由拒绝，命令没有执行。用户明确选择**保持项目范围**；全局配置未修改。桌面自动化此前也拒绝访问 Codex App，未绕过限制；真正的调用证据来自本对话已经加载的 MCP 工具。STDIO 和可信项目配置依据 [OpenAI 官方 MCP 文档](https://developers.openai.com/codex/mcp)。

## 真实桌面空 Session 实测

当前对话调用 `codeagent_create_session`，`request_id=gui-empty-session-20261028-01`，获得：

```json
{"id":"20261027-165016-b3490cad","workspace_path":"/private/tmp/codeagent-gui-workspace-__aaks0i/repo"}
```

这是桌面对话的原生 MCP 调用，不是通过 CLI 驱动或脚本模拟的调用。用户最初未在侧栏找到该会话；桌面工具恢复后，直接检查 Talkify，发现它位于折叠的 **repo** 项目分组内。展开后可见与回执完全相同的 Session ID，点击后窗口标题为该 ID，输入框为空，上下文使用率 0.0%。截图与 AX 观察已在对话工具记录中展示。

| 验证项 | 结果 |
| --- | --- |
| 当前对话加载七个真实 MCP 工具 | 通过 |
| 原生 MCP 创建 Session | 通过，回执已留存 |
| Bridge 与 Talkify 使用同一个活动 endpoint | 通过，身份校验包含真实 server_id/store_id |
| Talkify 发现并打开空 Session | 通过，agent 直接观察，不仅依赖用户回报 |
| Turn、工具执行、测试、审批与返工 | 未运行、未验收 |
| 断线增量回放 | 未验收 |

可见性问题是分组折叠与临时克隆目录名 `repo` 缺乏辨识度，本次不需要修改列表或 Timeline。后续可为协作会话增加显示名称，但不作为本次链路通过的条件。本轮没有发送消息、派发 Turn、修改隔离工作区文件或调用模型；仅更新验证文档和非秘密回执。

非秘密证据：`/private/tmp/codeagent-dual-gui-implementation/gui-empty-session-receipt.json` 与 `gui-session-runtime-check.json`。一次连接对照脚本因可能输出 authentication 字段被自动批准审查拒绝；改为仅比较 endpoint 和公开身份后通过，无需追加凭据权限。

## 验证

- `go test -race -count=1 -timeout=60s ./internal/mcpbridge -skip 'Test(LiveDaemonRoundTrip|DaemonStdioToolRoundTrip)$'` 通过。覆盖原有契约，以及 HTTPS/WSS、未信任证书拒绝、Runtime/Store 身份错误、授权撤销/替换/期限、权限和链接拒绝、错误不泄漏文件内容。排除两个需要单独真实/独立 Runtime 的驱动；没有运行真实模型。
- `go vet ./internal/mcpbridge ./cmd/codeagent-mcp`、Bridge 编译通过。
- 同一 [JSON fixture](contracts/local-mcp-connection-example.json) 由 Go 与 [Swift 协议检查](../tests/protocols/LocalMCPGrant.swift) 解码；Swift 实际验证私有写入、权限、拒绝异实例撤销、公有文件及符号链接。
- Chater `Talkify-MacDirect / Debug-MacDirect` 离线构建通过；第一次构建发现新菜单缺少 AgentKit import，修正后第二次成功。随后按原有签名配置再次构建成功；用户已重启并确认新菜单。`codesign --verify --deep --strict` 报本机证书链 `CSSMERR_TP_NOT_TRUSTED`，没有修改系统信任或绕过警告，不能宣称分发签名验证通过。
- 对实际 Bridge 完成 STDIO initialize / tools/list，协议 `2025-11-25`、七个工具均可列出，未暴露 generation controls。此项是直接协议就绪检查，不计为 Codex Desktop 对话调用验收。没有创建 Session 或提交 Turn，结果存于 `stdio-readiness.json`。

日志位于 `/private/tmp/codeagent-dual-gui-implementation/`：`go-results.json`、`bridge-race.log`、`vet.log`、`bridge-build.log`、`chater-build.log`（首次）、`chater-build-2.log`（修正后）、`chater-build-signed.log`。Go 测试/构建子进程剔除 API 凭据，并关闭依赖下载。模型请求 0 次，模型费用 0。

## 改动与回退

Code-Agent：`internal/mcpbridge/client.go`、新增 connection 校验及测试、`cmd/codeagent-mcp/main.go`、共享 fixture / Swift 检查、本说明及本机项目配置。

Chater：新增 `LocalMCPConnectionGrant.swift`、`LocalMCPConnectionCommands.swift`；`CodeAgentDaemon.swift` 增加显式导出/撤销和生命周期清理；`TalkifyApp.swift` 增加 MacDirect 菜单。构建自动抽取三个新增菜单字符串，共 9 行本地化条目；原有本地化 9 行修改与工程配置 4 行修改保留，未覆盖。

回退先禁用本项目 MCP，再从 Chater 撤销授权；正在执行的任务需通过原有会话控制处理。恢复本批修改前的两个 Chater Swift 文件并移除两个新增文件，即可去掉菜单；既有 Session/Task 数据无需迁移或删除。本批不修改数据库结构。不要用 reset/clean 覆盖原有未提交工作。

仍保留 P0-A 限制：Bridge 重启不能 attach 原 Session，修改后的工作区不能直接重新通过 clean 启动检查；当前 Diff 不是不可变产物；待审批是非终态；单次工具/Turn 完成不代表 Task 验收通过。当前仅空 Session 联动通过，不以此代替真实执行与完整协作闭环验收。
