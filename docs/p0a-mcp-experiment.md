# P0-A：隔离的 MCP 协作实验

状态：实验实现；默认不启用，不代表 P0-B/P0-C 的任务恢复和审查保障。Agent Loop 仅增加模型审计元数据，不改变决策/执行顺序；未增加 Session schema、Flux 或 AgentKit UI，也没有自动配置用户的 MCP。

## 实现范围

独立入口 `cmd/codeagent-mcp`，复用现有 MCP SDK，无新增依赖。

```text
Codex / MCP client → stdio Bridge → existing HTTP / WebSocket
                                  → Repository / TurnExecutor / Agent Loop
                   ← receipt / exact-turn events / current diff / command evidence
```

工具：`codeagent_list_models`、`codeagent_create_session`、`codeagent_send_to_session`、`codeagent_wait_sessions`、`codeagent_read_session`、`codeagent_get_artifacts`、`codeagent_cancel_turn`。

为限定实验边界，一个 Bridge 进程绑定一个干净、已有 HEAD 的本机 Git 仓库和一个会话，只允许一个执行中的 turn。仓库应是专用临时克隆。HTTP endpoint 必须为数字 loopback 地址和显式端口；本版不支持远程 runtime、HTTPS 或自动发现 app 内的随机 token。

## 构建与运行

```sh
go build -o /absolute/path/codeagent-mcp ./cmd/codeagent-mcp
```

使用操作者已明确绑定的 Daemon 地址及 access token，token 从环境变量读取，不作为命令行参数、MCP 参数或工具结果传递。Bridge 不启动另一个 Runner。

```sh
/absolute/path/codeagent-mcp --experimental-p0a \
  --runtime-url http://127.0.0.1:8797 \
  --workspace /absolute/path/disposable-repo
```

启动环境需提供 `CODEAGENT_BRIDGE_TOKEN`；也可用 `--token-env VARIABLE_NAME`。实际工作区和 Daemon 必须在同一主机。运行时策略继续由原 Daemon 决定：Bridge 不放行审批；需要人工决定时返回 waiting_for_human，由用户在原会话 UI 处理。

可供独立 Codex CLI 实例使用的配置示意（未写入用户配置）：

```toml
[mcp_servers.codeagent_p0a]
command = "/absolute/path/codeagent-mcp"
args = ["--experimental-p0a", "--runtime-url", "http://127.0.0.1:8797", "--workspace", "/absolute/path/disposable-repo"]
env_vars = ["CODEAGENT_BRIDGE_TOKEN"]
```

