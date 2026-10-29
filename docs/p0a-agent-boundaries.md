# P0-A：跨 Runtime Session 协作边界复核

结论：现有 P0-A 的委派接口符合 Agent 独立性原则。任务级预算协议尚未实现，不能把实验代理当作正式任务资源管理能力。本次不改 Runtime 模型选择、Thinking、Provider 或 Agent Loop，也不进入 P0-B/C。

## 当前源码核查

| 约束 | 实际实现 | 结论 |
| --- | --- | --- |
| MCP 负责会话、委派、等待、结果和反馈 | `internal/mcpbridge/mcp.go` 的七个工具；返工通过同一 Session 的下一次 send 提交 | 符合；另有可选只读模型目录和取消工具 |
| 默认继承 Runtime 配置 | `SendWithExpectations` 仅在 model 非空时查目录；`resolveTurnModel` 对空值返回 Runtime 默认 ModelConfig，`BuildRunner` 使用其 ReasoningEffort | 符合；客户端无需先调用 list_models |
| 调用方不能控制内部生成参数 | MCP 输入没有 max_tokens、max_output_tokens、reasoning_effort、thinking、temperature 或 max_steps | 符合；目录中的推理能力/默认值只是只读信息 |
| 模型选择可选且不静默换模型 | 显式选择在新 Turn 边界解析；未知/不可用选择报错，不改 Runtime 默认值 | 符合；细粒度的每调用方模型选择授权尚未实现，现有边界为 Daemon 访问凭据和已配置模型目录 |
| 原有审批机制保留 | waiting_for_human 非终态，返回 attention_required 和控制引用；Bridge 不批准 | 符合 |
| 执行结束不等于任务验收 | artifacts 提供实际 Diff、前台命令和执行证据；task_acceptance 为 not_reviewed | 符合；Codex 仍需独立审查 |
| 任务预算耗尽明确停止 | Bridge 没有正式任务预算、原子预留、费用记账或 budget_exhausted 契约 | 未实现；P0-A 仅由显式实验运行器约束，不能宣称已具备正式保障 |

MCP 委派必需信息仍是 session_id、message 和用于幂等的 request_id。model 为可选项；development 只声明需要核对的验证命令，不改变 Runtime 推理或工具权限。

P0-A 仍限于数字 loopback、专用临时克隆、每 Bridge 一个绑定 Session 和一个活动 Turn。它验证跨 Runtime 的接口连接；尚非任意远程 Agent 的通用协作平台。Codex 仍是 reported 规划/审查参与者，Daemon 不管理其生命周期。

## 实验限制的归属

之前真实重试的链路是 `Codex → MCP → Daemon → 临时预算代理 → DeepSeek`。`/private/tmp/codeagent-collab-retry-jnsa8728/gateway.py` 将单请求 max_tokens 额外设为最多 4096，还将上游传输改为非流式。`gateway-proposed.py` 中的 12288 只是未执行的后续实验提案。这些均不是正式 Bridge 或 Session 协议的默认行为。

原 Runtime 的 OpenAI-compatible / Responses 请求未设置输出 Token 上限，省略时由 Provider 默认处理；它也并非已经支持通用的 max_tokens 用户配置。不能因为实验代理问题，声称现有 Runtime 存在 4096 硬编码，或把代理造成的工具参数截断归因于 Agent 自身能力。

此前开始但尚未完成的统一输出策略、模型能力刷新、Provider 输出参数和 MCP 离线输出计算入口已全部撤回。撤回前的草稿保存在 `/private/tmp/codeagent-output-policy-withdrawn/`；`integrity.json` 确认原有 56 个改动文件逐字节保持一致。模型输出策略可作为独立 Runtime 议题另行评估，不属于跨 Agent 协作的前提。

历史实验报告和代理保持原样，保留实际限制、usage、finish_reason 和失败归因。失败响应 usage 的单独 Runtime 修复与离线证据见 [质量登记](runtime-quality-register.md)；历史报告不会因此改写，未报告的用量也不能据此推算。

## 下一次实验的预算要求

预算由资源所有者授权，委派方可提出更低约束，不能据此提高 Runtime 或部署上限。预算控制器应在物理请求开始前预留调用次数、输入/输出 Token、费用及时间；重试、流式回退和失败请求都计入。预留不足时应在请求前明确停止，报告 budget_exhausted，不能通过偷偷降低每请求输出上限或裁剪响应 JSON 继续工作。

如果 Provider 默认输出上限未知，就不能同时声称“完全透明转发”和“费用硬上限已证明”。应先取得部署所有者确定的有效上限以便预留，或在请求前返回 budget_configuration_required。预算不足以容纳完整请求时停止；已准入请求按原配置完整返回。时间、取消或其他安全硬限导致中断时须单独标记，部分结果不得作为完成的工具调用执行。

实验若需要额外输出限制，必须事先说明并获授权，独立记录原请求、代理施加的限制及截断原因；不能把它写入正式 MCP 协议。本次只做源码和离线核查，没有启动第三方 API 实验，也没有实现新的预算控制器。

## 离线回归

`TestMCPContractKeepsGenerationControlsInRuntime` 检查实际发布的 MCP InputSchema：没有内部生成参数，model 不为必填。原有 `TestCatalogSelectionRejectsUnavailableWithoutSubmission` 验证省略模型不查询目录；`TestNamespacedTurnSelectionAndDefaultIsolation` 验证默认继承与严格模型解析；`TestDaemonStdioToolRoundTrip` 验证真实 Daemon/Agent Loop 的工具链、审计和实际产物。

本次实际通过：MCP / Model 全包 race 测试、Runtime 默认继承聚焦 race 测试、`go build ./...`、MCP / Model / Bridge 命令的 `go vet`。测试环境不继承 API 密钥或 live 标志，Go 依赖网络下载关闭；没有运行全仓库测试。命令、耗时和日志保存在 `/private/tmp/codeagent-boundary-audit/`。
