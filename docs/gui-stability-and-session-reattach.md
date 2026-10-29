# 双 GUI 基础体验修复与 Existing Session Reattach

2026-10-28。本批只修执行证据、AgentKit 展示和 MCP 连接恢复。没有修改 Agent Loop、模型生成参数、模型选择机制、Task/Flux 调度或数据库。没有发起第三方模型请求，没有推送或部署。

## 根因与最小修复

### 1. 已批准的命令被误标 needs_validation

`run_command.decision` 是策略分类，`confirm` 表示执行前需要确认，并不是用户拒绝。原检查器只接受 `allow`，漏掉已通过 Dispatcher 审批、实际完成的命令。

保留原始 decision，不把 confirm 改写为 allow。检查器要求：关联的 tool_started/tool_finished、正确命令、无工具错误或拒绝、前台执行、明确 completed/exit 0、执行发生在最后修改之后。只有这些真实执行证据完整时，confirm 分类的结果才可以满足验证要求。仅分类、缺退出码、超时、取消、非零退出、拒绝和错误均不能通过。legacy allow 的既有兼容行为保留。

代码：`internal/executionevidence/check.go`、`check_test.go`。

### 2. 模型标签混合了配置与执行身份

发现三个独立问题：

- `ConversationViewModel.handleEvent` 只用于实时事件，首次 HTTP 历史导入不走该方法。
- Composer 从本地状态恢复自己的模型选择，原先仅在 conversation ID 变化时同步 ViewModel；不能把 ViewModel 改值等同于下拉框已经改值。
- 真实 model_request 的 model 是 Provider 内部 wire ID `openrouter/free`，而 model_finished 审计的 resolved_model 是 `openrouter/openrouter/free`。后续失败调用前的短 ID 不能覆盖同 Turn 已确认的完整路由。

新增独立的 `ConversationModelIdentity`，历史和实时都更新观察状态。Wire 解码保留 model_selection，最近成功响应的 actual_model 来自 model_finished，并记录 Turn/invocation 引用。失败/取消调用不会把最近成功实际响应抹掉。后续同 Turn 的 legacy 请求只作为缺失审计时的 fallback，不覆盖已确认路由；新 Turn 可以重新解析。

不再通过模型事件写回 selectedModel 或持久配置。输入区单独说明：下方选择用于**下次发送**，同时展示最近 Turn 路由和最近实际响应。原来的 bai/deepseek-flash 可以继续作为输入选择存在，但不再被用来代表已发生的 OpenRouter 调用。最近实际响应也不是 Session 的固定模型配置。

代码在相邻 AgentKit 的 `ConversationModelIdentity.swift`、`WireFrame.swift`、`AgentEvent.swift`、`ConversationViewModel.swift` 和 `ConversationDetailView.swift`。modelFinished 增加带默认 nil 的 selection；内部枚举模式匹配相应适配，旧事件无审计字段仍可解码。

### 3. 非空 Session 安全重连

原 Bridge 只有进程内所有权，启动要求干净工作区；attach-empty-session 明确拒绝任何历史。新入口与它分开：

```text
codeagent-mcp --experimental-p0a \
  --connection-manifest <已授权的私有连接文件路径> \
  --workspace <原隔离仓库绝对路径> \
  --reattach-session <明确的原 Session ID> \
  --baseline-commit <先前保留的完整 HEAD>
```

约束：

- 仅使用原有 owner grant；保留私有文件所有权/权限、有效期、进程存活、撤销/替换检测、loopback、TLS/主机名校验和禁止重定向。
- 校验 server_id、store_id、Session ID、canonical workspace 和 shared_workspace policy；拒绝 archived、fork、needs_rebind 等不同语义。
- 显式 baseline 必须等于当前 HEAD；保留已有脏修改，不能把它们自动作为新的干净基线。连接期间固定仓库目录和 Git 目录文件身份，拒绝替换/重定向。
- 从完整持久事件恢复 request_id → exact Turn → admission cursor；拒绝缺失/重复准入、异序或截断历史。不会根据模型文字或 Session 摘要猜测 Turn。
- 只重建观察通道，不创建 Conversation，不发送输入、resume、cancel 或审批。read/wait 可有界重连观察连接，连接失败时持久 HTTP 状态仍可读取；身份或授权错误直接拒绝。
- 新输入前重新检查身份、历史、完整活动快照和既有 Turn；不与运行中任务或待人工控制重叠。跨客户端检查不是全局执行租约，最终执行互斥仍由 Runtime 掌握。
- 已恢复的旧 request_id 不允许再次派发未知原始 payload。读取已准入 Turn，开始后续新工作时使用新 request_id。

边界：本次不恢复模型执行、审批决定或客户端工具执行；不自动跟随已撤销授权的替换文件。历史 development expectations 原为进程内数据，重连不伪造恢复，read/artifacts 明确返回 evidence_note；不能据缺失元数据宣布验证通过。超出当前响应/历史预算时拒绝，尚未实现任意长历史的分段 reattach。

旧 New/NewFromManifest 和 attach-empty-session 仍要求干净工作区；新行为只通过显式 reattach 入口开启。MCP 仍为原七个工具。本项目配置已经切换到明确的原 Session 和 baseline，没有全局注册或权限扩大。

代码：`internal/mcpbridge/reattach*.go`、`client.go`、`connection.go`、`artifacts.go`、CLI 与项目 MCP 配置。

### 4. Created 4 files 与取消原因

