# 下一次真实异步委派验收 · 待确认模板

状态：**Prepared / Not authorized / Not started**。真实任务、源码外发范围、模型与预算均须另行确认；本模板不授予模型调用、Canary、推送或部署权限。

## 启动前填写

| 项目 | 待填内容 |
| --- | --- |
| 任务/项目/验收标准 | pending |
| 冻结版本 | Collaboration UX P0，MCP `0.3.0-experimental`；启动时填写 code-agent commit |
| Git 工作区/完整初始 HEAD | pending；保留原工作区未提交文件 |
| Runtime/Store/新 Session | pending；不改写 TimelineKit 旧 Session 基线 |
| Executor 模型/Provider/必要数据范围 | pending；不沿用旧任务外发授权 |
| 请求、Token、费用、时长、返工上限 | pending；说明硬限制与观测限制，含失败/重试 |
| 必需单测/构建 | pending；每条前台命令独立执行，不能用管道尾部退出码替代原命令结果 |
| 设备/人工视觉检查 | pending；明确授权与不可自动验证项 |

选择自然有实际价值的小任务；20–30 分钟只是希望观察的执行长度，不人为拖延、不制造缺陷或返工。不新增调度器、MCP 工具、Adapter 或受管隔离能力。

## 固定流程

1. 原生 connection_info 核对 workspace/Session/Runtime/Store。无临时 stdio/PTY 驱动；新建 Session/配置所需人工步骤逐项记录。
2. Codex 检查与规划，向 Executor 明确文件范围、验证命令与未完成时的报告要求；`development.validation_commands` 与任务文本一致。模型内部参数由 Runtime 管理。
3. 用稳定 request_id 发送；拿到精确 Session/Turn/cursor 的持久 Receipt 后，将引用告知用户并**结束当前回复**。
4. 执行期 Codex 主动 wait/read/状态轮询目标为 **0**，不安排自动唤醒。用户在 Talkify 观察并处理审批。
5. 用户确认执行结束，回到原 Codex 对话。首次进入审查只调用一次精确 Turn 的 Handoff；若客户端重启，允许必要的只读绑定核验，另计接入开销。
6. Codex 按风险展开实际 Diff/必要日志/截图。区分独立复核与替 Executor 补做缺失验证，不重新实现任务。
7. 真实缺陷才触发同 Session 返工；使用新 request_id，保留旧 Turn/证据。每次返工仍在准入后结束回复。不为展示流程制造问题。
8. 最终代码审查、验证与用户验收分别记录；本地提交/合并须有明确授权。Manual 交付结论不冒充自动 ReviewGate Accepted。

回执不明时先 lookup_admission；unknown 不是未提交，不另造 request_id 重发。不确定副作用、持续失败、预算或权限不满足时停止并保留证据，不擅自放宽约束。

## 结果记录

| 指标 | 本次实际值/证据 |
| --- | --- |
| 接入人工步骤、授权/重载次数 | pending |
| send 次数、稳定 request_id 与 Receipt | pending |
| 执行期主动轮询次数 | pending；目标 0，依据原 Codex 对话工具记录 |
| 首次审查 Handoff 调用数 | pending；目标 1 |
| 主动 MCP 调用总数与完整返回 JSON 字节 | pending；按调用记录/可获得的原始输出计，不能将结构化子对象字节当总传输量 |
| Codex 主动参与轮次 | pending；按实际回复 Turn 计 |
| 有效审查轮次 | pending；基于真实证据给出 PASS 或具体缺陷返工的回复 Turn，不以“必须发现 Bug”计成功 |
| 有效审查轮次 / 总参与轮次 | unknown，取得同口径计数后计算 |
| Executor 应做但由 Codex 补做的验证 | pending，列原因与命令；不得改名为独立复核隐藏代劳 |
| 必要独立复核 | pending，列风险、命令/证据与结果 |
| 用户审批及其他干预 | pending，记录时间和原因；审批不由 Bridge 代答 |
| 检查/规划、Executor、审批等待、审查、返工、总耗时 | pending；记录起止点，不能只把模型响应时间当任务耗时 |
| Code-Agent Provider 调用/Token/费用 | unknown，按实际可获得证据记录，不使用目录估价冒充账单 |
| Codex 账户用量 | unknown，拿不到任务级精确数值则保留 unknown，不估算节省比例 |
| 测试、代码审查、UI/用户验收、最终交付 | pending，分别给出结论与限制 |

本轮仍不是与 Codex 单独执行的严格 A/B 对照。先证明异步交接能稳定交付，再在固定任务规格、环境、权限和验证标准下另行评审比较。
