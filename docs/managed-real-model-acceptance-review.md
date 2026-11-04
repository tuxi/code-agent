# 真实模型 Managed Collaboration：执行前评审草案

2026-10-29，Asia/Shanghai。**仅供评审，不是执行许可；当前 NO-GO。** 本轮只查询本机模型目录、公开官方文档和源码/版本信息，没有模型推理、部署、新账户、Run 准入或生产配置写入。

## 1. 冻结范围与基线

P0.5 核心只读展示阶段性完成，其结论不受下一次模型实验成败影响。不增加 Task UI、调度能力、恢复豁免或 Agent Adapter。手工与受管协作继续并存。

[跨仓库兼容 manifest](contracts/p05-compatibility-baseline.json) 固定三个仓库的完整 commit、四个阶段提交、Task SQLite schema 3、wire/JSON schema、capabilities、fixture hash 及测试结果。AgentKit 与 Xcode 使用的既有依赖锁不同，manifest 分别记录 revision，不能用“main 分支”或再次自动解析替代。二进制是否对应这些 commit 尚未建立证据，不能把用户正在运行的包直接视为该基线。

未来构建必须从这些 commit 导出干净源码，不复制当前 AgentKit/Talkify 未提交文件；固定两个 lock graph，禁用包自动更新，记录工具链、构建参数、可执行文件 SHA256/签名与 Runtime/Store 身份。测试目录、模型配置和身份凭据不纳入源码 manifest。Provider、配置或二进制变化需生成新实验记录，不追改 P0.5 验收历史。

## 2. 执行边界：两个方向都要验证

| 风险 | 必须证明的边界 |
| --- | --- |
| 未知旧 Job 干扰新实验 | 旧执行域不能写新 workspace、状态、程序、依赖或控制凭据，也不能通过新 Daemon/SSH/MCP 间接发起写入 |
| 新模型生成危险工具操作 | 文件工具与子进程只能访问许可路径；不能读取 API key/owner grant/原项目，不能任意联网、提权、后台脱离或修改 Runtime 状态 |

新 Store 是独立环境建立后的状态分离，不能作为环境隔离的证明。新 Run 不恢复、不导入、不忽略旧 Job，原 Store 的 `recovery_required`、历史及租约保持原状；不再向已知被拒绝的原任务提交启动请求。

候选路线按最低现成条件选择，不在本轮创建：

1. **若有另一台可信、可清空测试内容的 Mac，优先评估在其上运行完整实验栈。** 应用、Bridge、Daemon、控制凭据均留在该可信环境；不通过旧用户可读取的 SSH key/agent socket、owner token 或可写共享目录把写权限重新交回旧执行域。现有 Mac 上的产品环境不迁移。是否有这样的设备尚未确认。
2. **同机专用非管理员登录用户是备选，不默认实施。** 完整 GUI/MCP/Daemon 控制栈在该用户内，工具进程还须有出站文件/网络边界。只覆盖普通旧 UID 的威胁模型；不能补证历史 Job 未提权。用户之前不希望为 GUI 验收迁移账户，该决定保留；真实执行如选此路线，须重新评审范围。
3. **VM 是另一备选，不引入容器平台。** 同样要保护宿主上的磁盘、配置和控制面，禁用旧域可写共享挂载；仅把代码放在同 UID 持有的 VM 中不够。不存在已验证实例时，不为赶验收搭一套平台。

当前 MCP Bridge 只接受 loopback endpoint，并核对本机 daemon PID。若未来使用另一台主机，应让既有 Bridge 与 Daemon 同处该主机，走其本机真实 grant；不能把隧道 PID 伪装成 daemon PID 或放宽 parseEndpoint。保留同一旧用户桌面远程控制需要另行解决控制凭据边界，不是现成零改动方案。

### 执行前必须完成的负向测试

