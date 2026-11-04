# G1 / G3 最小技术决策与负向验证

2026-10-30，Asia/Shanghai。基于 [真实模型验收评审](managed-real-model-acceptance-review.md) 与 [P0.5 兼容基线](contracts/p05-compatibility-baseline.json)。**当前仍为 NO-GO；本文件不授予 Canary 或模型任务权限。**

本轮没有部署、新用户、生产配置或数据库写入，没有修改 Agent Loop、Flux、Task UI、模型参数或 Provider 生产实现。新增的 Go 文件只有离线特征测试。

## 已确定的最小决策

1. **G1 选择现成独立 Mac 作为第一候选。** 用户已确认有设备，尚未核验设备、权限与工具边界；不迁移当前 Talkify，不创建专用用户，不调查同一条旧 Session。若独立 Mac 不能通过边界核验，停止并另议，不自动切到同 UID 新库或新账户。
2. **G3 原生 Go 额度不能独立满足候选实验硬预算。** 用户确认当前为 Go、Extra Usage 关闭、自动充值关闭；Budget policy 状态未提供，但其公开语义本身已不足以保证本实验调用/Token/标价上限。
3. **先评审实验专用传输层准入/计量适配，再决定实现。** 不建设通用 Token 网关；不修改 MCP 或生成参数。普通 HTTP 外层逻辑调用计数不够，必须核对真实发出请求和重试。

候选 12 次、200k 输入/16k 输出、最多 1 次返工、15 分钟和 $0.50 均保持为待批数值；不能为了让实验启动悄悄改变其含义。

## G1：独立 Mac 的最小执行拓扑

**独立 Mac：可信控制面 + 受限工具执行面。** 在该机从固定 commit 构建 Talkify、MCP Bridge 和 Daemon；控制凭据仅在该机可信控制面。Code-Agent 的文件工具和测试子进程必须受另外的路径/进程/网络限制，不能因为“换了机器”便允许模型读取该机所有文件。

当前 Mac 保持原 Store、Daemon 和用户工作区。不得把新环境的 owner token、SSH 私钥或可用 agent socket 放回当前旧执行域，使旧进程可以间接写入新环境。当前对话没有可用的该 Mac 已验证接入入口；用户确认“有设备”不是允许扫描网络或直接登录任意主机的依据。

现有 Bridge 仅接受 loopback，并检查本机 daemon PID。因此 Bridge 与 Daemon 同机运行，使用该机真实本地授权；不把本机隧道 PID 假充 daemon PID、不放宽 endpoint 规则。规划/审查在该可信环境的 Codex 客户端进行，或以后单独评审安全控制方式，不默认在当前旧用户环境保存新环境的写权限。

### 最小文件和工具范围

- 模型可写：`duration_smoke/duration.py`、`duration_smoke/test_duration.py`，规格沿用原评审，不修改需求。
- 模型只读：明确列出的需求文本、上述文件及必要的合成测试上下文。不得读原三个产品仓库、旧 Session、其他 HOME 内容、密钥、owner grant 或实验状态库。
- 固定验证：受控环境中的 `/usr/bin/python3 -I -B -m unittest discover -s duration_smoke -p test_duration.py -v` 和 `git diff --check`。命令须匹配固定 argv，不接受拼接、重定向、额外参数、环境覆盖、后台化或任意解释器命令。
- 固定 Python 命令会执行模型生成的测试，因此其子进程仍是不可信代码，必须拒绝越界写入、联网、读取控制凭据及逃逸子进程；不能以“命令字符串固定”替代这层约束。
- `.git`、Session/Flux、ReviewBundle、日志及计量账本由可信组件维护；模型工具不可写这些路径。OS/Python/Git 的必要只读依赖逐项登记，模型写入范围与系统组件写入范围分开。

现有 `managedTurnTools` 会限制工具集合并禁止后台，但**没有将 ReviewPolicy.Files 变成两文件的 OS 写入白名单**。路径工具也保留外部路径审批能力；本实验不能通过批准越界请求突破限制。

