# V1-d：桌面验收进行中

日期：2026-11-04。状态：原生工具加载、过期授权、身份核验及授权替换后的显式重连已通过；用户另行授权的一次性 Go API 只读诊断已核实实际 Daemon 能力声明。没有安装候选 Daemon，整体 V1 仍为 NO-GO；本轮没有新模型授权或调用，不发送 Turn。

## 本轮已验证

- Talkify 正在运行，其打包 Daemon 进程也在运行；只读取程序名/PID及二进制 build info，没有读取进程环境。实际 Daemon 的源码基线为 `ca92dc9a61cd97541604a54b4fd1c9ed932f1367` 且 modified=true，不能据此认定包含 C1/C2；必须由当前 capability 和二进制身份核验。
- 切换前项目配置指向旧固定 Session Bridge，二进制基线为 `9192a0b1619512d39a1a2491c2da771ee7da231c`，没有显式 profile 或 required=false；现已切换并加载下述 Manual 候选。
- 预检时本地 MCP 授权已于 `2026-10-31T19:31:45Z` 过期，记录的 PID 与正在运行的 Daemon 不同；先验证了过期拒绝，之后由用户恢复授权。报告不保存 Token。
- 实际 GUI 只读检查：当前服务器为“本地 CodeAgent 服务”，“协作任务”可打开并显示原三项桌面测试任务。未启动任何任务；详情点击时检测到用户同时操作，因此未继续改变窗口。这个结果只证明现有入口与列表可读，不是新 V1-d 的完成验收。

## 准入基线失败的调查与修正

`failingRequestCommands.SendMessageWithRequestID` 测试替身少了 effort 字符串参数，因而没有实现 Router 的 `RequestMessageTarget`；实际被路由到嵌入的旧 SendMessage，返回 nil，导致测试收不到预期拒绝。生产 Router 已有 generic request_failed 回报逻辑。

本轮仅修正测试替身签名并增加编译期接口断言，保留原“准入前拒绝/准入后使用 lifecycle”断言，未改生产 Router。独立候选快照运行 Router/验证约定/资产输入相关 `-race`，25 项顶层测试通过。Manual stdio 初始化、重绑及 Handoff 预检 9 项顶层 `-race` 通过，均为本机离线测试。

另外三项历史失败仍登记，不修改认证或 Provider 迎合测试：Provider 用例硬编码旧目录数量及旧模型首项；两项 pairing 用例对 bootstrap 字符串直接做 hash，而当前生产入口先按 base64url 解码再 hash 原始字节。它们需要专门校正/验证测试数据与当前协议，不可通过放松认证解决。本轮未改这些断言，也未宣称完整 Server 套件通过。

## 可审阅候选

目录：`build/manual-v1d-zjqqweum/`，由 C3 清单的独立源码快照及本轮测试替身修正构建，排除其他未提交 Runtime/工具/TUI 改动。

- `codeagent-mcp`：Manual 候选，已通过项目配置加载；未覆盖原 Bridge 二进制。
- `codeagentd`：arm64 native 候选，采用当前 Talkify 构建脚本的 external-link/native 参数。构建成功，ld 提示 PIE 下忽略 image_base；尚未在实际 App 内启动验收。
- `project-mcp.toml`：候选项目配置，Manual profile、enabled=true、required=false，绑定同目录全新 Git 测试工作区 `workspace`；不指向主源码工作区执行。本轮只核对连接和历史，不发送 Turn。该临时工作区不是 OS 隔离声明。
- `preflight.json`：候选 SHA256、测试计数和当前未部署状态。
- `project-config.before.toml`：原项目配置私有备份，0600；不在报告展开，不进入 Git。

候选目录在 /build 忽略范围内，未写入生产 Store、授权文件或已安装 App。文件哈希保存在 preflight.json。

## 项目 MCP 配置切换

用户已明确授权本次项目范围变更。已替换 `.codex/config.toml` 的同名 `codeagent_gui` 条目：Manual 候选二进制、隔离测试工作区、enabled=true、required=false；移除旧固定 Session/基线启动参数。其他解析后设置保持一致，未全局注册，未续授权或修改 Daemon。