原 `ExecutionReducer.handleAutoApproved` 创建了带随机 ID、exit 0 的虚拟 toolCall。真实记录是 3 次 create_file 加 1 次 auto_approved，投影因而显示 4 files。

修复后只在唯一匹配的真实 running call 上标记审批元数据；不能唯一关联时保留审批系统元数据，不虚构执行成功或新增文件。真实 GUI 回放已经显示 Created 3 files。这里的分组数量仍是工具调用数，并不保证等于任何复杂任务最终 Git 的去重文件数。

原 turn_cancelled 只终止 running 节点，没有建立可见的终态节点；若取消发生时没有 running 工具，就看不到原因。现在持久取消事件形成幂等的可见提示，保留服务端原因，Web 展示不泄露内部 modelActivity 标签。没有把取消改写为失败，也没有补造最终答案。

代码在 AgentKit 的 `ExecutionReducer.swift` 与 `ConversationWebDocument.swift`。

## 测试与实际本机验证

| 检查 | 结果 |
| --- | --- |
| Go executionevidence + mcpbridge race | 通过；包含本机脚本 Provider 的真实 Runtime/工具/MCP stdio 集成，明确禁用第三方模型实验 |
| Go vet、Bridge 构建 | 通过 |
| Swift 最后定向范围：GUIStabilityTests、LifecycleProtocolTests、ExecutionReducerTests | 44 项通过 |
| Swift 扩展范围：再加入 MultiConversationTests、ConversationWebDocumentTests、ConversationHistoryPaginationTests | 124 项中 119 项通过，5 个测试出现 6 条失败断言 |
| 修改前独立源码副本复现上述 5 项 | 同样 5 项、6 条断言失败；没有修改原断言或回滚用户工作区 |
| Talkify MacDirect 最终构建 | 通过 |
| 同一实际 Daemon，两个不同 OS 进程的 stdio MCP 重连 | 通过；同 Session，两个 Turn 分别 completed/cancelled，cursor=2618959，无待审批、无重复派发 |
| 实际 Daemon 的 Bridge 自有 WSS 连接强制断开 → 公共 read 自动重连 | 通过；身份/游标/终态一致，不停止 Daemon、不影响 Talkify 连接、不发送任务或审批 |
| 用户重启 Talkify，重新进入原 Session | 最终模型路由、实际响应、3 文件计数、取消原因及两轮历史均已在真实 GUI 核对 |
| 用户仅重启 ChatGPT 后，本对话原生 MCP 重新读取原非空 Session | 通过；两轮为 completed/cancelled，terminal=true、stream_connected=true、pending_controls=[]；从 cursor=2618959 增量读取均无新事件 |

最终 GUI 可见：

```text
下次发送模型：使用下方模型选择
最近 Turn 路由：openrouter/openrouter/free
最近实际响应：nvidia/nemotron-3.5-lightning:free
Created 3 files
已取消 — user_requested
```

上下文窗口同时显示 openrouter/openrouter/free。未发送测试消息，实际生产 Session cursor 保持 2618959。

此处“网络断线”严格指本 Bridge 自有实际 WSS/TCP 连接被切断后重连；没有切断系统网络、模拟公网分区或主动中断 Talkify 自己的网络通道。Talkify App 重启及原历史恢复已实测，不能据此声称所有网络故障模式均已覆盖。

重新授权后，旧桌面 MCP 进程按设计失效；用户仅重启 ChatGPT 后，本对话原生 MCP 已成功使用新版 reattach 配置读取两轮终态。没有重复派发、没有新增模型请求。重载时应保持 Talkify 运行，避免再次轮换授权。证据：`desktop-native-mcp-final.json`。

### 已有失败登记

- ConversationWebDocumentTests.testBundledShellLoadsFromPrivateSchemeAndHandshakes：两个布尔断言。
- MultiConversationTests.testArchiveAndRestoreMoveRuntimeOwnedPartitionsAndPreserveWorktree。
- MultiConversationTests.testDraftCommitMigratesStateAndRetainsSubmittedSnapshotUntilAccepted。
- MultiConversationTests.testManagedWorktreeDraftSendsOptInIdempotentCreateRequest。
- MultiConversationTests.testManagedWorktreeStagesAttachmentsOnlyAfterFinalWorkspaceIsKnown。

这些在修改前副本中复现；本批不扩大为 Archive/Draft/Worktree/WebKit 基线治理。不能把本次测试范围说成全仓或全 Swift suite 通过。

## Diff、迁移与回滚

本批范围基于修改前文件副本计算，避免把仓库已有未提交实现混入新改动：

- `/private/tmp/codeagent-gui-stability-20261028/code-agent.patch`
- `/private/tmp/codeagent-gui-stability-20261028/agentkit.patch`
- 同目录的测试日志、真实重连 JSON、验证脚本和 before-code-agent / before-AgentKit 副本。

没有 SQLite schema 或数据迁移，也没有新增依赖。现有单 Agent 的 Loop、权限和模型机制不变；AgentKit 仅改变观察身份和事件呈现。

回滚须先审阅这些 scoped patch，逐文件确认没有后续用户改动后反向应用并重建；不可用 reset/checkout/clean 覆盖整个脏工作区。项目配置可从 reattach-session/baseline-commit 恢复为原 attach-empty-session，但当前会话已经非空，旧空会话模式会拒绝它，这是原本的安全边界。

P0.5 父 Task Timeline 和 P0-B2 自动编排仍未实施。
