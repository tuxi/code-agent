# 异步委派与桌面只读恢复验收

2026-10-30，Asia/Shanghai。没有第三方模型调用、Canary、新生产 Turn、审批回答或取消请求；没有修改 TimelineKit 源码、Agent Loop、Flux 或新增 MCP 工具。

## 默认使用约定

持久准入成功后，Codex 只向用户交代 Session/Turn/request/cursor、Talkify 观察入口和回来审查的方式，然后结束当前回复。执行期间不主动 wait/read、不安排自动唤醒。用户看到 Talkify 完成后，在**原 Codex 项目对话**恢复，先取一次 Handoff 再按风险展开证据。缺回执仍先核对准入，unknown 不是未提交。

调整仅涉及既有 Server instructions 与 send/wait 工具描述。协议说明不能强制上层 LLM 停止推理，也没有增加自动通知 Codex 的调度器；真实 Desktop 是否遵守零轮询仍需实际对话验收。Talkify 的 Session completed 也不等同于 Task Accepted 或系统已产生“等待审查”状态。

## 独立执行的实际离线证据

`TestDaemonContinuesAfterBridgeExitWithoutClientPolling` 使用真实 Daemon/TurnExecutor/SQLite/工具循环与标准 MCP 子进程，Provider 仅为本机脚本服务器：

1. 一次 send 获得持久 Receipt；脚本模型被测试屏障阻塞，尚不能完成。
2. 关闭 MCP 客户端，确认 Bridge 子进程已退出，然后才释放屏障。
3. Daemon 独立完成 read_file、`go version` 和 Turn；仅保留模拟 Talkify 的 WebSocket 推送观察器，未发送输入或审批。
4. 屏障释放至终态期间，HTTP `/events` 和 `/v1/activity` 查询计数为 **0**。
5. 启动新 Bridge，校验同一 Runtime/Store/Session/初始基线；恢复客户端只调用 **一次 get_handoff**，无 wait/read/lookup 调用。
6. 返回 completed / unknown / not_reviewed，并含真实命令退出码 0。unknown 明确保留了进程内验证约定丢失的事实，没有因命令成功升级成验收完成。

这是本机离线 SDK/子进程验证，不是实际 Codex Desktop 推理行为或真实模型质量验证，也不是几十分钟持续运行的时长实测。脚本 Provider 只执行固定无害工具，不提供 OS 隔离证明。

最终日志 `/private/tmp/codeagent-async-ux-tests.log`：三个定向包 39 项顶层测试通过；vet、Bridge 构建通过。新增 failed/人工等待回归保持：waiting_for_human 拒绝最终 Handoff；failed/cancelled 可返回诊断证据，不能成为验证完成或 Accepted。既有 incomplete validation 回归继续通过。

## TimelineKit 现有数据只读核对

`TestExistingSessionHandoffReadOnly` 为显式 opt-in 的 Go API 诊断，不是替代 Desktop 的 stdio 驱动；全程 send/approval/cancel=0。

| 项目 | 实际值 |
| --- | --- |
| Session | `20261030-052252-b8f78ce8` |
| 精确返工 Turn | `turn_18e33a6ab0de1858_45` |
| request | `repair-thumbnail-multiple-main-tracks-20261030` |
| 初始 baseline | `34e59c4e5bec71e3a33ff6ec6c8f36e35de1cdad` |
| 执行 / 验证 / 验收 | completed / unknown / not_reviewed |
| 变更与命令 | 8 个文件，9 条命令；Diff 未截断 |
| Diff 文本 SHA-256 | `369748d6e493f9cc0ad968ab3166102eb7035f0621e1337529b3c4de3bf7dd60` |
| 本次持久 cursor | `2641927` |

结果为 `/private/tmp/timelinekit-repair-handoff-async-review.json`，pretty JSON 6530 bytes；日志 `/private/tmp/timelinekit-handoff-readonly-test.log`。该大小不是 MCP 总传输字节或 Codex Token 消耗。

证据不足之处：原 Bridge 的验证约定未持久化，不能恢复成通过；多条 Swift/Xcode 命令带管道或复合表达式，单一退出码不能独立证明每个上游验证成功。另有一条记录退出码 -1。Diff hash 仅对应现有 Git Diff 文本，不是冻结树 hash，也不覆盖图片二进制的全部内容。这里不重新运行测试或替 TimelineKit 开发任务作最终验收。

## Talkify 实际 GUI

通过原生 UI 只读打开正在运行的 Debug-MacDirect Talkify，选中 `TimelineKit Thumbnail Implementation & Hashable Fixes`，窗口 Session 为上述 ID：

