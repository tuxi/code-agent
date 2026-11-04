# Collaboration UX P0

2026-10-30。范围为 Manual Collaboration 的接入、安静等待与执行证据交接。不修改 Agent Loop、Flux、模型生成参数、受管准入或 AgentKit UI。

## TimelineKit 复盘与接入修正

历史对话「配置 Code-Agent 手工协作」显示：项目根与 worktree 配置错位，原生工具未加载后改用临时 stdio，遇到 PTY 行截断、stdin 关闭和重复状态回放。Codex 最终补运行测试/构建并发现多主轨封面问题，但最后一个对话 Turn 因账户额度结束；本批不宣称该开发任务已验收。

本轮只读检查发现：原 checkout 和实际 worktree 都使用 `[mcp_servers.code_agent]`，保留 `--attach-empty-session 20261030-052252-b8f78ce8`。该入口只允许空 Session；真实开发完成后不能靠反复重启恢复它。应保留原 Session，使用其已知初始基线显式 reattach：

```toml
# 合并到当前实际 worktree 的 .codex/config.toml。
# 保留现有 server 名称、command、其他配置；只替换旧 attach 参数。
"--reattach-session", "20261030-052252-b8f78ce8",
"--baseline-commit", "34e59c4e5bec71e3a33ff6ec6c8f36e35de1cdad",
```

不能在重连时以当前脏目录推导新基线，不清理、reset 或绕过身份检查。显式 reattach 同样可以观察身份匹配的空 Session，因此在初次创建并记录基线后，即可用于后续固定配置；不必长期保留 attach-empty 模式。

本轮通过新二进制执行 `--check-connection`：原 Runtime/Store 身份、Session、worktree 与上述基线均核对成功。只读取历史并打开观察连接，没有发送输入、审批或取消；**不等于当前 TimelineKit 对话已加载原生工具**。未修改该项目配置或替换它正在使用的 Bridge。

