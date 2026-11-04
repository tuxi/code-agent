# V1-c/C2：输入接线、能力与持久 Handoff

日期：2026-11-04。候选实现，未部署或调用第三方模型；不等于整个 Manual Collaboration V1 生产验收。

## 接线与兼容边界

- `AgentInput.validation_contract` 为可选、严格版本化的纯数据。Router 在执行前拒绝非法结构、未知字段/版本、null、重复约定/身份字段、非 text 输入和不合法 request_id；仅调用可选 typed target。缺少能力时不会回退到旧 SendMessage。
- `TransportSession` 把输入交给 C1 的原 TurnExecutor/SQLite 接受事务。未带约定的 TUI/AgentKit/旧客户端仍走旧入口，不改变 Agent Loop、提示词、模型参数或审批。
- HTTP `durable_validation_contract_v1` 和 WebSocket Hello 根据实际 Executor + Repository + 底层 Store 能力发布；配置标志不能让不支持的 Store 冒充支持，动态 capability resolver 也不能绕过降级。
- Agent Wire 和既有 HTTP events 投影携带接受事件的约定/hash，不增加事件类型、接口或数据库。授权、workspace/server/store 绑定沿用原路径。
- 新 Manual profile 的 development 在发送前检查 Runtime capability，失败/缺失/false 均返回 `validation_contract_unsupported`，不派发、不创建准入缓存、不静默降级。约定进入原 wire payload 与幂等身份；持久回执必须恢复到匹配的原约定。
- 旧 Executor profile `SendWithExpectations` 保留兼容行为；来源标为 `legacy_process_local`，不宣称跨进程持久化。没有约定的旧事件为 `not_declared`。

## Handoff 恢复

Read/Artifacts/Handoff 共享接受事件来源投影：`validation_source`、`validation_contract_version`、`validation_contract_hash`、`validation_contract_ref`。引用包含精确 Session、Turn、request_id、accepted_seq；有已核验绑定时附 server/store 身份。

从完整、有序的原接受事件恢复；拒绝双重接受、重复请求身份、错误 Session、终态缺失接受事件、未知版本、hash 不匹配、null 及回放截断。重绑时也检查接受约定。尚未观察到接受事件的旧非终态状态/审批读取仍可展示 waiting 或 attention，不编造准入、验证完成或终态交接。

Handoff 收集前后核对状态、约定来源和工作区 Diff；不是不可变 ReviewBundle。合同恢复不会使当前工作区成为逐 Turn 冻结快照，也不自动证明外部写入或人工检查安全。所有 manual_checks 始终列入 `pending_manual_checks`，即使机器命令成功也不自动通过。

execution、validation、acceptance 仍独立；failed/cancelled 是诊断交接，审批等待不是终态；手工 Handoff 的 acceptance 保持 not_reviewed。约定本身不是审批、调度或自动执行指令。调用方仍需在任务文本中向 Executor 说明应执行的检查。

## 验证范围

新增离线用例覆盖 Router 拒绝与不回退、底层 Store 能力及 HTTP/Hello 一致、Wire 往返、能力查询失败时零派发、损坏/歧义接受事件、无缓存恢复、人工检查未完成以及 failed/cancelled/人工等待状态。

真实组件用例使用本机脚本 Provider、真实 Runtime/SQLite/工具/MCP；销毁 Bridge 和 MCP Session 后，重新显式绑定并一次读取 Handoff，核对原约定引用/hash、命令结果、Diff 和 not_reviewed，且脚本请求数不增加。这是 Bridge 对象/连接重建，不是新一轮独立进程 SIGKILL 或真实桌面/模型验收。

### 实际结果

验证使用 `ca92dc9a61cd97541604a54b4fd1c9ed932f1367` 的独立临时克隆，按明确文件清单叠加必要的 V1-a/V1-b/C1 前置改动和 C2；不依赖工作区其余 TUI、Runtime、工具或 G1/G3 草案改动。38 个 Go 源码/测试文件的 SHA256 与被测快照一致。

- C1+C2 定向 `-race`：23 项顶层测试通过，0 失败。包括原事务与幂等测试、Router/Wire/capability、恢复负向测试、原注意事项兼容和真实组件的 Bridge 重建。
- 最终 `go test -race ./internal/server ./internal/mcpbridge ./cmd/codeagent-mcp ./internal/executionevidence -count=1`：302 项顶层测试，290 通过、8 跳过、4 失败，无 DATA RACE 报告。Bridge、CLI 和 executionevidence 三个包通过；Server 仍不全绿。
- 四项失败均在未修改基线以相同 `-race -count=1` 独立复现：`TestProviderStoreUpsertBuiltinFillsModelsFromRegistry`（目录数量预期），`TestSharedEnrollmentWaitsForHostAcknowledgement`、`TestSharedEnrollmentTimeoutRevokesPendingStateAndKeepsBootstrapRetryable`（pairing bootstrap 拒绝），`TestRouterTerminatesEveryPreAcceptFailureWithoutRejectingAcceptedTurns`（pre-accept 失败未回报）。本批没有修改这些生产逻辑或断言。
- `go vet ./internal/server ./internal/mcpbridge ./internal/conversation ./internal/session/... ./internal/executionevidence ./cmd/codeagent-mcp`：通过。
- Daemon 和 MCP 候选二进制构建通过；仅输出到 `/private/tmp/codeagent-v1c-c2-codeagentd`、`/private/tmp/codeagent-v1c-c2-mcp`，没有安装或启动它们。
- `git diff --check` 通过。没有运行全仓测试，不能宣称全仓全绿。

本机临时证据：`/private/tmp/codeagent-v1c-c2-contract-final.jsonl`（23 项定向）、`/private/tmp/codeagent-v1c-c2-release-check.jsonl`（最终相关包）、`/private/tmp/codeagent-v1c-c2-baseline.jsonl`（4 项基线复现）。快照位置记录在 `/private/tmp/codeagent-manual-v1c-c2-location.txt`，其 `scope.json` 保存文件清单、基线及哈希；临时日志不是长期归档。

回归过程中曾发现两处本批兼容问题：空的尚未开始 Turn 查询、缺实时请求帧的非终态审批读取被过度拒绝；均修正生产逻辑后重新验证，保留原有测试断言。人工检查未完成的规则放在原 executionevidence 判定器中统一处理，Bridge 只补充待核对项和来源。

## 升级与回退

没有新增 schema 或历史回填。升级顺序为支持 C1/C2 的 Daemon → Bridge → 离线/桌面只读验证。新 Manual 带 development 对旧 Daemon 拒绝；旧 Executor 的进程内兼容路径需明确来源。

存在带约定的 accepted/queued/running 输入时，不能直接让不理解新 hash/metadata 的旧二进制自动恢复；先停准入并核对任务，不删记录、不清租约、不修改 recovery_required。普通不带约定的旧输入无需数据库迁移。

本批未提交、推送或安装候选二进制。生产授权、Store 和 Talkify 均未操作。C3 的兼容快照与发布记录、V1-d 的真实桌面验收仍是后续工作。
