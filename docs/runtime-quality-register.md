# Runtime 质量修复与基线登记

2026-10-27。本批独立于跨 Agent 产品设计；P0-A 的 MCP 输入、模型选择、Thinking 和输出配置均保持原样。没有真实模型调用、依赖安装、提交或部署。

## 已修复：失败响应 usage 丢失

根因有两层：OpenAI-compatible / Responses 的错误或中断路径丢弃已解析的 usage；ResilientProvider 在无效响应、重试耗尽、取消和流式回退时清空响应，只对最终成功响应计量。一个逻辑 model_finished 因而少算失败的物理请求。

当前行为：

- Provider 在收到有效 usage 快照后，即使 HTTP/流式协议失败也保留用量及 actual_model；重复的流式累计快照取最后一份，不叠加。
- 重试层累计每个物理请求的已观测 usage，包括校验失败、失败流、回退及取消。失败结果保留计量/来源，清除可执行工具调用、内容和 Raw，仍按原策略失败或重试。
- `Response.Usage` 保留最后一次物理响应大小；新增内部 `AccountingUsage()` 供账目读取。自动压缩、Session.PromptTokens 和缓存命中采样继续读取最后响应，避免把重试输入累计当作上下文大小。
- model_finished、Session last_model_call、Turn Token/Billing 汇总读取累计账目；错误状态仍为失败，不能因为有 usage 就标记模型验证成功。
- Observer 的 RequestStat 包括失败用量，Trace 在内存含每次 usage；失败流和后续 Complete 各自记录 telemetry，累计不重复。现有 SQLite AttemptRecord 仍只存耗时/结果，未在本批扩展逐次 usage schema；持久化汇总由已有 RequestRecord 和逻辑 model_finished 保存。

边界：只累计上游实际提供的用量。上游未发 usage 就断线、无法解析的残缺 JSON、Provider 隐藏消耗仍未知；不能凭空恢复账单，也不声称这些数值可以独立实施费用硬上限。本批不改变模型请求参数、超时或重试次数。

离线新增覆盖：错误响应携带 usage、截断工具 JSON、重试成功/耗尽、流式错误及回退、重复 usage 快照、缺少终态、主动取消、禁止错误工具执行、model_finished 与 last_model_call 保留账目、上下文大小不被累计值放大。Ollama 在 Scanner 错误时也保留已经观测到的用量。

## 已复核：工具参数校验

现有 Tool Dispatcher 统一在 hook / Inspect / 审批 / 执行前校验参数。缺少 path 返回 `invalid_tool_arguments` 和 retryable_by_model；有效结构仍走原权限/审批。普通、并行、客户端和 nested dispatch 的离线回归均通过。本批没有再添加重复的 edit_file 专用逻辑，也没有放宽审批。

## 基线失败：10 项仍开放

本次重新运行 Runtime / Server / Agent 三包，失败集合与 `/private/tmp/codeagent-foundation-validation.json` 的已复现基线完全一致。下列为实际失败表现，不将现象直接当作已确定根因，不通过放宽断言或跳过全部测试掩盖问题。

| 优先级 | 测试 | 当前失败表现 / 后续检查 |
| --- | --- | --- |
| 协作前置 | TestEventStoreEmitterCapsToolStreamPersistence | 序列化持久化约 80,903 字节超过断言预算；检查原始 chunk、JSON 封装和 marker 预算/关联，需覆盖 UTF-8 与终态 flush |
| 协作前置 | TestRouterTerminatesEveryPreAcceptFailureWithoutRejectingAcceptedTurns | 普通 pre-admission 失败未返回预期拒绝；检查接受前后边界，避免已接受 Turn 被误拒绝 |
| Flux 接管前 | TestFluxWorkflowToolEmitsTerminalFailureWhenPlanningCannotStart | 预期 workflow failure 生命周期为空 |
| Flux 接管前 | TestFluxWorkflowToolRunsGeneratedDefinitionWithCanonicalEngine | planner billing 未返回，ModelUsage 为 nil；单独核对工具层计量与生命周期 |
| Flux 接管前 | TestFluxWorkflowToolExecutesCrossSessionDispatchDAG | 缺少预期 workflow admission |
| Flux 接管前 | TestFluxWorkflowToolExecutesCrossWorkspaceMapTemplate | Map 未在测试时限内完成，检查子任务并发和 Await，不能仅扩大超时 |
| 独立修复 | TestRunTurnWithAssetsPreservesUserGatewayAsset | 断言资产引用丢失；实际请求含用户资产 manifest，先核对已演进的 Gateway 资产契约 |
| 独立修复 | TestProviderStoreUpsertBuiltinFillsModelsFromRegistry | GET 模型视图与测试预期不一致；核对原有 Registry 改动和动态展开，不能为测试回退模型选择 |
| 配对入口 | TestSharedEnrollmentWaitsForHostAcknowledgement | 未创建预期待确认 enrollment，检查 bootstrap 与当前认证前置条件 |
| 配对入口 | TestSharedEnrollmentTimeoutRevokesPendingStateAndKeepsBootstrapRetryable | 请求返回 401 pairing_bootstrap_invalid，核对 fixture/认证语义后再改 |

先处理事件预算、pre-admission 和 Flux 相关失败，再实施 B2 的受管派发。它们不阻止 B1 的 Task/Session 契约设计；不能据本批聚焦检查宣称全仓库测试通过。

## 本批验证

受影响 Model / MCP / Agent / Tools / ExecutionEvidence 的全包 race 通过，其中 Agent 仅排除上述已知资产基线失败；三包完整复核保留原 10 项失败，没有新增。`go build ./...`、受影响包 `go vet` 通过。未运行全仓库测试。环境去除 API 密钥和 live 标志，依赖下载关闭；仅使用本机假 Provider/Daemon 与临时克隆。

另以本批开始时的源码建立只读 Go overlay，新 usage 回归能稳定检出原缺陷；当前实现通过同一回归。设计 fixture 仅验证 JSON、参与/attempt/产物引用及游标关系，未宣称运行 Go/Swift 生产 DTO 解码或 UI 测试。

日志、命令及耗时：`/private/tmp/codeagent-runtime-quality-checks/`。本批开始时的源码快照：`/private/tmp/codeagent-runtime-quality-before/`。P0-B 的设计产物见 [最小 Task/Session 契约](p0b-task-session-contract.md)。