最小装配候选是在实验宿主中使用既有 Repository/TurnExecutor/ServeRunBuilder，限制实验工具注册与目标路径，固定验证工具的 argv，并为实际子进程安装经验证的 OS 约束。保留原审批接口，不修改通用 Agent Loop。具体 OS 约束方式须在独立 Mac 上验证后选定；若无法证明拒绝外部写入、凭据读取和进程逃逸，G1 不通过。不能仅用应用层路径检查宣称 OS 隔离。

### G1 负向测试清单（目标机尚未执行）

| ID | 操作 | 必须观察到的结果 |
| --- | --- | --- |
| I1 | 从旧执行域使用已获准的测试路径/入口尝试写入新机哨兵，或用旧凭据调用新控制入口 | 文件不能修改；认证拒绝；新机不把旧 UID/Keychain/共享目录自动视为已授权 |
| I2 | 检查新 workspace、控制凭据、程序及依赖的共享、ACL、签名与加载路径 | 无旧域可写来源、可用控制 socket 或可替换程序；不能只检查顶层 chmod |
| O1 | 通过实际模型文件工具使用绝对路径、`..`、symlink/hardlink 指向哨兵/控制路径 | 读写均拒绝，哨兵内容与 inode 保持；不弹出可绕过硬边界的批准入口 |
| O2 | 在许可 repo 内尝试写第三文件、`.git`、配置或账本 | 拒绝，不仅在 Finalize 时发现范围外修改 |
| O3 | 由固定 Python 测试尝试读伪密钥/owner grant、读父进程环境或获取控制面权限 | 拒绝；工具环境无真实密钥、owner token、SSH agent 能力 |
| O4 | Python 测试尝试联网、任意 exec、后台化或脱离进程组存活 | 约束生效，退出后无可继续写候选的残留进程；不以“主 PID 不存在”证明安全 |
| O5 | 扩展固定命令的 argv、shell 运算符、环境覆盖、sudo 或未许可解释器 | 工具执行前拒绝，实际副作用次数为 0 |
| P1 | 两个许可文件的合法修改、固定验证、冻结产物读取 | 正向成功，才能排除“所有能力都被关掉”的假通过 |

只使用合成哨兵和假凭据进行负向测试，不拿真实密钥做泄漏验证。旧机到新机的拒绝测试需要用户提供并允许核验的具体入口；不开展端口扫描或宽泛远程访问。记录实际执行身份、进程树、允许/拒绝路径、退出码、哨兵前后指纹与证据时间。

## G3：原生预算的确切边界

用户只读确认：**Go，Extra Usage 未开启，自动充值未开启**。这不是本轮从 Console API 独立核实的结果，没有修改开关或新建 key/policy。

官方文档确认以下语义：

- Budget policy 可以阻止后续计费请求，但套餐内 Go 用量不计入；余额支付的 Extra Usage 才计入。预算按成员/服务账号等主体，而不是独立任务 key 自动获得独立额度。
- 限额是在请求开始时检查，在途请求仍可完成并略微超出；不能把设置 $0.50 直接写成严格任务资源上限。
- Extra Usage 开启可在 Go 用量耗尽后转扣余额；自动充值是另一项独立扣款行为。用户报告的两个开关均关闭降低了转扣风险，但本轮没有耗尽真实额度进行行为证明。