接入依据：[官方 MCP 配置](https://developers.openai.com/codex/mcp)。项目配置只用于可信项目；CLI 检查与 Desktop 真实工具发现是两项验收。

## 新项目一次接入操作

1. 准备干净的独立 Git 工作区并记录完整 HEAD。临时路径或 Git worktree 不构成 OS 隔离声明。
2. 通过 `--print-project-config --workspace /绝对工作区 --connection-manifest /已有私有授权文件` 输出配置草案。生成器不读凭据、不联网、不写配置或 Git exclude。
3. 将条目合并到**实际工作区**的 `.codex/config.toml`。保留原设置、相同连接只用一个条目，不全局注册。若 `.codex/` 影响 clean 检查，先明确处理配置的本地管理方式，不删除用户文件或自动提交。
4. 新 Session 创建并记录 ID 后，使用明确的 Session ID 和原始 baseline 生成 reattach 配置。没有可验证的基线时停止，不猜测。
5. 用相同参数加 `--check-connection` 确认身份；它现在明确返回 workspace、baseline，以及 `native_tool_discovery=not_tested`。
6. 在实际项目对话调用 `codeagent_connection_info`，核对 workspace/Session/Runtime/Store；只有成功才允许派发。如果工具缺失，核对项目根、项目信任、server 名称、二进制存在、启动错误及空/非空绑定模式，然后再重载连接。不以 CLI list 或直接 stdio 成功代替原生工具发现；不临时创建 PTY 驱动。

本版本不自动编辑用户配置、轮换 owner grant、创建 Session 或启动 Daemon。配置输出只是审阅入口；无法从 Bridge 内部修正 Desktop 的项目信任或热加载状态。

### MCP 故障不阻塞普通对话

2026-11-03：TimelineKit 的旧手工配置使用 `required = true`，临时授权过期且原 Daemon 退出后，Bridge 在 initialize 前安全退出，Codex 因必需服务未就绪而拒绝创建对话。生成器现在明确输出 `required = false`，服务仍启用，但失败只影响协作工具。该行为使用 Codex 原生可选 MCP 配置，不引入自定义握手或自动恢复协议。

合并配置时，必须替换同一 server 表内原有的 `required = true`，不能重复追加同名键，也不能新增一个不同名称的条目后留下原必需条目。生成器不会自动迁移已有配置。其他 MCP 客户端应使用其原生可选服务设置；`required` 是 Codex 客户端设置，不属于 MCP 协议。

授权过期时，由用户在 Talkify 重新授权后重连；工作区或原基线改变时，停止委派并核对任务绑定。不能自动刷新 owner 授权、修改旧基线或静默创建新 Session。任务交付后设置 `enabled = false` 停用该任务绑定；新任务使用新的 Session/基线。若只需普通 Codex 对话，无需启动 Talkify 或恢复授权。

此修正不使 Bridge 在无效授权下继续提供执行工具：启动仍执行原有身份校验。客户端可选服务语义负责让普通对话继续；委派前仍须成功调用 `codeagent_connection_info`。CLI 回归验证配置策略，真实客户端恢复结果以桌面验收为准。TimelineKit 的两个旧配置已停用，用户确认普通消息恢复；新生成配置下服务故障的桌面验收尚未重复执行。

## 默认异步委派与按需等待

```text
connection_info
→ send_to_session(request_id 固定、任务范围、验收命令、development.validation_commands)
→ 将持久 Receipt 告知用户，结束当前 Codex 回复，不主动轮询或安排自动唤醒
→ 用户在 Talkify 观察执行并处理审批
→ 用户回到原 Codex 对话，指定原 Session/Turn 恢复审查
→ 一次 get_handoff
→ 仅对需要核对的证据调用 get_artifacts / read_session / read_task
→ Codex 独立审查，发现真实问题才向同一 Session 派发新 request_id 的返工
```

只有用户明确要求等待时才调用 `wait_for_update`，传 timeout_seconds=30 并沿用返回 cursor。MCP 指令是协作使用约定，不能强制 Desktop 模型结束推理；真实客户端是否遵守须单独验收。异步离线验证与本机部署进展见 [异步桌面验收](collaboration-async-desktop-acceptance.md)。

`codeagent_wait_for_update` 不返回模型正文、工具参数、日志或历史 Events。未变化返回小结果；文本/模型 delta 只推进持久 cursor，工具结果和生命周期是可见里程碑。终态即使位于 cursor 之前仍可见。审批保持非终态，并返回 Session/Turn/Control 引用；实时具体 Control 丢失时仍保留 Runtime attention，不能伪造审批引用。

该接口会在 Bridge 内部读取持久历史重建状态，未引入第二套持久投影或缓存恢复系统。它减少模型收到的内容，不承诺降低 Daemon HTTP 请求数；历史超过现有完整回放限制时仍明确失败，不能默默忽略旧事件。

发送超时或没有准入回执时，调用 `codeagent_lookup_admission(session_id, request_id)`：

- `admitted`：得到精确 Receipt，继续观察原 Turn，不能重发。
- `unknown`：不是“未提交”。原 Bridge 进程内只允许原 request_id 和完全相同 payload 幂等重试；重启后先核对持久历史。没有凭据或历史不完整时明确报错，不换新 request_id 冒险派发。

旧 wait/read/artifacts 接口保留原字段和游标语义。Artifacts 的命令记录新增可选 `source_seq`，直接保留各条原始事件的位置，避免重复 call_id 将引用串到后一条命令。Read 从同一份完整持久事件观察计算状态、增量和游标，修复两次 HTTP 读取间终态到达导致的旧游标问题；原生 MCP 新工具版本为 0.3.0-experimental。

## Handoff 的可信范围

`codeagent_get_handoff` 由 Bridge 确定性地读取 Daemon 的持久事件、既有 executionevidence 检查和真实 Git 数据生成，不使用模型总结。没有新增 Daemon Handoff API 或第二套 Task 协议。

| 字段 | 来源与边界 |
| --- | --- |
| execution_status | 精确 Turn 的持久生命周期事件，不等同于测试通过 |
| validation_status | `complete` 表示已声明命令在最后可观测修改之后有成功记录；缺失/失败/取消为 partial，未声明或重连丢失约定为 unknown |
| acceptance_status | Manual 工作区证据保持 not_reviewed，不由 Turn 完成或模型自述推导 PASS |
| session_id / turn_id / request_id / cursor | 原始持久事件引用；不使用 latest Turn 猜测 |
| attempt_id | 默认 null；可选 task_id 通过已有 scoped Task Snapshot 取得精确匹配；不创建或伪造关联 |
| task_revision / task_acceptance | 显式 Task 的权威 Snapshot；独立于本次可变工作区 hash 的审查结论 |
| changed_files / baseline_commit / diff_sha256 | 当前 Git 工作区与原基线；包含 untracked，所有截断明确标记 |
| commands | 实际命令、状态、退出码、耗时、策略分类及 Session/Turn/source_seq/call_id；不回传日志正文 |
| validation | 缺失检查、未完成调用、未解决参数错误；审批 confirm 不等于执行成功 |

文件列表最多 200 项、命令索引最多 64 项，超限有标记；不把缺失退出码当 0。采集期间共享同一个 Bridge 发送锁，并二次检查 Diff，发现改变则拒绝交接。外部写入者不受该锁约束，**此处仍非不可变、非每 Turn 冻结产物**；需要冻结审查时沿用已有 managed ReviewBundle，而不是给手工模式伪造冻结版本。

命令验证只覆盖可观测 Turn 事件，不能证明外部没有改文件、视觉已通过或测试本身足够。设备/人工检查须在委派文本中明确列为未完成项，由审查方核验；本索引不解析模型正文来伪造视觉 PASS。validation expectations 仍为原 Bridge 进程内元数据，重启后未恢复就返回 unknown，不偷偷重新解释历史。

## 验证与效果对照

本批新增测试覆盖：

- 长正文不进入 quiet response、无变化超时、终态游标重读、审批非终态、越界 Session 和取消。
- 准入未观察到返回 unknown、已准入精确定位、重复准入歧义拒绝、查询不重复派发。
- completed / partial / unknown / cancelled 分离；失败命令、最后修改后缺失验证、模型声称通过均不能伪造验收。
- 精确 Task/Attempt/Store 关联及重复/错误关联拒绝。
- 真 Daemon + SQLite + 工具循环 + 标准 MCP 子进程：原始 Diff、command source_seq、quiet wait、准入核对及 Handoff。
- 配置生成使用实际工作区、保留原基线，不读 grant 或写文件。

合成负载基线：同一事件流的 Read JSON 约 190 KB、quiet update 108 B；含 90 KB 命令日志的 Artifacts 约 91 KB、Handoff 约 1.2 KB。短结果可能因新增状态字段更大。数字是结构化 JSON 字节，**不是 MCP 总传输字节、Token 或 Codex 账户额度节省率**。

真实对照尚未执行，必须另行授权模型数据范围/预算。对照前固定同类任务规格、候选基线、验证命令、视觉矩阵、模型与返工上限；不得把 TimelineKit 的历史任务追认为当前新授权。

每次记录：接入人工步骤/重载次数、客户端主动 MCP 调用数、每次完整 CallToolResult JSON 字节及累计值、Codex 主动参与轮次、有效审查次数、Executor/Codex 补测次数、用户审批/干预次数、耗时、最终验收和缺陷。可获得的账户 usage 单列；未知保持 unknown，不推算每对话额度。`有效审查次数 / 总参与次数` 与验收率一起解释，小任务委派反而更贵时也应记录。

## 部署、回退与未完成验收

首批候选构建到 `/private/tmp/codeagent-mcp-ux-p0`。后续异步验收批已备份并安装新版 Bridge，修正 TimelineKit 的原 Session reattach 配置，未重启 Talkify/Daemon；具体指纹与回退依据见异步桌面验收文档。保持原 Session/基线，不重复 create/send。

回退仅恢复先前二进制和本项目配置；不删 SQLite、不 reset 工作区、不撤销用户历史。新接口为增量增加，旧客户端仍可使用原接口。回退也不能使非空 Session 合法使用 attach-empty。

首批离线测试日志：`/private/tmp/codeagent-collaboration-ux-p0-tests.log`。后续原生 Desktop 一次 Handoff 恢复已通过，见异步桌面验收记录；TimelineKit 同类真实任务对照及实际额度收益仍待验证，离线 SDK/stdio 验证不代替 Desktop 验收。

## 冻结范围与收尾审查

Collaboration UX P0 冻结为本次提交内容，MCP `0.3.0-experimental`：接入诊断、按需 quiet wait、准入核对、结构化 Handoff，以及准入后结束回复的默认异步使用约定。后续不继续增加 Flux、Managed Collaboration、Adapter 或 MCP 工具；真实任务和外发授权另行确认。

独立复查了旧工具输入/返回兼容性、Session/Runtime/Store/workspace 约束、未知准入不重复派发，以及 Handoff 对执行/验证/验收的区分。复查发现重复 call_id 会把前一命令的日志引用指到后一条事件，已改为直接保留每条命令的 source_seq；新增回归先复现错误（两条均指向 16），修复后要求分别指向 14、16，不放宽断言。

原生桌面一次 Handoff 恢复已通过；TimelineKit 随后完成交付，分别记录在[桌面验收](collaboration-async-desktop-acceptance.md)和[Case 001](collaboration-cases/001-timelinekit.md)。[下一次异步验收模板](collaboration-cases/next-async-acceptance.md)仅准备记录口径，尚未指定真实任务或授权模型。

明确保留的限制：验证约定在 Bridge 进程内，丢失时 unknown；手工产物为当前工作区而非冻结候选；历史回放有现有大小限制；MCP 指令不能强制上层 Agent 停止推理；真实长任务零轮询和账户额度节省仍未实测。提交后的新任务使用新 Session/新基线，不修改旧 Session 的原基线来继续执行。

G1/G3 草案、模型计费特征测试和独立 Mac 采集脚本不在本次提交范围。冻结验证使用仅含本批暂存内容的临时克隆快照，禁用所有 live 测试入口和依赖下载；结果以收尾报告为准，不以工作区其他未提交文件支撑构建。
