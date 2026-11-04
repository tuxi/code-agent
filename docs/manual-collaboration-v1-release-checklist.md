# Manual Collaboration V1 发布收尾

2026-11-04；状态 **NO-GO**。机器清单：[release-readiness](contracts/manual-v1-release-readiness.json)。历史 C3/P0.5 清单保留，不用当前结果追改旧测试结论。

## 本批改动与验证

现有代码已由其他操作提交到 `eae1c4552727fd642962b613c2ad667cd521e92d`；本批经用户授权，以包含本文的收尾提交固定四份测试、验收文档及 manifest，未部署。不修改 Agent Loop、认证、模型参数、Flux 或 UI。工作区另有 `internal/server/history_diffstat_test.go`，未修改、未纳入候选快照或本次提交。

- 用户明确批准后，Provider 用例改为核对内置 Registry 的完整有序模型 ID 列表，仍要求目录非空、磁盘不保存模型快照、自定义 Provider 显式提供模型、显式列表覆盖 Registry。
- 两项配对用例改用协议要求的 base64url 编码原始 32 字节 bootstrap，对原始字节求 hash；主持确认、超时、可重试、撤销和一次性消费断言不变。新增 malformed/short/wrong secret/hash-of-encoded-text 拒绝检查。生产认证未改。
- 工具流预算用例在原 ca92dc9 上也失败：测试将 JSON 包装大小混算为正文预算。测量显示正文头部 65,536 字节、含尾部的标记 4,202 字节、JSON 包装约 10 KB。经用户批准后改为分别严格核对头部、4 KiB 尾部、单标记、事件身份、记录数量和序列化开销，保留完整实时流断言；没有提高正文预算。
- 扩大 race 回归捕获 fakeEventStore 的并发 Append 竞态。仅给测试替身 Append/Replay/ReplaySince 加锁，Replay 返回切片副本；未改变生产 Store 或测试断言。

独立快照从 eae1c45 克隆，只叠加已批准的测试修正，排除其他未提交文件。记录所有失败和后续修复，不通过重试掩盖失败：

| 检查 | 结果 |
| --- | --- |
| Server 全包 race | 215 顶层通过，0 失败 |
| Conversation 全包 race，替身加锁后 | 92 顶层，连续 3 轮全部通过，276 次执行，0 失败 |
| Session、SQLite、executionevidence、Bridge、CLI race | 同一提交及前三份测试修正的前一快照全部通过；随后只修改 Conversation 测试替身 |
| 新旧实际 CLI/Runtime 独立进程兼容 | 1 顶层、2 子场景通过；原 Store 事实保留，重启 MCP 恢复原验证约定 hash，不重派；Provider 调用 0 |
| vet | 相关包通过；最后改动的 Conversation 包再检查通过 |
| 全仓测试 / 新模型任务 | 未运行 / 未授权 |

原 3 项 Server 失败、预算用例失败及 race 日志均保留在 `build/manual-v1-release-readiness/`；最终快照和输入 hash 见机器清单。跨版本复验实际使用新构建的 eae1c45 候选，不将旧 C3 的通过结果冒充本轮结果。

## 原生桌面空 Session 重绑定

用户明确授权后，在既有隔离验收工作区创建 Session `20261104-084452-3e9f2643`，只新增会话元数据，无 Turn、模型调用或文件修改。

用户保持 Codex/Talkify 运行，撤销并重新授权后，原生工具实测：旧 binding 返回 grant_replaced；服务显式 reconnect 返回 bindings_require_revalidation=true；旧 binding 仍拒绝；按原 Runtime/Store/Session/基线提供 replace_binding_id 成功得到新 binding；新句柄身份有效，旧句柄 binding_not_found。重复绑定同一元组复用新句柄。

原句柄 `89931bd5-c029-4a08-9c26-94fac65c8d43`；新句柄 `b25ac61d-d929-4ff9-bad5-c3b4ac350e61`。它们是本 MCP 进程路由引用，不是凭据，也不承诺进程重启后仍有效。此场景不代表执行中或待审批 Turn 的桌面恢复通过。

用户随后明确选择停机验收并退出 Talkify。只读取进程可执行文件名，确认 Talkify 和其内嵌 Daemon 均不在运行，原候选 Bridge 仍在运行。原生 connection_info 普通查询及上述新 binding 查询均返回标准工具错误 grant_invalid / retryable=false，没有关闭 MCP；用户在同一 Codex 对话成功发送消息，本轮工具调用和回复正常。退出场景的“服务停止不阻断普通聊天”通过。该错误发生于本地授权校验阶段，不声称已测试 TCP 连接超时或网络黑洞。

用户启动 Talkify 并恢复原授权后，普通/旧 binding 查询均返回 grant_replaced。使用原 Runtime/Store/workspace 显式 reconnect 后，返回 reconnected=true、bindings_require_revalidation=true；按原 Session/基线并指定 replace_binding_id 成功恢复为 `ce4a677f-0268-49be-93da-99b9b0652aa3`。新句柄 identity_verified=true、execution_bound=true，原 Session ID 不变；前一 `b25ac61d-...` 句柄返回 binding_not_found。停机至恢复期间 Codex/MCP 没有重启或重载，没有发送 Turn、审批或模型调用。空 Session 停机恢复子项通过，不外推为执行中/待审批工作恢复。

## 版本与发布边界

已记录 code-agent eae1c45、AgentKit `1c4601e3fd3d2dee4b0b79f3eb3f559d6f351c7c`、Talkify `901cf877ea2f500563f68411e42d877438dca792` 的观测 HEAD，以及实际运行中 App/Daemon/Bridge 的 SHA256。Daemon build info 为 eae1c45 / modified=true；实际 Bridge 仍是此前冻结快照构建，不能把其 ca92dc9 基线标签当作候选完整源码版本。

这些是观测清单，不是已完成的干净发布版本映射。AgentKit 有其他未提交改动，不能推定当前 App 等于某个干净 UI 提交。收尾源码范围已由本次授权提交固定，最终发布仍须明确实际安装包构建输入；本批不重构历史提交或覆盖运行中的二进制。

## 尚未完成的门槛

1. 本提交固定测试修正和证据；最终可复现安装包来源仍待固定，保留独立构建输入与实际二进制 hash。manifest 的 release_commit=null 指最终发布尚未定稿，不是收尾源码缺少提交。
2. 已完成：Talkify/内嵌 Daemon 停止时普通聊天仍可用，以及启动重新授权后不重启 Codex、恢复原空 Session。网络连接超时/黑洞故障未在本轮覆盖。
3. 在实际桌面补充执行中/待审批 Session、带新持久验证约定的原生 Handoff 和 MCP 重启恢复。独立进程离线结果已经通过，但不能替代 GUI 证据；未知副作用和审批不自动重放。
4. 单独授权一个新版真实异步小任务：原生 Send 后结束 Codex 回复，执行期间主动轮询为 0，Talkify 执行/审批，用户返回后一次 Handoff 进入审查，最终验证和交付完整。实际额度未知就保留 unknown。

不为验收改写 TimelineKit 已交付代码，不清原 Store、不修改 recovery_required，不扩展 Managed/G1/G3、Adapter 或 Task UI。提交、影响用户执行的停机/重载及新第三方模型调用分别确认。

## 回滚

本批没有数据库或生产行为变更；回滚测试修正只影响校验代码，不改变持久历史。不自动执行 Git 回滚；如需回滚，以本次限定收尾提交为边界并另行确认。需要回退已加载 Bridge 时沿用原私有项目配置备份，保持 required=false，不恢复撤销凭据、不删除验收 Session、不重派 Turn。
