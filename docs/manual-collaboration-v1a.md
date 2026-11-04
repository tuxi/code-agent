# V1-a：接入可靠性

日期：2026-11-03。实现批次，不代表 Manual Collaboration V1 上线验收完成。

## 本批行为

- 项目配置生成器明确输出 `enabled = true` 和 `required = false`。协作工具失败不应阻断普通 Codex 对话；设置语义由 Codex 客户端保证。
- 继续使用标准 MCP SDK。业务失败通过 `CallToolResult.isError` 和 JSON 文本内容返回；没有新握手协议、错误轮询器或调度器。成功结果和既有工具名称不变。
- Bridge 可识别的安全/准入错误具有稳定 code、静态安全 message、`retryable`、`next_action` 及已知精确引用；不反射底层异常、HTTP 正文或凭据。
- `retryable=false` 表示不得自动重放原操作，不表示用户完成 next_action 后永远不能再次读取。它尤其不能作为“换一个 request_id 再发送”的理由。
- 发送已写入 WebSocket 后，读取准入证据失败也返回 `admission_unknown`，不能误报成普通连接失败而暗示未提交。被未知旧请求阻止的新发送返回旧 request_id 供核对。
- 既有 `RuntimeAPIError` 保留 code、http_status、trace_id 和 Go cause，增量增加 retryable/next_action。已知恢复/受管错误保持原业务类别。

## 错误契约

```json
{
  "error": {
    "code": "admission_unknown",
    "message": "Execution admission is uncertain; work may already have been submitted.",
    "retryable": false,
    "next_action": "Use codeagent_lookup_admission with the original Session and request_id. Never allocate a new request_id; unknown is not unsubmitted. After restart reconcile durable history; do not blindly retry.",
    "session_id": "session_example",
    "request_id": "request_example"
  }
}
```

这是 MCP 工具错误文本中的 JSON，不要求客户端支持新协议。初始化前发生的错误仍输出到 stderr，可能带已有阶段前缀；stdout 只供 MCP 使用。

| code | 含义与处理 |
| --- | --- |
| grant_invalid | 私有凭据缺失、格式或文件权限无效；核对并由用户授权 |
| grant_expired | 授权过期；用户恢复原范围授权后重连 |
| grant_replaced | 授权被撤销/替换；不自动跟随新身份 |
| connection_closed | 原 Bridge 已关闭；重连不会重放执行或恢复审批 |
| daemon_stopped | 授权记载的进程不可用；核对当前 Talkify 实例 |
| runtime_unreachable | Runtime 连接失败；发送回执不明时先核对原请求 |
| identity_mismatch | Runtime/Store 不匹配；停止，不自动迁移 |
| workspace_mismatch | 工作区/Git 身份或 Session 策略不匹配；停止 |
| baseline_changed | HEAD 不再是原执行基线；保留旧事实，新任务使用新绑定 |
| session_mismatch | Session 不属于当前绑定；核对，不自动新建 |
| session_busy | 有活跃执行或人工控制；观察原任务，不并行派发 |
| turn_not_terminal | 尚无最终产物；读取原 Turn 的状态/attention，由用户审批 |
| admission_unknown | 可能已提交；按原 request_id 核对，不盲目重发 |
| create_unknown | Session 创建回执不明；先核对现有 Session，不能假定没创建 |
| request_conflict | 相同 request_id 的载荷发生变化；拒绝 |
| already_admitted | 重连后发现原请求已准入；读取原 Turn |
| evidence_incomplete | 持久证据不完整；不推断完成或通过 |
| invalid_tool_arguments | Bridge 识别的参数错误；修正参数，不改已准入请求 |

SDK schema 校验、其他遗留参数/产物错误仍使用 SDK 原有工具错误格式；本批不宣称所有内部错误均已完成结构化迁移。待审批的正常读取结果继续使用原有非终态和 attention，不新增审批接口。

## 已有配置迁移

1. 检查实际项目及其 worktree 的 `.codex/config.toml`，保留现有配置并先做本机备份。
2. 找到原 server 名称（可能是 `code_agent` 或 `codeagent_gui`）。替换同一表内的 `required = true` 为 `required = false`；不存在时显式添加。不能追加重复键，也不能新建另一个可选条目却留下旧必需条目。
3. 已交付任务的旧绑定保持 `enabled = false`。不要为恢复聊天而重新授权、改旧基线、清库或重新发送已完成任务。
4. 当前仍需要协作的有效绑定可保持 `enabled = true`，由用户在客户端重连或重新打开项目。恢复后先核对 connection_info，再考虑委派。
5. 下游不可用时应能发送普通消息；需要任务时再检查 Talkify 和授权。不得将 MCP 失效解释为允许绕过审批。

生成器只打印可审阅 TOML，不写用户配置、不扩大注册范围、不改生产 grant。没有自动迁移。TimelineKit 已停用旧绑定并由用户确认恢复普通消息；“启用但可选 + 故障”的真实桌面矩阵仍待 V1-d 验收。

## 验证与剩余范围

回归覆盖私有 grant 的过期/进程停止/替换/关闭区别、原因脱敏、原生 MCP 业务错误不终止连接、写入后准入证据失败、原请求核对、载荷冲突以及默认可选配置。兼容性继续运行既有 Bridge/CLI 测试。

实际结果：当前工作区和仅包含本批文件的独立临时快照均通过 Bridge/CLI 两包的 57 项顶层测试（`go test -race`，0 失败）；快照 `go vet` 和 CLI 构建通过。候选二进制的配置输出实测包含 `required = false`，使用不存在的 grant 也能离线生成。快照排除了工作区既有未提交的工具/权限修改及 G1/G3 草案。测试日志：`/private/tmp/codeagent-manual-v1a-isolated-tests.jsonl`；候选包：`/private/tmp/codeagent-manual-v1a-candidate`，未替换已安装包。

本批不自动部署或提交，不调用生产 Daemon/第三方模型。现有固定 Session CLI 仍在 initialize 前校验授权和工作区，因此授权失效仍会导致该 MCP 进程退出；普通聊天可用性依赖客户端可选设置。启动与下游可用性进一步解耦、原范围授权恢复后无需重写配置属于 V1-b。

回滚仅恢复本批代码或已备份的单项配置，不删除 Session/SQLite/事件。故障配置回滚时不得重新引入阻塞普通对话的 `required = true`。R5 验证约定持久化、全量错误归一及生产发布门槛尚未完成。