- 在明确威胁模型下，从旧域验证新 workspace/祖先路径、凭据、程序和加载依赖无法读取/替换/写入；检查 UID/GID、ACL、共享目录、symlink/hardlink、继承 FD 和控制入口。
- 从实际文件工具/子进程验证 `..`、绝对越界、链接逃逸、密钥目录、状态数据库、原仓库、网络、后台执行和提权均被拒绝。审批是授权流程，不能代替 OS/执行边界。
- 只允许模型访问新任务及工具所需数据。Provider key 仅由可信 Provider 客户端持有；工具进程环境不得携带它或 owner token。模型提示词中的“不要读取密钥”不能充当安全证明。
- 验证命令和模型生成的测试也在受限环境中运行。Codex 的“独立测试”不等于直接在当前主工作区执行不可信测试代码。
- 任何拒绝/未知状态均保留证据并停止；没有这些负向结果，不准入真实模型。当前没有证明现有部署满足以上要求，也没有授权为达标修改其权限。

## 3. 模型与 Provider 核实

本轮真实 MCP 目录只读结果：

| 项目 | 已有证据与边界 |
| --- | --- |
| requested/resolved 目标 | `opencode-go/qwen3.8-flash`；未来 Run 显式选择，固定到返工，不修改 Runtime 默认模型 |
| wire ID / 最近 actual | 均为 `qwen3.8-flash`；同名别名不保证 Provider 永久固定底层权重版本 |
| configured / available | 均 true，只证明当前目录和凭据条件，不是本次推理成功 |
| verified | true；process scope、24 小时缓存，verified_at=`2026-10-29T10:48:29.603722Z`（北京时间 18:48:29），不是本轮调用，也不能归因为 Managed 实验成功 |
| 配置指纹 | `ebb20d7d095b1e3c6565275bb1dca05733e463c5fc3eecfe16fe53a3794b75e9`；执行前重新核对，不能用它证明凭据未轮换 |
| Tool Calling | 目录 supports_tools=true；仍需本次授权后的最小 canary 验证实际参数、工具结果续接和 usage |
| Runtime 配置 | 目录 reasoning_effort=medium、context_window=1,000,000；只记录，不由 Codex 修改。上下文容量不是 max output |
| 本机用户层 Provider base | `https://opencode.ai/inference/go/openai/v1`；文件只读抽取公开 endpoint。用户层不是完整 live merged 配置，执行前需核对实际协议与配置指纹 |

