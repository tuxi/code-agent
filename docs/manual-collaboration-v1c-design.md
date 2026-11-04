# V1-c：验证约定持久化与准入一致性

日期：2026-11-03。状态：设计及现有事务验证，生产输入链路尚未修改。依照 V1 需求先评审存储/兼容边界，再实施协议变更。

实现进度补记（2026-11-04）：下文保留原设计评审时的状态与验证记录。当前 C1/C2 候选实现和实际测试分别见 `manual-collaboration-v1c-c1-implementation.md` 与 `manual-collaboration-v1c-c2-implementation.md`；尚未部署，不代表完成生产验收。

## 1. 核实结果与选择

现状：Bridge 的 `development.validation_commands` 只参与进程内去重和 evidence 检查，没有发送给 Daemon；重启/重新绑定后，期望验证项丢失。Daemon 已有 durable TurnInput inbox，不需要再建一个协作数据库。

`internal/session/sqlite/turn_input.go:ReserveTurnInput` 已在一个 SQLite 事务内写入 turn_inputs、turn_accepted 事件并回填 accepted_seq。TurnExecutor 取得成功的持久准入后才开始执行；重复请求返回原 Turn，并由 executor 核对原 payload_hash。上述事务可作为本批一致性边界。

**方案：版本化验证约定纳入既有输入幂等身份，并保存在同一个 turn_accepted 的 JSON payload。** 事件是约定的唯一持久事实；TurnInput 可通过 accepted_seq 读取它，不在 Bridge 文件、第二个数据库或新 Task 表维护副本。

首选不增加 SQLite 表或列，不引入 Outbox、不新增任务调度或 MCP 工具。若实现中证明此边界无法满足原子性，应停下来修订方案，不拆成两个非原子提交。

## 2. 输入与约定

MCP 调用方继续传 development；无需理解内部合同字段或自行分配版本。Runtime/Bridge 的边界使用既有 AgentInput 增量字段 validation_contract，而不是把验证说明混入 system prompt 或模型配置。

示意：

```json
{
  "version": 1,
  "validation_commands": ["swift test"],
  "manual_checks": ["用户确认 iPhone 横屏布局"]
}
```

- version 是协议解析版本，不是可被覆盖的“最新要求”。每个 Turn 的原始约定不可变；修改命令、人工检查或模型请求必须用新的请求身份，不能修改已准入事实。
- 机器命令按原有规则精确匹配；保持明确的数组顺序，不能将模型总结解析为“验证已声明”。人工/设备检查单独记录，自动命令成功不能满足它们。
- 建议上限沿用机器命令 32 项、每项 4096 字节；人工检查最多 32 项、每项 1024 字节。空白项、未知版本及不支持的结构在准入之前拒绝。
- 哈希针对严格校验后的规范化结构，不直接使用任意 JSON 原文，避免字段顺序/空白导致假冲突。约定的原始文本应保存，不自动改写为另一条命令。
- 未提供 development 的旧输入保持原行为。旧事件没有约定时为 unknown/not_declared，不补写过去的数据。
- 合同数据类型必须保持无 Agent Loop 依赖。当前 executionevidence 依赖 reflection/tools，不能直接从底层 session 或 agent.Event 引入而造成循环依赖；应将纯数据定义与判定逻辑分离，判定器仍复用原 executionevidence。

## 3. 持久准入顺序

1. Router 解析并验证输入的约定、request_id、可支持能力及资产边界；失败不启动 executor、模型或工具。
2. 通过可选 typed transport/executor extension 传递约定，禁止旧 CommandTarget 静默丢弃该字段并回退执行。
3. TurnExecutor 用既有文本/模型/资产/来源身份和约定共同计算新请求 hash。没有约定时调用原 hash 路径，保持旧幂等身份。
4. 构造包含精确 Session/Turn/request_id、约定版本、内容及 hash 的 turn_accepted。
5. SQLite 事务同时 Reserve inbox、写 acceptance payload、回填 accepted_seq。任一步失败全部回滚，不发布已准入回执、不执行工具。
6. 提交成功后发布原持久准入身份；执行仍由既有 TurnExecutor 管理，约定不控制 Agent Loop、不自动执行命令、不代表审批。