变更前后 SHA256、路径和操作范围记录在候选目录 `config-switch.json`；原始文件另存 `project-config.approved-before.toml`，权限 0600。变更后重新解析 TOML，核对目标表与候选一致，候选二进制哈希仍匹配。

用户已重启 Codex 并回到当前聊天。当前工具清单实际暴露 11 个原生 codeagent 工具，未使用临时 stdio 驱动。连续两次原生 `codeagent_connection_info({})` 均返回 `isError=true`、`error.code=grant_expired`、`retryable=false`，并提示用户授权后核对原 Runtime/Store/workspace；第二次调用仍能返回相同结构化错误，连接没有因过期授权关闭。用户消息和本轮正常工具调用/回复未被 MCP 初始化阻断。未续授权，未发送 Turn，未调用模型。

该结果只覆盖当前过期授权场景，不代表 Runtime 离线、授权替换、有效授权身份核验或候选 Daemon 部署均已通过。

用户随后在 Talkify 恢复授权，并仅提供原连接文件路径。未读取或展示文件内容；原生 `codeagent_connection_info({})` 返回 `identity_verified=true`、`execution_bound=false`。Server 为 `srv_31ff12dc-6485-453d-a565-53da1fe86f03`，Store 为 `cstore_16a5c66010fa0840a975cb9ff13173e8`，与预检身份一致。工作区为候选目录下 `workspace`，HEAD 为 `ca92dc9a61cd97541604a54b4fd1c9ed932f1367`；没有创建或绑定 Session。该响应未暴露 capability 列表，不能据此声称当前 Daemon 已支持 durable_validation_contract_v1。

## 后续桌面步骤

1. 已完成：用户授权项目配置切换并重启 Codex；原生工具加载、连续过期授权工具错误及普通聊天可用已实际验证。
2. 用户恢复授权后的原生身份核验已完成；实际 capability 核验也已由下述单独授权的 Go API 诊断完成。续授权不代表允许任何模型任务或扩大工作区范围。
3. Daemon 替换前单独核对运行任务和一致备份，再确认可停止/重启 Talkify 的时机；不直接覆盖运行中的程序。安装候选后核对能力、旧 Session 与 Handoff。没有完成这些步骤前，不标记“部署完成”。
4. 新模型任务及外发范围另行授权；原 Store recovery_required、G1/G3 和 Managed 安全门槛保持不变。

本轮未提交或安装 Daemon。当前 Daemon 已发布所需能力，不应仅为读取能力而重复部署；剩余 Session/Handoff 和 V1-d 场景仍单独验收，不发送 Turn。

## Talkify 再次重启后的重连缺陷

用户重启 Talkify 并恢复授权后，当前旧 Bridge 原生查询返回结构化 `grant_replaced`，未阻断聊天。能力核验因此尚未完成。用户同意优先修复“无需每次重启 Codex”的显式重连路径。

修复及离线证据见 [显式重连修复](manual-collaboration-v1-reconnect.md)：扩展原 connection_info，不增加工具、Daemon 或数据库。独立快照 race 84 顶层通过、9 skip、0 失败，vet/build 通过。用户已明确批准并完成仅修改 command 的项目配置切换，原二进制保留，私有备份已保存；未替换运行中程序。等待重载新版 MCP 一次，再实际验证同一 MCP 连接内的授权重连，不能将离线通过记为桌面已完成。

后续实际验收已完成：用户加载新版并恢复授权，原生工具核实同一 Runtime/Store 和工作区。用户随后撤销再授权，保持 Codex/MCP 运行；普通查询明确拒绝 grant_replaced，显式 reconnect 成功，紧接着普通查询恢复且身份、HEAD 均不变。此场景未创建 Session、未发送 Turn、未调用模型，授权文件仅由 Bridge 使用，内容未展示。授权替换后的桌面重连子项通过；实际 capability 核验、Daemon 候选部署和其他 V1-d 场景仍待完成，整体 NO-GO 不变。