接入方式参考 [Codex MCP 官方文档](https://developers.openai.com/codex/mcp)。当前聊天不会因为编译了二进制就自动获得新工具。

## 手工实验流程

1. Codex 读取任务范围、制定计划和验收条件；明确禁止提交、安装依赖、递归委派以及修改其他工作区。
2. 创建会话；send 使用稳定 request_id，取得实际 durable admission 的 session_id / turn_id / cursor。省略 model 即继承 Runtime 默认模型及其配置，无需先查询目录。需要显式选择时，可通过 list_models 查询共享目录并传入命名空间 ID。
3. wait 保留 admission cursor；单次最多等待 30 秒，超时不会取消执行。输入结果不明时仅重试相同 request_id 和 payload。
4. turn 结束后读取 exact-turn events 与 artifacts。artifacts 包含基线 commit、暂存/未暂存/未跟踪文件的当前 Diff、Diff hash，以及 foreground run_command 的命令/退出码/输出。
5. Codex 阅读实际代码及 Diff，核对测试命令、退出码和覆盖范围，形成审查意见。需要返工时显式发送下一轮请求；P0-A 不自动循环。

缺失退出码、后台任务尚未完成、格式无法解析的命令结果均不标为测试通过。输出被截断会显式标记。当前 artifact 是采集时的工作区状态，不是不可变或严格归属于某轮的快照；必须停止其他写入者，且不得把多轮累积 Diff 误认为单轮结果。

## 验证层次

- `TestMCPDelegationAndEvidence`：MCP 契约、稳定 request_id、重复派发、错误会话、exact-turn 隔离、实际 Git diff、失败/后台命令及输出上限。
- `TestHumanApprovalAndCancellation`：等待人工审批、另一客户端已解决审批后不持续误报、请求取消后的 terminal。
- `TestEndpointAndWorkspaceRestrictions`：绑定范围和干净仓库前置条件。
- `TestDaemonStdioToolRoundTrip`：真实 Repository、SQLite EventStore、TurnExecutor、ServeRunBuilder、HTTP/WebSocket、stdio MCP，以及实际 read/edit/run_command 工具；模型响应由本机脚本固定提供，禁用全局 MCP 发现。它证明技术链路，不证明 agent 的任务质量或效率。
- `TestLiveDaemonRoundTrip`：显式 opt-in 的真实模型实验。使用同一套隔离生产组件和实际 bridge 二进制，配置来自显式隔离 settings 文件；调用前需对接收方、模型和数据范围重新取得授权。

普通测试不会连接外部模型：

```sh
go test -race ./internal/mcpbridge ./cmd/codeagent-mcp
go vet ./internal/mcpbridge ./cmd/codeagent-mcp
go build ./...
```

真实模型实验需显式提供 `CODEAGENT_P0A_LIVE=1`、`CODEAGENT_P0A_WORKSPACE`、`CODEAGENT_P0A_BRIDGE`、`CODEAGENT_P0A_PROMPT`、`CODEAGENT_P0A_OUTPUT`，这些路径均须为绝对路径；另需 `CODEAGENT_P0A_SETTINGS` 指向显式隔离 settings JSON；凭证由其中引用的既有 resolver 提供。可选 `CODEAGENT_P0A_MODEL` 使用完整命名空间 ID，留空继承该 Runtime 默认模型。driver 不内置模型、Provider endpoint 或 thinking 覆盖；调用预算与外发隔离须由实验运行器设置。`go test ./internal/mcpbridge -run '^TestLiveDaemonRoundTrip$' -count=1 -v -timeout=9m` 会消耗真实 API 用量并记录 receipt/events/artifacts/run JSON。

该 driver 按本聊天中 Codex 已制定的任务调用 MCP，并非另一个自主选择工具的 Codex CLI 实例。真实模型完成后，还需本聊天中的 Codex 独立审查补丁。单次成功只能证明可行性，效率/质量改善仍需后续同任务对照。

## 当前执行状态（2026-10-27）

本机脚本模型测试已贯通真实文件修改、命令执行、动态模型选择和身份/用量回传。此前经用户一次性授权的 DeepSeek 实验只验证了部分链路，未通过任务验收：新增测试 fixture 有误，前台测试子进程长时间运行后由 Codex 停止。不能据此宣称效率或质量提升，也不能沿用该授权开展下一次外发。

本轮基础接口与超时修复仅运行离线测试。实现与边界见 [模型目录与命令可靠性](model-discovery-and-execution-reliability.md)。

## 已知边界

协作与模型内部配置的职责、临时预算代理的限制及任务级预算缺口，见 [Agent 独立性边界复核](p0a-agent-boundaries.md)。Bridge 不提供 max_tokens 或 Thinking 控制，也没有正式 budget_exhausted 协议。

- Bridge 的创建回执缓存和会话绑定仅在进程内。普通 create API 没有持久化幂等；创建结果不明时先检查 Daemon，不能自动重复创建。
- 没有 Task、Outbox、ReviewGate、不可变 Artifact、审批跨重启恢复和自动接管；这些属于后续阶段。
- Codex 为 reported 参与者；Bridge 无法观察其完整工具轨迹或管理它的生命周期。
- 已完成 turn 的后台 job 不等于已停止；实验任务必须使用 foreground 命令，其他进程写入也不受 Bridge 的会话锁约束。