来源：[Budgets](https://opencode.ai/v2/docs/console/budgets/)、[Go](https://opencode.ai/v2/docs/console/go/)、[Billing](https://opencode.ai/v2/docs/console/billing/)、[API key 主体](https://opencode.ai/v2/docs/console/api)。这几项不能证明 12 次、200k/16k Token 或本实验 $0.50 的硬限制，故需要实验级准入方案。

### 协议、Tool Calling 和账单证据

- 固定基线内置 `opencode-go` 使用 OpenAI-compatible transport，默认 base 为 `https://opencode.ai/inference/go/openai/v1`，目标 wire ID 为 `qwen3.8-flash`；本机用户层 base 与其一致。新机合并配置须重新核对，不复制旧机 secrets。
- 新增离线测试截获完整请求，确认 POST `/chat/completions`、稳定 `x-opencode-session`、模型和正常 reasoning 配置，并验证 8 KiB 工具参数未被截断。它使用替代 RoundTripper，**没有拨号，不是实际 Provider 工具调用验证**。
- 官方将 Qwen native API 列为 Messages，并声明其他文本 API 可翻译；当前不自动切换 API。真实链路仍需另批 canary。来源：[Go endpoints](https://opencode.ai/v2/docs/console/go/)。
- 一次无认证公开 GET `/inference/go/v1/models` 返回 200，目标项只含 `id/object/created/owned_by`，没有本实验所需的最大输出上界；该 GET 没有源码、密钥或推理输入。
- 本轮本机 MCP 目录仍报告目标 configured/available/supports_tools=true、历史 verified_at=`2026-10-29T10:48:29.603722Z`。这里只读，不将历史验证当本次 canary。

公开 API 文档区分 `usage.cost`、`opencode.list_cost`、`opencode.allowance`、`billing_source` 和分类 tokens；流式值在携带 usage 的最终事件上。Runtime 标准 usage 会保留已观察到的 Token，但不保存这些 Go 计费扩展为 typed accounting。完整成功非流式响应可从 Raw 取回；流式及失败路径不能据此假定扩展已保留。来源：[Inference API](https://opencode.ai/v2/docs/console/api/inference)。

另有价格差异：固定基线内置 Qwen 参考价格为输入 $0.16 / 输出 $0.32 每百万，当前 Go 官方表为 $0.15 / $0.47。不能用内置估算作为实际账单或硬预留依据；本轮未为价格变更改生产代码。

## 最小实验请求准入/计量适配（提案，未实现）

优先采用**实验装配中的 HTTP 传输装饰器**，不建立监听网关，不修改 Provider URL、MCP 或模型生成参数。只允许本次选定 origin/path/model，所有模型请求、重试和流式 fallback 都经过同一实验账本。

关键装配限制：ServeRunBuilder 会在显式选择模型时重新 BuildProvider，不能只包一开始注入的 base Provider，否则会漏计。实验 builder 必须在每个 Turn 的真实 Provider 构造后、执行前确认装饰器已装到 OpenAI-compatible HTTP client（包括 ResilientProvider 内层）；未知 Provider/协议拒绝。此为待批准的实验宿主代码，不是新增产品 API。

1. **物理请求计数**：先持久保留一个名额，再发送。所有失败/连接尝试保守计数，拒绝本地重试不得触达上游。关闭 redirect/代理/连接重用和隐式 replay 等旁路，配置单个新 HTTP/1.1 连接处理一次 POST；核对 wire trace 与计数。仅包 RoundTrip 而允许内层 Transport 透明重试，也不能宣称已覆盖每次物理发送。Provider 服务内部的请求不在本机可观测范围。
2. **请求前资源预留**：账本串行准入、检查请求/时间/剩余输入/输出/费用后再发出。输入必须有可信 tokenizer 或已验证保守上界；输出依据 Runtime 原请求与 Provider 的可信最大值，费用按含缓存收费的保守上界预留。不能用最终 response usage 倒推此前已经安全准入。
3. **保留实际计量**：透明旁读 JSON/SSE，仅提取白名单 usage/cost/model/request-id、终止原因和请求指纹；不记录认证头、提示词或工具源码。保存成功、HTTP 错误、SSE 错误、已观察 usage 后断流和取消的字段。累计快照不重复相加；没有最终 usage 的请求保持 unknown、占住预留，不释放为 0。
4. **不改输出策略**：完整请求 body 的发送前后 hash 一致；不插入/降低 max_tokens、不修改 Thinking、工具 schema 或生成文本。不因额度紧张换模型/付费路由。不允许模型工具写账本。
5. **耗尽即停**：新请求在网络发送前拒绝，输出本地明确预算原因并取消实验执行 context，阻止 resilient fallback/后续请求继续外发；断电后的未决预留不自动释放。停止必须核对实际活动 Turn/进程和 Task 状态，不把取消请求当作已停止或 Accepted。

**当前无法证明 16k 总输出硬上限能在不改正常请求的情况下准入第一条请求。** 基线 chat request 不显式发送 max_tokens，公开 models GET 也未给默认最大输出；context_window 不能当 max output。适配器不能凭空解决这个信息缺口。必须取得可靠上界，并让候选总预算容纳它；否则请求前拒绝。若需要调整预算或由 Runtime 资源所有者明确调整实验配置，须另行评审，不能由 Codex MCP 偷改或静默截断。

## 已执行的离线负向/特征验证

新增 `internal/model/opencode_review_test.go` 只描述冻结适配器行为，不把“缺口符合预期”的通过结果称作硬预算达标：

| 验证 | 结果 |
| --- | --- |
| mock transport 截获 Qwen Chat Completions 路径与 session header | 通过，无外部网络发送 |
| 不注入 max_tokens/网关扩展字段，8 KiB 工具 JSON 原样返回 | 通过；不代表真实模型能力 |
| Go cost/allowance 与 stream Raw 是否进入 typed accounting | 确认缺口：未进入；成功非流式 Raw 有原始字段 |
| 一个普通 policy 429 的逻辑流式调用，MaxRetries=2 | 实际 4 次 transport 调用：1 次 stream + 3 次 Complete；证明逻辑计数不足 |
| 既有失败/流式错误/取消保留已观察 Token usage、重试累计及 quota 特殊分支 | 定向 race 通过；没有 usage 的真实失败仍属 unknown |
| 既有 managed-worktree 路径/链接拒绝、create/edit 越界拒绝、shell 越界读取拒绝和取消回收 | 定向 race 通过；只证明本机被测路径，不是独立 Mac 的 OS 隔离或两文件限制 |

日志：`/private/tmp/codeagent-g1g3-characterization-20261030.log`、`codeagent-g1g3-model-offline-20261030.log`、`codeagent-g1g3-path-offline-20261030.log`。使用已有依赖缓存，未安装包。

### 适配方案实现后必须补的负向用例（当前未实现/未跑）

| ID | 必须验证的拒绝/保留行为 |
| --- | --- |
| B1 | 第 13 次与并发最后名额竞争在发送前拒绝；重试/stream fallback/重建 Provider 不漏计 |
| B2 | 不同 host/path/model、redirect、未装饰 Provider 或隐式发送旁路拒绝 |
| B3 | 输入/输出/价格上界缺失或剩余不足时发送次数为 0，不能静默降低请求配置 |
| B4 | HTTP 错误含 usage、SSE 最终 usage 重复、先 usage 后断流/取消均正确计量；未知不归零 |
| B5 | ledger 写入失败、未决请求恢复、落盘前后故障不释放已占资源，不自动重放 |
| B6 | 预算拒绝后 cancellation 生效，无后续物理请求；模型参数/body hash 不变；Token/费用不依赖旧 registry 价格 |

## 剩余输入与放行条件

1. 用户提供独立 Mac 的具体可核验入口/操作方式；先只读确认系统、架构、执行身份、共享与工具链，再单独批准离线哨兵测试。当前未登录该机、未迁移配置。
2. 确定 OS 工具约束能否在该机满足 I/O/P 用例；无法证明即停止，不升级隔离平台或恢复状态机。
3. 取得 Provider 默认输出/收费的可靠上界；评审最小传输适配及 B1–B6。所有候选预算保持未授权。
4. 上述通过后才申请一次明确的 canary + 真实任务授权。Canary 的物理请求与资源占用计入同一总预算，任务规格、真实审查原则、ReviewGate/Finalize 均保持原评审。

本批没有修复生产 Provider 的 429 或价格行为，也没有为实验新增通用网关。当前 G1=设备已选、隔离未验证；G3=原生限制已厘清、硬预算缺口已复现、最小适配待评审；维持 NO-GO。