## 实际 Daemon 能力声明：只读核验通过

北京时间 2026-11-04 16:25，用户明确允许一次性 Go API 诊断复用 Bridge 内部的安全授权读取。诊断只加在候选临时源码快照中，没有修改主源码、MCP 配置或已安装 Daemon。先限制为原 Runtime/Store 和工作区，再通过现有 probeIdentity、request、verifyRuntimeIdentity/checkBinding 核对身份和授权；仅执行身份/能力 GET，没有 Session 或 WebSocket 执行操作。凭据内容未展示或另存。

这不是原生 MCP 能力展示验收：当前 connection_info 仍只透出身份。实际 HTTP 声明的限定字段如下：

| 来源 | 能力 | 实际值 |
| --- | --- | --- |
| runtime/capabilities | durable_validation_contract_v1 | true |
| runtime/capabilities | task_links_v1 / task_timeline_v1 | 均 true |
| runtime/capabilities | activity_snapshot_v1 / session_attention_snapshot_v1 / session_attention_delta_v1 | 均 true |
| runtime/capabilities | multi_session_execution_v1 / session_scoped_client_tools_v1 / workspace_execution_policy_v1 | 均 true |
| runtime/capabilities | managed_worktree_v1 / conversation_archive_v1 / history_pagination | 均 true |
| tasks/capabilities | task_links_v1 / task_timeline_v1 | 均已发布 |
| tasks/capabilities | task_managed_workflow_v1 / task_review_gate_v1 / task_executor_model_v1 | 均已发布 |
| tasks/capabilities | automatic_turn_replay | false |

实际 Runtime/Store 与此前原生核验记录一致。一次定向诊断测试通过，输出仅保留白名单字段；记录为 `build/manual-v1d-reconnect-nf8_odrv/runtime-capabilities-readonly.json`（0600），诊断源码副本为同目录 `authorized_capabilities_readonly_test.go`。本次发送 Turn=0、模型调用=0。

结论仅为当前 Daemon 的权威端点已发布这些能力；不是持久化准入/Handoff 的当前安装包端到端证明，不是 Managed 真模型验收，也不证明当前 Store 可以准入新 Run。managed_worktree_v1 是工作区能力声明，不是 OS 隔离证明；旧 Store recovery_required、G1/G3 和禁止自动重放约束不变。下一项应核对既有 Session 的安全重绑定及精确 Turn 的只读 Handoff，不为本轮诊断创建或重跑任务。

## TimelineKit 已交付 Session：限定只读恢复

用户单独授权后，仅在候选临时源码快照运行 Go API 诊断，目标固定为 Session `20261030-052252-b8f78ce8`、Turn `turn_18e33a6ab0de1858_45`、原工作区及基线 `34e59c4e5bec71e3a33ff6ec6c8f36e35de1cdad`。未切换当前项目的原生 MCP 工作区。

首次调用 Manual 执行绑定返回 `baseline_changed`，未能读取 Handoff；保留原诊断及失败结果，没有放宽生产检查或更改原基线。实际工作区 HEAD 已为用户此前确认交付的 `8def8a27da4b5b496d4172ff44e22f1d21c4e2ad`，不满足执行绑定的 HEAD==baseline 条件。这不是凭据重连失败，不能用修改 baseline 来规避。

随后使用已有 commit-range observer 路径，固定原基线到上述交付提交：先核对原 Runtime/Store 与授权代次，校验两个 commit 和祖先关系，确认只读模式，再读取同一 Session/Turn。该路径没有执行权限、不打开控制 WebSocket；与原 Manual 执行绑定分开，不声称已完成执行绑定的桌面恢复验收。

北京时间 2026-11-04 16:33:55 的一次 Handoff 调用结果：

