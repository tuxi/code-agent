# V1-c/C1：约定类型、幂等身份及接受事务

日期：2026-11-04。候选实现，尚未部署；本批不是整个 V1-c 或生产发布验收。

## 实际改动

1. `session.ValidationContract` 是无 Agent Loop 依赖的纯数据类型：version=1、validation_commands、manual_checks。严格检查未知/重复键、版本、UTF-8、空白项、数量和字节上限；规范化 JSON 哈希保留原始检查文本及顺序，深复制避免调用方改写已接收数据。
2. 新增 typed Go 输入入口 `TransportSession.SendMessageWithValidationContract` / `TurnExecutor.ExecuteWithValidationContract`，参数显式传到原 claimRequest，不使用隐式 context 继承，不注入模型提示词或修改生成参数。
3. 约定哈希纳入请求 payload_hash；nil 约定继续使用原哈希。resolved_model 仍不属于请求身份。同 request_id 修改/移除约定均冲突，相同输入只确认原 Turn。
4. 既有 turn_accepted 增量增加可选 validation_contract 和 validation_contract_hash。SQLite Reserve 在原 inbox/接受事件/accepted_seq 的事务中保存，约定与输入不一致时拒绝；重复回执携带原约定和持久序号，不补写第二条事件。
5. TurnInput 通过原 accepted_seq 读取接受事件中的约定，核对 Session/Turn/request/类型/hash。重开数据库及 RecoverableTurnInputs 均可恢复；损坏的约定不静默变为成功。
6. 使用可选 `SupportsValidationContracts` 持久化能力端口，sqliteRepository 根据实际底层 Store 判断；仅实现通用 TurnInputStore 不等于具备这项能力。新 typed 输入对不支持的 Store 在执行前拒绝，旧入口保持原行为。

没有新增数据库表/列、调度器、Outbox、MCP 工具、模型参数或恢复状态机。既有 SQLite 的事件序号来自 session_events.id；不是一个新增 seq 列。

## 本批边界

- 已实现的是 typed Go transport → TurnExecutor → 原 SQLite 接受事务。
- Agent Wire Router、HTTP capability 发布、Bridge 的 durable validation 准入检查和 Handoff 来源/版本恢复在 C2；当前 MCP development 仍沿用先前实现，不能提前宣称已持久化。
- manual_checks 在 C1 只保存声明，不代表已经完成人工验收。自动证据判定与 UI 不在本批改动。
- 两个 Store/Executor 连接的并发测试不等同于两个 Daemon 进程或 SIGKILL 验收；独立进程故障窗口仍需后续覆盖。
- 不持久化所有发送前/在途意图，不解决 Session 创建的跨进程去重；没有可信准入证据时保持 unknown，不自动重派。

## 测试口径

新增用例覆盖严格解码和哈希、文本/顺序/人工要求的身份差异、深复制、旧元数据、错误身份/hash、SQLite 重开和恢复读取、接受事件失败时整体回滚、同 request_id 不能覆盖原事件、两连接并发只保留一次准入、两个 Executor 的不同约定争用仅一次执行、重启后原请求去重和合同删除冲突，以及 legacy hash/事件不变。

模型相关执行使用确定性的测试 Runner 计数器，没有调用 Provider。计数用于证明重复请求未再次运行，不证明真实模型质量或额度收益。原事务可行性测试补充了当前约定格式要求的 hash，原持久事件不变/整体回滚断言保留。

### 实际验证结果

以 `ca92dc9a61cd97541604a54b4fd1c9ed932f1367` 为基线建立临时克隆，只叠加 C1 文件，排除工作区内 V1-a/V1-b 和其他未提交改动：

- `go test -race ./internal/session ./internal/session/sqlite ./internal/conversation -run 'TestValidationContract|TestAcceptedValidation|TestTurnInputReserveStartAndReload' -count=1`：13 项顶层测试通过。
- 上述三个包完整 `-race -count=1`：260 项顶层测试通过、1 项跳过、2 项失败。`TestSequencingEmitterCapsToolStreamPersistence` 保存 79869 字节，超过 65536 预算；未修改基线同样失败且数值相同。`TestTurnExecutor_CancelReleasesSchedulerSlot` 报告 fakeRepo 的 `executor_test.go:75/77` 数据竞争；基线单次未复现，随后单独重复 10 次有 1 次复现相同竞争。不得据此声称完整套件通过或整个包 race clean。
- `go vet ./internal/session ./internal/session/sqlite ./internal/conversation ./internal/agent`：通过。
- `go build -o /private/tmp/codeagent-manual-v1c-c1-daemon ./cmd/codeagentd`：通过；候选二进制未运行或安装。

测试日志位于本机临时目录：`/private/tmp/codeagent-manual-v1c-c1-final-targeted.jsonl`、`/private/tmp/codeagent-manual-v1c-c1-isolated-tests.jsonl`、`/private/tmp/codeagent-manual-v1c-c1-baseline-tests.jsonl`、`/private/tmp/codeagent-manual-v1c-c1-baseline-cancel.jsonl`。临时证据不是长期归档；上面的结果及基线 SHA 是本报告保留的验收口径。本批不修改这两项既有失败的代码或断言。

## 发布与回退

SQLite schema 不变，不回填历史约定；旧数据缺约定时仍未知。新增能力需等 C2 接线和能力声明测试通过后才能对外启用。

没有运行中合同输入时可回退候选二进制；已有 accepted/queued/running 合同输入不能直接交给不理解它的旧版本自动恢复。先停准入并核对任务；不删事件、不改 payload_hash、不清空恢复保护。旧客户端不带约定的路径不需要数据库迁移。

未提交、推送、部署、修改生产 Store 或调用第三方模型。
