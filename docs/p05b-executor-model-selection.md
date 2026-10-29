# P0.5-B 受管 Run 可选执行模型

2026-10-29。实现范围：模型选择、持久化、HTTP/MCP/AgentKit 表单与离线验证，不是新的真实模型实验。

## 契约

- `StartRunInput.executor_model` 为可选命名空间模型 ID，来源是既有 Runtime Model Catalog。例如 `opencode-go/qwen3.8-flash`；不新增 Provider 白名单。
- 省略/空字符串保留原有默认模型行为。AgentKit 默认项省略该字段；不更改 Session 或全局默认模型。
- 显式模型保存到 ManagedRun、提交给 Flux 的原始 payload 及持久化 dispatch；所有返工沿用。它固定的是请求模型 ID，不冻结凭据、生成参数或 Provider 配置；每个 Turn 原有 requested/resolved/actual/configuration_id 审计仍为实际调用证据。
- Runtime 使用现有解析器与非联网可用性探测检查身份和凭据。失败返回 HTTP 409 / `executor_model_unavailable`，不自动换模型。调用成功与实时在线不在该检查的保证范围内。
- 启动前拒绝不满足条件的选择；派发每轮前重新校验。若模型随后失效，任务进入 waiting / `executor_model_unavailable`，不新增执行 Turn，不自动恢复或切换默认模型。
- `max_tokens`、Thinking、温度及 Agent Loop 配置继续不暴露。调用方不接管 Runtime 的生成策略。

## 能力发现与客户端

- 仅在 managed service ready 且装配模型校验器时发布 `task_executor_model_v1`。旧装配仍支持省略模型；显式模型请求在 HTTP/MCP 入口被拒绝。
- MCP `codeagent_start_task_run.run.executor_model` 为可选参数，复用原 Session/Task 范围与 owner 权限检查。
- AgentKit 独立可选 `CodingTaskModelReading` 读取同一 HTTP 模型目录。表单在身份前后核对且具备该能力时展示模型选择；只列有 canonical ID 且 available 的模型。旧 Runtime/不支持目录的客户端仍只继承默认。
- POST 固定原 origin、认证头和 Server/Store，并额外核对模型选择能力。Task 详情展示 Run 执行模型配置，不把它当作响应 actual_model。
- 幂等请求包含模型选择；同一 request_id 更改模型冲突。原请求重试读取持久回执，不因当前模型失效而重新校验已准入 Run；客户端保留完整原参数。

## 存储兼容及回滚

无新增 SQLite 表、列或版本迁移。字段加入现有 JSON 文档，omitempty 保持默认模式的原幂等 payload/hash；历史记录缺字段等价于未指定。Flux 定义和版本 hash 保持不变。

发布需要重建 codeagentd、codeagent-mcp 和 AgentKit/Talkify。未自动重启正在运行的 App/Daemon。升级前沿用 Session/Flux 数据库备份流程。

回滚时先停止受管准入并确认/隔离未完成的显式模型 Run；旧代码会忽略新 JSON 字段，不能让它自动接管尚未结束的显式模型 Run，否则可能使用默认模型。保留新版本处理这些 Run，或在停机时恢复匹配的升级前 Session/Flux 备份；不删记录、不篡改租约、不重放未知副作用。

## 验证范围

共享 Go/Swift fixture：`docs/contracts/task-executor-model.json` 与 AgentKit `Fixtures/coding-task/executor-model.json`，仅协议示例，不执行任务。

回归涵盖：默认 payload 兼容、显式模型持久化/重开、同请求变更模型冲突、dispatch 强制绑定、两轮审查返工并 Finalize、返工前模型失效不回退、准入前拒绝、原回执重试、HTTP 能力与错误、MCP 可选 schema、Swift capability/retry/目录兼容。具体执行结果在本批报告记录。

本批实际结果：

- `go test -race ./internal/collab ./internal/collabflux ./internal/session/sqlite ./internal/mcpbridge -count=1` 全部通过（Flux 含原有独立进程故障注入回归）。
- Runtime/Server/SQLite/Flux/MCP 新增模型选择定向测试通过；共享 fixture 的 Go 测试另行通过，两个 JSON 文件逐字一致。
- `go build ./...` 与上述改动包及 `cmd/codeagentd` 的 `go vet` 通过；项目配置引用的 `build/codeagent-mcp` 已重新构建。
- Swift `CodingTask|GUIStabilityTests|ConversationLocalStateTests|RuntimeServerConnectionsTests` 共 63 项通过。
- Talkify-MacDirect / Debug-MacDirect arm64 与 Talkify / Debug iOS Simulator 按顺序构建成功。保留既有编译警告，未安装依赖。
- 新增模型失效测试曾发现：下一轮派发前停止时没有新 Attempt，原 attention 节点的 iteration 校验误拒绝。已修正生产分支承认现有 waiting/recovery fence，不创建新 Attempt；重新测试和 race 回归通过。

本机日志：`/private/tmp/codeagent-executor-model-race.log`、`/private/tmp/codeagent-executor-model-test-final.log`、`/private/tmp/agentkit-executor-model-regression.log`、`/private/tmp/talkify-executor-model-mac-build.log`、`/private/tmp/talkify-executor-model-ios-build.log`。

此前真实双 GUI 手工协作和 P0.5 metadata 验收继续有效。真实受管模型任务验收仍待进行，无第三方 API 调用，无费用；未改变已有两个禁止执行的 GUI 测试 Task。

## 用户重启后的桌面核对

2026-10-29，用户重启新版 Talkify、恢复原 MCP 授权并重载连接后，实际验证：

- 运行包为 DerivedData/Debug-MacDirect 下的新版 Talkify，内置 codeagentd 随同运行。项目 Bridge `--check-connection` 的 TLS 与授权身份校验通过：endpoint `https://127.0.0.1:55288`，原 Server `srv_31ff12dc-6485-453d-a565-53da1fe86f03`、Store `cstore_16a5c66010fa0840a975cb9ff13173e8` 与 Session `20261027-165016-b3490cad` 保持一致。
- 真实 Talkify 打开既有 metadata-only Task 的启动表单：新增“执行模型”选择器可见，默认选项是“继承 Runtime 默认模型”，目录当前默认显示 `openrouter/openrouter/free`。
- 下拉目录有 `opencode-go/qwen3.8-flash`；仅在未提交的表单内选中，标签立即更新。文件范围和验证命令保持空白，“检查并启动受管任务…”保持禁用。关闭表单，没有确认或提交 Run。
- 通过项目配置的临时 stdio MCP 进程仅读取工具列表、模型目录和原 Task：Run schema 的 `executor_model` 存在且非必填；所选模型 configured/available=true、verified=false。该结果验证目录与配置，不保证第三方 API 实际请求成功。
- 原 Task `ctask_a55bef04-850c-4d1f-aa32-2a25ae222e10` 保持 waiting/planning/not_reviewed、`snapshot_task_seq=5`、`managed_run=null`，GUI 仍显示 0 轮执行。
- 当前对话工具清单仍没有原生 `codeagent_gui` 工具。本次 stdio 只读检查不能宣称当前对话已恢复原生 MCP 工具加载；该集成项须与 GUI 表单验收分开记录。

结论：模型选择表单和已部署 Bridge 的只读检查通过；当前对话原生工具加载、真实模型准入、审查/Finalize 桌面闭环仍未由本次检查验证。本次仅更新验证记录，不修改生产代码，不派发任务，不调用模型。