- 精确 request_id 为 `repair-thumbnail-multiple-main-tracks-20261030`，cursor 为 `2641927`。
- execution_status=completed；validation_status=unknown；acceptance_status=not_reviewed。
- validation_source=not_declared、validation_contract_ref=null、attempt_id=null；没有从旧历史补造验证约定、Task 关联或 Accepted。
- evidence_source=commit_range；baseline 和 delivery 均为原记录；当前 HEAD 等于交付提交。8 项文件引用与此前交付范围对应（3 实现、2 测试、3 截图），不是逐 Turn 文件归属证明。
- diff_sha256=`21bb4e94b4e187871c9bfd631b05b1660b49d023228ae95591305706c10bde2f`；Diff、文件和命令索引均未截断。
- 9 项历史命令引用中，8 项 recorded completed / exit 0，1 项 status 缺失 / exit -1（source_seq=2641774）；原值保留。命令退出码不等于约定验证通过，evidence_complete=true 仅表示索引证据未截断，不能替代验证或验收。

只输出状态、文件名和事件引用，不输出源码、Diff 正文、命令正文、日志或凭据。没有重新运行测试、修改 TimelineKit、发送 Turn 或调用模型。此前人工 UI 确认、独立测试/构建及本地合并的交付结论不因旧 Turn 的 unknown/not_reviewed 被撤销；两者证据来源独立。

记录为 `build/manual-v1d-reconnect-nf8_odrv/timelinekit-delivery-handoff-readonly.json`（0600）；初始执行绑定诊断与后续 observer 诊断分别保留源码副本。限定只读 observer 验收通过；它是 Go API 结果，不是当前项目原生 MCP 的跨工作区绑定验收。当前仍保留整体 V1-d NO-GO 及未完成的发布条目，不自动扩展工具、部署或发起模型任务。

## 后续发布收尾更新

见 [发布收尾清单](manual-collaboration-v1-release-checklist.md) 及 [当前观测 manifest](contracts/manual-v1-release-readiness.json)。源码现已有 eae1c45 提交；此前“未提交/未部署”的段落是当时状态，本批没有创建该提交或更新 Daemon。

用户批准后校正原三项 Server 测试，Server 全包 race 215 顶层通过；另经批准校正预算测试的 JSON 包装混算，并修复测试替身 fakeEventStore 的并发访问，Conversation 92 顶层连续 3 轮 race 通过。没有修改生产认证、正文预算或 Agent Loop。保留失败前后日志，不将旧失败登记静默删除；当前新旧实际进程兼容 2 子场景复验通过。

用户另行批准创建空验收 Session `20261104-084452-3e9f2643`。实际原生 MCP 完成授权替换后的旧绑定拒绝、服务 reconnect 要求 Session 重验证、显式 replace_binding_id 得到新句柄、旧句柄退休、重复绑定复用新句柄。Codex/MCP 未重启，发送 Turn 和模型调用均为 0。只验证空 Session 元数据恢复，不替代执行中/待审批/新约定 Handoff 的桌面验收。整体 NO-GO 与剩余发布边界见最新清单。

用户明确同意停机演练后退出 Talkify，并在当前 Codex 对话通知。进程可执行文件名只读检查确认 App/内嵌 Daemon 均已停止，候选 Bridge 仍运行；原生普通/精确 binding 查询均安全返回 grant_invalid，当前对话及 MCP 通道仍可继续。停机不阻断聊天子项通过，恢复后原 Session 重绑定待核对。本次不等同于网络超时故障测试，未派发任务或调用模型。

恢复阶段已通过：用户启动 Talkify 并重新授权后，原生普通/旧 binding 查询均返回 grant_replaced；显式 reconnect 验证原 Runtime/Store/workspace，并要求旧 Session 重验证。使用原 Session `20261104-084452-3e9f2643`、原基线和 replace_binding_id 得到新 binding `ce4a677f-0268-49be-93da-99b9b0652aa3`；新句柄身份核验通过、旧句柄 binding_not_found。没有重新创建 Session、重启 Codex/MCP、发送 Turn 或调用模型。实际 App/Daemon 停止至恢复的空 Session 路径通过；执行中/待审批及其他剩余项见发布清单。