约定的存储与新请求 hash 必须一致：重复 request_id 相同语义返回原 Turn/原约定；不同约定或不同输入拒绝为 request_conflict。不能由于 resolved_model/动态上游模型身份变化而改变已经冻结的请求身份。

同一 request_id、两进程并发请求仍以既有 SQLite 唯一键为准；Bridge 的缓存不是权威。不得先回 ACK，再异步保存约定。

## 4. 读取与 Handoff

- 重连时从持久 turn_accepted 恢复约定；不能仅从内存 expectations 或最新一条助手消息重建。
- 读取必须匹配 server/store/session/turn/request_id/accepted_seq，并验证约定版本/hash。歧义、事件不完整或损坏时明确拒绝/返回证据不完整，不能静默降级为“有一份内存结果所以通过”。
- 新增可选 Handoff 元数据：validation_contract_version、validation_contract_hash、validation_contract_ref、validation_source。ref 指向精确接受事件，不创建新的产物系统。
- 来源区分 persisted_turn_accepted、legacy_process_local、not_declared；旧 Executor 兼容入口的内存约定必须标明来源，不能冒充持久事实。
- 保留 execution_status、validation_status、acceptance_status 三者独立。约定恢复只解决“应该验证什么”，不是自动证明“已经验证”。
- 命令失败、最后修改后缺少检查、未完成工具、工具参数错误、管道过滤及人工待审批继续使用既有保守规则。
- manual_checks 只作为未完成项；未经可信人工结果接入不能自动标为通过。本版不新增人工验收按钮或通过模型文本采信人工结论。
- failed/cancelled 仍是诊断交接；waiting_for_human 仍是非终态。没有 Task 的手工证据不会因此变成后端 Accepted。

## 5. 能力声明与旧版本边界

建议增量 capability：durable_validation_contract_v1。通过现有 Runtime capabilities 发布，只有实际 executor + repository + store 支持原子约定持久化时才为 true。不能只检查顶层 repo 实现了接口：当前 sqliteRepository 包装器可能存在，而其底层自定义 Store 不支持相应能力。

新 Manual 模式在带 development 派发前核对此能力；缺失/false/读取失败时准入前返回 validation_contract_unsupported，不静默转成进程内约定。没有 development 的普通任务仍按已有语义处理，验证状态保持 unknown。

既有 Executor 入口可保留旧兼容行为，但必须明确 legacy_process_local 来源，不承诺其重启恢复；是否自动升级使用持久能力须在实现时做兼容测试。旧 TUI/AgentKit 不发送该字段时不改变其任务流程。

Agent Event/Agent Wire 仅增量增加可选 metadata，不新增事件 kind，不将多个 Agent 的事件混入 UI reducer。持久约定的恢复由 Bridge 消费已有事件 HTTP 接口，不额外查询模型 Provider。

## 6. 未知准入与恢复底线

| 故障窗口 | 行为 |
| --- | --- |
| 输入尚未到达 Daemon | 没有持久回执；调用方仍不能把本地网络错误直接认作未提交 |
| inbox 插入后、acceptance 写入前失败 | 同事务回滚；无已准入 Turn 或执行 |
| SQLite 提交后、回执丢失 | 原 request_id 查到原 Turn/约定；不重派 |
| Bridge 在收到回执前退出 | 重绑后先核对原 request_id；没有证据仍 unknown，不换新 ID |
| 请求已准入、执行状态未知 | 保留原 Turn/约定和 recovery_required 证据；不因约定可读就允许重跑 |
| 约定事件丢失/截断/版本未知 | 不伪造已验证；读取失败或明确证据不完整 |
| 终态后 Bridge 重启 | 从原事件恢复约定并重新计算证据，不依赖内存 |