OpenCode 官方 Go 文档列出目标模型及对应 native Messages endpoint，同时说明可翻译其他文本 API。保留当前 OpenAI-compatible 路径作为待验证候选，不因文档 native 路径不同就改生产配置；canary 失败不能静默改模型或协议。Qwen 官方资料声明支持工具调用；这不等于本机适配器已经通过实际验证。来源：[OpenCode Go](https://opencode.ai/v2/docs/console/go/)、[Qwen 模型说明](https://docs.qwencloud.com/developer-guides/getting-started/latest-model)。

计费参考快照：Go 文档列出每百万 token 输入 $0.15、输出 $0.47、缓存读 $0.016、缓存写 $0.20；该数字是公开价，不是已核实的用户账单。Extra Usage 可切余额，需用户核对关闭，不在本轮访问或修改其账户。来源：[Go 计费与额度](https://opencode.ai/v2/docs/console/go/)。

官方 inference 文档区分余额扣款 `cost`、标价 `opencode.list_cost`、Go 额度 `opencode.allowance`、`billing_source` 与缓存/推理 token。Runtime 当前通用 Usage 只有 prompt/completion/total、cached prompt 和 billing units，不能假设已保存这些 Provider 扩展。实验取证必须保留可获得的原始计费字段；缺失时写 unknown，不能将订阅扣款 0 当作资源成本 0。来源：[OpenCode Inference API](https://opencode.ai/v2/docs/console/api/inference)。

授权后 canary 使用新实验 Session、同一模型和少量公开测试数据，验证一次实际 ToolCall/ToolResult 续接；计入整体请求/Token/时长预算。不得发送旧 Session 历史、项目源码、密钥或环境值。失败立即停止并归因，不作为自动换模型的理由。

## 4. 小型真实编码任务

在新 Git repo 实现 Python 标准库 `parse_duration_ms(text: str) -> int`，仅新增 `duration_smoke/duration.py` 和 `duration_smoke/test_duration.py`。仓库由公开合成需求创建，不 clone 三个产品仓库，不复用原 GUI Session。模型可写两文件，Reviewer 对冻结副本只读审查。

验收规格提前固定：

- ASCII 非负整数搭配 `h`、`m`、`s`、`ms`，组件按单位从大到小且不可重复；支持 `90s`、`1h 2m 3s 4ms`、`2m30s`。
- 数字和单位之间不能有空白；组件间、首尾允许 Unicode 空白。只接受 ASCII 数字，拒绝全角数字、符号、浮点、未知单位、重复/逆序、尾随垃圾与空字符串。
- 总计范围 0–86,400,000 ms，输入最多 128 字符，越界 ValueError，非 str 为 TypeError；返回 int，行为确定。
- 测试覆盖普通、边界和无效输入，不只检查模型自编测试。测试和 `git diff --check` 必须发生在最后修改之后。

固定命令候选：`/usr/bin/python3 -I -B -m unittest discover -s duration_smoke -p test_duration.py -v`、`git diff --check`。实际环境需先核对解释器与 Git 的可信路径和版本，不能把旧用户可改写的 Homebrew 路径视为可信。独立验收用例在执行前固定版本/hash，模型执行期间不改验收标准；它们不作为模型修改目标。

## 5. 流程与通过标准

1. 离线确认环境边界、基线、Provider 出口、工具白名单和预算准入；本步不推理。
2. Codex reported Planner 提交完整计划、范围和验收条件；Code-Agent managed Executor 绑定新本地 Session；reported Reviewer 绑定本次 Codex 会话。Daemon/Flux 为唯一受管调度者。
3. 最小 canary 通过后启动唯一 Managed Run，显式 executor_model；不夹带 Thinking、max_output_tokens 或保护路径覆盖。
4. Executor 完成 → 精确 Turn 证据 → 冻结 bundle。Codex 检查原始 bytes/hash、实际 Diff、模型工具与验证日志，并在受限环境独立执行测试。
5. 有真实缺陷才提交绑定当前 bundle 的 request_changes；最多一次返工，同 Session 新 Attempt。若首轮合格直接 PASS，不为了演示虚构问题。工具超时、待审批、预算耗尽或未知副作用不冒充编码缺陷。
6. ReviewGate 验证审查/产物/证据，Finalize 验证候选仍匹配，权威 Snapshot 为 accepted 后才通过。Flux succeeded、Turn completed、Review recorded 或 PASS 均不能单独验收。
7. Talkify 同时核对 Executor Session 的真实消息/工具/审批与父 Task 的实际状态。外部 Codex 仍 reported，系统 Gate/Finalize 不伪装成 Agent。

成功要求：规格与独立测试通过、许可文件外无修改、全部来源/版本匹配、验证证据完整、无未决审批/失控子进程、最终 accepted。任何阶段失败均保留原 Run/Turn/receipt，不另开请求绕过。首轮直接通过也算闭环成功；“真实返工能力”若未触发，标记本次未覆盖。

## 6. 拟议资源预算与实际缺口

以下数值仅供审批，不是已有授权：

| 项目 | 候选上限 |
| --- | --- |
| 实际外发 HTTP 模型请求 | 12 次，含 canary、所有自动重试及失败请求；不只数 model_finished |
| 累计输入 / 输出 | 200,000 / 16,000 token；输出包括可获取的 reasoning，缓存明细分列，不重复相加 |
| 执行轮次 | 最多 2 次执行，即首轮 + 1 次真实返工 |
| 费用 | 目标标价/额度占用不超过 US$0.50；余额扣款须单独核对，不购买/充值，不自动启用 Extra Usage |
| 时间 | 从首个 canary 请求计总 wall time 15 分钟，含审批及审查等待；其中 Code-Agent 活跃执行最多 10 分钟；规划另记 |

按无缓存公开单价简单估算，200k 输入 + 16k 输出约 $0.03752。此估算不含缓存写入规则差异、未知失败收费、价格变动及 Codex 使用成本，不能作为硬保证。

**当前不具备这些任务级硬预算保证。** StartRunInput 没有预算字段，max_iterations 不能限制 Provider 内部重试。需要在执行前选择并验证其一：

- 已证实生效的 Provider/key 级限额，且能分别约束本实验；仅看账户总额度或关闭 Extra Usage 不足以证明 token/调用硬上限。
- 经单独批准的实验专用请求准入/计量适配，位于真实请求边界，计入重试和在途请求；不改 MCP 契约、Agent Loop、生成参数或模型选择。请求按原内容完整转发；根据 Provider 上限/Runtime 正常请求配置预留剩余资源，不足时在请求发送前拒绝整个请求。无法证明最大输出或缓存收费上界时不得声称可硬预留，不能猜一个 4096 限制，也不能截断工具 JSON。

拒绝应留下明确的预算耗尽证据；Runtime/Task 若没有可识别的该类终态，必须实测其进入可解释的失败/等待状态，而非把 generic error 当 Accepted。不能为本次预算扩展一套调度器，也不能未经评审新增产品 budget API。

仅靠事件观测后 cancel 是软限制，在途请求可能超限，不能在授权书中写成硬上限。若最终选择软限制，必须另行明确批准超限风险及停机方式；当前默认不选择。usage/cost 丢失、计数不可核对或连续两次同类 Provider/工具失败即停止，不用重试耗尽预算。

## 7. 结果归因与后续对照

每个请求/Turn/Attempt/Review 记录 UTC 时间及单调耗时、server/store/task/run/epoch/session/turn/request/invocation、配置指纹、requested/resolved/actual model、HTTP 重试/状态、finish reason、usage/cost 明细及缺失原因。只记录脱敏事件，不打印原始认证头、密钥或环境值。

时间分为：Codex 规划/主动审查、Runtime/Flux 排队、Provider 请求与重试、工具执行、人工审批、审查等待、冻结/Gate/Finalize、总 wall time。并行区间不直接求和；模型端内部排队与推理不可分时称“Provider 请求耗时”。人工等待不算模型慢；工具失败、预算拒绝、环境问题与代码缺陷分开归因。

Codex 的 token/费用若当前桌面不提供精确值，记 unknown 和可观测耗时，不能按经验补数或拿账户总用量差冒充单任务成本。Provider bill/list cost/Go allowance 分开，不合并成一个“费用”。保留执行和独立测试结果、原始冻结内容及所有审查发现，不以 Executor 自述验收。

本次先验证单个闭环，不自动开始对照实验。后续同任务的 Codex-only 与协作组应使用相同初始 Git 树、规格、独立用例、环境/预算和干预规则，分别新 Session；固定并记录双方模型/工具版本，避免把第一次审查结果喂给第二组。建议至少多任务/多次重复并交换运行顺序；单次小样本只报告观察，不宣称普遍效率优势。对照组需要另行授权和预算。

## 8. 评审决策与最小下一步

| Gate | 现状 | 放行条件 |
| --- | --- | --- |
| G0 版本兼容 | 固定源码与双依赖图，已有测试 | 新环境从精确基线构建并核对 binary hash/capabilities |
| G1 执行边界 | 未验证 | 选定一个可用环境，通过旧域入站与模型工具出站负向测试 |
| G2 模型/计费 | 本机目录及官方文档可核对；本次未推理，账户计费未查 | 授权后 canary、实际协议和 usage/cost 核对，确定账户额外付费设置 |
| G3 资源控制 | 只有轮次上限，硬任务预算缺失 | 确认可执行限额或批准最小实验计量方案并离线故障验证 |
| G4 本轮授权 | 没有 | 用户明确 Provider/上游、外发数据、环境与预算；旧许可不沿用 |

建议先决策 G1 的环境和 G3 的限额方式，接下来只做选定环境的离线验证，不立即申请“开始真实模型”。不循环调查旧 Session，不用新库绕过旧域风险。G1/G3 未满足时，本次真实执行维持 NO-GO，P0.5 阶段性交付保持成立。