- 原委派、审查反馈及多主轨返工历史正常显示；输入区没有运行中停止按钮。
- 最新实际响应标签显示 `qwen3.8-flash`，最近 Turn 路由 `opencode-go/qwen3.8-flash`；下次发送默认模型另行显示，未修改。
- 返工报告仍列出 iPhone 横屏、iPad 全宽/分屏、失败占位等待办视觉检查。
- 没有发送消息、改模型、处理审批或停止其他运行中的 Session。

历史正文中的“测试通过”仍为模型报告，不能替代 Handoff 的真实事件证据或独立审查。本轮只验证已有终态历史显示，没有新制造审批，也没有验证系统通知。

## 部署与项目边界

已原子替换 `code-agent/build/codeagent-mcp` 构建产物；安装 SHA-256 为 `08f7abb1d4209920dc28ba48bb86f1656768a3eb33731986629c923ad71f2cf5`。原进程不因磁盘替换被中断，新进程重载后使用新版本。

该指纹记录桌面验收时安装的构建。收尾审查随后修正重复 call_id 的命令引用问题；最终冻结版本以收尾提交为准，使用前从该版本重建 Bridge，不把历史构建指纹当作最终提交的构建证明。

TimelineKit 原 checkout 与实际 worktree 的 `.codex/config.toml` 均保留 `[mcp_servers.code_agent]`、原 command/授权路径/工作区，只把 attach-empty-session 改为同一 Session 的 reattach-session，并明确增加上述原始 baseline。没有全局注册、授权扩展、凭据轮换或生产 Daemon 重启。

备份 `/private/tmp/codeagent-async-rollout-r_62v0x8` 保存旧二进制、两份原配置和 rollout.json；文件含路径与二进制指纹，不含 owner token。回退可恢复旧二进制；原版本也支持显式 reattach，应保留正确的非空 Session 绑定。不要直接恢复旧空绑定后宣称重连可用，不删除 Store 或源码改动。

ChatGPT 重启后，本 code-agent 对话已成功原生调用新版 connection_info：Runtime/Store 身份通过，但按本项目配置仍绑定旧实验 workspace `/private/tmp/codeagent-gui-workspace-__aaks0i/repo`、Session `20261027-165016-b3490cad`。这是项目范围隔离的预期结果；未通过此连接读取 TimelineKit，不自动扩大访问范围。

## TimelineKit 原生桌面恢复已通过

用户重启 ChatGPT 并在原对话「配置 Code-Agent 手工协作」发送只读恢复请求后，本轮只读核对了该对话的实际工具记录：

- 对话 ID `01a18b3b-a1fc-7bb3-81d2-ca604a188c8b`，恢复对话 Turn `01a18c6e-a8f9-7c32-8c39-d4013de608c3`，状态 completed。
- 实际 cwd 为 `/Users/xiaoyuan/.codex/worktrees/videoeditor-thumbnails-layout/TimelineKit`。
- 原生 server `code_agent` 调用 `codeagent_connection_info` 一次，完成，2 ms。
- 同一原生 server 调用 `codeagent_get_handoff` 一次，精确绑定 Session `20261030-052252-b8f78ce8` / Executor Turn `turn_18e33a6ab0de1858_45`，完成，859 ms。
- 本恢复轮次 send=0、wait=0、命令执行=0、文件修改=0；没有临时 stdio/PTY 驱动。两次工具调用后报告并结束回复。
- 用户返回的结果正确区分 completed / unknown / not_reviewed，保留管道命令、视觉矩阵和最终独立审查未完成的边界，没有推导验证通过或 Accepted。

本轮读取的是真实调用元数据并与用户提供的返回结果交叉核对；read_thread 未暴露原始 MCP 输出正文，因此没有为“再次证明”重复查询该 Handoff。原生两次调用共 861 ms、整轮 21.501 秒，不是额度或 Token 节省率。

验收结论：项目级原生工具接入、非空 Session 重连、一次 Handoff 恢复到有意义的审查入口、报告后结束本轮，均已获得真实 Desktop 证据。此只读恢复发生时，TimelineKit 功能开发本身仍未最终验收。

后续用户完成 UI 验收，原项目对话补齐无管道验证、最终审查并本地合并 `8def8a27`，已登记为 [Case 001](collaboration-cases/001-timelinekit.md)。这不改写早先 Handoff 的状态，也不代表该案例证明了异步执行或额度节省。

## 仍未实测的边界

真实 Codex 长任务期间零轮询、恢复后审查效率以及账户额度收益仍未实测。当前没有新的真实模型任务授权，不申请或执行模型 Canary。