需要明确：这一方案不把 Bridge 未发送/在途的所有请求变成持久 Journal，不保证 Session 创建跨进程 exactly-once，也不能识别调用方故意换 ID 的相同业务意图。任何缺少可信准入/副作用证据的恢复继续拒绝自动写入。若产品需要持久的“发送前意图”，应另行评审 Daemon 侧记录方式，不能暗中增加 Bridge SQLite/临时日志来宣称已经解决。

## 7. 迁移、发布与回滚

- SQLite schema 首选不变，只升级新增输入/事件 metadata 和读取投影；没有历史回填、无破坏式 migration，不访问生产 Store。
- 旧数据缺少约定时 unknown；新服务不能根据旧命令日志反向编造当初约定。
- 稳妥升级顺序：Daemon 支持与 capability 验证 → Bridge 候选 → 离线/只读 GUI → 独立授权的真实任务。新 Bridge 对旧 Daemon 必须 fail closed，而不是先派发再发现字段丢失。
- 无 DDL 不等于可随时降级。旧二进制不理解新请求 hash 和约定；存在受影响的 accepted/queued/running Turn 时不能直接让旧 Daemon自动恢复。先停准入并核对任务；无法确认时保留新版本/人工恢复。
- 回滚不删除新事件、约定、输入记录，不恢复过期授权，不清除 recovery_required。普通聊天继续使用可选 MCP。

## 8. 离线验证矩阵

| 编号 | 必须断言 |
| --- | --- |
| C01 | 未知版本、非法命令/人工检查、无 request_id 时准入前拒绝，0 模型请求/工具执行 |
| C02 | 原 inbox/acceptance 事务提交后重开 SQLite，约定内容及 accepted_seq 不变 |
| C03 | acceptance INSERT 故障注入：inbox 与事件均回滚，不能留下半准入 |
| C04 | 同 request_id/同约定幂等，跨进程并发只产生一个原 Turn/接受事件 |
| C05 | 同 request_id 改约定/文本/模型请求拒绝，不覆盖原约定；nil 约定的旧 hash 不变 |
| C06 | 回执丢失和 Bridge 重启：只读核对原请求，取得原约定，不产生重复工具副作用 |
| C07 | 底层 Store 不支持、旧 Daemon 不支持时 capability=false；Manual 带约定发送前拒绝 |
| C08 | 重新绑定后 Handoff 仍能正确区分 complete/partial/unknown，无内存依赖 |
| C09 | failed/cancelled/waiting_for_human、缺检查、修改后未验证、管道和人工待验证均不伪造 PASS |
| C10 | 事件损坏、hash 不匹配、双重接受、回放截断拒绝，不改用 latest/内存猜测 |
| C11 | 无 development 的 TUI/Daemon/旧 Session/Managed 路径保持现有行为，不改 Loop/模型参数 |
| C12 | 真实 Daemon + SQLite + MCP + 本机脚本 Provider 验证准入、执行、重连和交接 |

当前只添加 C02/C03 的现有事务可行性测试，未宣称 typed contract、capability、Bridge 恢复已实现。测试使用临时 SQLite，包含实际关闭重开和数据库触发器故障注入，不调用模型或生产服务。

实际结果：独立临时克隆仅增加本批测试文件，运行两项新用例及既有 TestTurnInputReserveStartAndReload，`go test -race` 共 3 项通过、0 失败，`go vet ./internal/session/sqlite` 通过。日志：`/private/tmp/codeagent-manual-v1c-isolated-feasibility.jsonl`。此结果证明现有事务可承载/保护额外 JSON，不证明新输入协议、约定 hash 校验或重启 Handoff 已接线。

## 9. 最小实施范围

1. C1：纯数据约定/严格校验/hash；可选 Transport/Executor 输入扩展；同一接受事务的持久 metadata。复用现有 inbox，不新增 Loop 分支。
2. C2：真实能力声明、Agent Wire/HTTP 事件投影、新 Manual 的准入前检查与 Bridge 恢复；补齐上述负向矩阵。
3. C3：独立快照兼容测试、发布/回滚记录；V1-d 再进行真实桌面验收。任何真实模型调用另行授权。
