# V1-b：长期连接、显式任务绑定与工具模式

日期：2026-11-03。状态：设计已认可，候选实现见 [V1-b 实现与验收](manual-collaboration-v1b-implementation.md)。本文件保留设计边界，不代表已部署或已通过真实桌面验收。

依据：[V1 需求](manual-collaboration-v1-requirements.md) R1/R4。先固定 schema、并发及授权边界，再实施路由修改。

2026-11-04 补充：桌面使用发现无 Session 时缺少授权替换入口。现有 connection_info 的显式 reconnect 增量设计及验收见 [重连修复](manual-collaboration-v1-reconnect.md)；下文保留原 B1 设计记录。

## 1. 已核对的现状

- `cmd/codeagent-mcp/main.go` 先构造 Client，再调用 MCP Server.Run；失效 grant/旧基线会使进程在 initialize 前退出。
- `Client` 固定 root/baseline/id，维护 submissions、WS controls 和进程内验证约定。`checkSession` 限制为一个 Session；它不是跨任务连接管理器。
- `newClient` 在构造时检查 Git 状态/HEAD，因此不能直接拿它充当“没有任务的长期连接”。
- `Server(c)` 注册 22 个工具，兼容原执行入口；`ReviewerServer` 提供 7 个只读工具，既有离线 bundle 入口不需要凭据。
- `--profile` 当前默认 executor；改变它的默认语义会影响老配置。新配置应显式选 manual，原 executor 保持兼容。
- grant 文件的权限/属主/符号链接校验、Runtime/Store 验证和撤销观察已有实现，可复用；不能通过跳过这些检查获得长期可用性。

## 2. 选择的最小结构

```text
MCP Server（协议与工具目录，进程内稳定）
  ServiceConfig：manifest 路径、单个项目工作区边界、profile
  ConnectionProbe：按需读取 grant、只读核对 Runtime/Store
  BindingRegistry：不可变 binding_id -> 已验证的单 Session Client
    Binding A：原 Session + 原基线 + Runtime/Store + 授权代次
    Binding B：新 Session + 新基线 + Runtime/Store + 授权代次
```

不新增 Daemon、Supervisor、数据库或任务调度协议。Registry 是进程内路由，不是恢复引擎；断线、进程重启不会执行上一个工具。

### 长期配置

新生成配置显式使用 `--profile manual --workspace /绝对项目工作区 --connection-manifest /私有授权文件`，保留实验开关和 `required = false`。不包含 Session、baseline、delivery。服务不全局注册。

第一版一个配置只允许一个工作区：路径规范化后固定根目录和 Git 身份；不自动授权同仓库的其他 worktree、父目录、子仓库或任意传入路径。另一个 worktree 使用它自己的项目配置。连续任务可以使用同一工作区的不同 Session/新基线，无需改启动配置。

启动只验证参数结构及可读取的本机工作区身份，不读取 grant、不访问 Runtime、不创建 Session、不检查任务 HEAD 是否匹配。不存在/不可验证的工作区只能进入诊断状态，不能在稍后路径出现时自动获得执行权限；需要重新加载配置并建立可信边界。

授权过期、Daemon 未运行或无任务不影响 initialize/tools/list；失败在工具调用阶段报告。用户仍可普通聊天。参数冲突、无效 profile 或损坏程序包仍可使该 MCP 进程启动失败；可选服务配置负责隔离其对普通对话的影响。

## 3. 工具与 schema

### 3.1 保留名称，手工模式的目标必须显式

旧 executor profile 的现有参数/成功结果保持不变。manual profile 的任务工具新增必填 `binding_id`，并保留已有 session_id/turn_id 等精确引用；二者不匹配即拒绝。不同模式的目录在连接期间不变。

不允许以下便捷规则：省略 binding_id 使用 latest/current；以 Session ID 查找失败后新建 Session；自动将旧基线替换为 HEAD；根据另一个聊天的最后调用切换目标。

`binding_id` 是不透明随机路由句柄，不是凭据或授权；持有它不能扩大 grant 的权限。进程重启后旧句柄返回 binding_not_found，不按目录/顺序推导替代对象。

### 3.2 connection_info（只读）

无 binding_id 时：读取当前连接诊断。无效授权仅返回安全错误类别/操作指引，不枚举 Session，不读取私有历史。有效授权时可报告已核对的 Runtime/Store、允许的工作区和当前 HEAD，并明确 execution_bound=false。

有 binding_id 时：只检查该绑定，不改变任何绑定、凭据或 Session。返回 execution_bound=true 仅表示绑定存在且有效，不代表 Turn 已开始或任务已验收。

第一次观察有效 grant 可建立连接身份锚点。此后 grant 的内容/代次或 Runtime/Store 改变只能报告变化；普通读取不能自动采用新授权。

### 3.3 create_session（现有工具，增量扩展）

manual 输入：`request_id`、`baseline_commit`、`server_id`、`store_id`。身份必须来自已核对连接；baseline 必须是工作区当前完整 HEAD，并满足现有新执行工作区检查。

成功结果保留 `id`、`workspace_path`，增加 binding_id、原 baseline、Runtime/Store 和复原绑定所需的引用。创建本身不发送 Turn。

相同 request_id 在同一进程只能对应同一创建请求；载荷变化拒绝。创建响应丢失返回 create_unknown，停止自动创建；跨重启不承诺 Session 创建 exactly-once。用户先在 Talkify/API 核对已创建 Session，再显式绑定，不能因新 Registry 为空就重试创建。

### 3.4 bind_session（唯一新增工具）

建议名称：`codeagent_bind_session`。语义是路由绑定，不是恢复执行，不允许批准、取消或重发。

必填输入：`session_id`、`baseline_commit`、`server_id`、`store_id`。工作区由本项目配置提供，不接受任意路径或凭据。可选 `replace_binding_id` 明确表示用户恢复原范围授权后，要求核对并替换该旧绑定；它不是 force-resume。

校验顺序：

1. 私有 grant 的路径/权限/有效期及当前 Daemon 身份。
2. Runtime/Store 必须与输入身份一致；授权替换不能借此迁移到其他 Store。
3. 项目工作区/Git 身份、原基线、Session 持久 workspace/policy、归档/rebind/fork 限制。
4. 完整持久准入历史及当前执行/人工控制状态。可绑定正在执行的 Session 做观察，但不得重发或替代当前 Turn。
5. 全部通过后发布不可变 binding_id；失败不改变任何现有绑定。

无 replace_binding_id 的首次/重启绑定可使用当前明确授权的连接；有旧绑定时授权代次变化必须显式替换。替换成功返回新句柄，旧句柄保持失效；不把原 Client 的 token 原地改成新 token。

同一 Session 的原基线冲突必须拒绝。相同身份、基线、授权代次的重复绑定复用同一个 Client 和句柄，不能绕过其 submissions 去重、sendMu 或未知准入保护。

### 3.5 其他工具

send/read/wait/handoff/artifacts/cancel/lookup 均按 binding_id 获取固定 Client，再核对 session_id。模型目录只依赖已核对的服务连接；不需要任务绑定，也不测试模型 API。没有授权时不返回模型目录。

原 durable Receipt 的 session_id/turn_id/request_id/cursor 保留，manual 响应增量附带 binding_id 与必要的复原引用。绑定句柄丢失不能替代精确 Turn 引用。

详细证据仍按需读取；get_handoff 不隐式绑定 Session。恢复原聊天时保留有效句柄可直接一次 Handoff；Bridge 重启/授权变化后需要一次显式 bind 再 Handoff，单独记录连接恢复成本，不将其隐藏为“一次查询”。

## 4. 并发、撤销与恢复

- 每个 MCP stdio 进程拥有独立 Registry；不共享跨聊天的可变“当前 Session”。每次调用取得固定 Client 引用后，不会受随后创建/绑定影响。
- Registry 只在校验/发布/替换时短暂加锁，不能持全局锁等待 30 秒。既有 Client.sendMu 继续串行化同 Session 的派发与证据采集。
- 替换时先封闭旧绑定的后续调用并与在途写入边界同步，再发布新绑定。旧响应带原 binding_id；不能被解释为新绑定的状态。已经发出的请求若结果不明，继续走原 request_id 的准入核对。
- 同一工作区在本服务内存在活跃或未知结果写入时，不准入另一个绑定的新写入。读取和人工审批观察不等于新写入授权。
- Bridge 不承诺跨进程/跨用户的 workspace 排他执行，也不提供 OS 沙箱；不能用进程内锁宣称解决 G1 隔离或 Managed 的历史 unknown-owner 问题。不得绕过原 Daemon 的执行准入。
- 授权过期/撤销封闭后续读写，关闭旧观察通道，不取消已准入 Turn。用户在 Talkify 取消仍是独立操作。
- 旧绑定永不自动跟随 manifest 替换；connection_info 可以诊断新授权，但只能通过显式 bind 核验并产生新句柄。
- 授权替换不能删除原绑定中已知的未知提交/创建记录。它们必须作为拒绝新写入的记录保留，直到原请求得到可信核对；新句柄或相同 Session 的新 Client 不得清除此限制。历史没有找到准入仍不等于未提交。
- 断线重连不重放输入、工具副作用或审批。验证约定在 V1-c 实现前重连仍可能 unknown，不伪造恢复成功。

Registry 不持久化，因此不能独自提供跨进程 exactly-once。重启后恢复未知请求必须依赖原 request_id、既有持久准入和可信执行状态；无法确认则仅允许观察，不通过新建绑定来恢复写权限。跨重启未决请求的可靠记录及准入一致性需要与 V1-c 一并评审、V1-d 故障注入验收，在此之前不宣称生产恢复保障完成。

## 5. 工具模式与兼容

| profile | 目录 | 启动/绑定 | 兼容原则 |
| --- | --- | --- | --- |
| manual（新配置默认） | 11 个手工工具 | 延迟连接 + 显式 binding_id | 新 schema 明确，不猜测旧语义 |
| executor（旧默认） | 原 22 个工具 | 原固定 Client/Session CLI | 不静默改变旧模式或丢工具 |
| managed（显式） | 既有受管/Task 工具及其依赖 | 仍按原固定绑定与 capabilities 准入 | 不实施新 Managed 编排 |
| reviewer | 原 7 个只读工具 | 原 live/commit-range/bundle 入口 | 不提供 bind/create/send/cancel |

manual 目录：connection_info、list_models、create_session、bind_session、send_to_session、lookup_admission、wait_for_update、read_session、get_handoff、get_artifacts、cancel_turn，均带 `codeagent_` 前缀。

旧 wait_sessions 和 Task 工具在兼容 executor 中保留，不放入新 manual 默认目录。需要 Task 元数据的用户继续选择已有兼容入口，本批不额外增加一个新的动态工具模式。

服务端仅注册允许的工具，不依赖 annotation 或客户端隐藏按钮授权。Reviewer 的方法级只读限制保持；直接构造隐藏工具请求也不能执行。工具列表不随授权过期而变化，只有调用结果变化。

旧配置可继续运行。生成器新建无 Session 配置时使用 manual；携带旧 attach/reattach 参数时明确生成 executor 兼容配置。不能让旧参数进入 manual 后被忽略。手工迁移只改本项目，不进行全局注册，不迁移 Talkify 工作区或 Store。

## 6. 负向测试与通过标准

| 编号 | 场景 | 必须断言 |
| --- | --- | --- |
| B01 | grant 不存在/过期、Runtime 停止时 initialize/list | 标准 MCP 成功；工具调用返回安全错误；0 Session 创建、0 WS 输入 |
| B02 | 未绑定或旧进程 binding_id 发送 | binding_not_found；不退回默认 Session；0 写入 |
| B03 | A/B 并发绑定、发送交错 | 每个调用只命中其原 Client/Session；另一绑定不能改变目标 |
| B04 | binding_id 与 session_id 不匹配 | 拒绝，0 WS 输入，不泄漏其他 Session 历史 |
| B05 | 同 Session 重复绑定 | 相同元组复用 Client；不同基线拒绝；不清空未知提交 |
| B06 | 根目录/符号链接/Git 身份替换、另一个 worktree | 拒绝，不以相同路径名或共同仓库推断授权 |
| B07 | Runtime/Store 与调用身份不匹配 | 拒绝，不自动 retarget 或创建 Session |
| B08 | 用户替换 grant、旧请求晚到 | 旧句柄失效，新身份需显式核验；旧响应不能改变新绑定 |
| B09 | 新 grant 身份改变 | 即便 replace_binding_id 也拒绝原任务迁移 |
| B10 | 绑定正在执行/待审批 Session | 允许有权限的观察；新发送拒绝；审批不自动处理 |
| B11 | 丢失 Send ACK 后重新 bind | 原 request_id 可查；没有重复 WS 输入；证据不足就停止 |
| B12 | 创建响应丢失、Bridge 重启 | create_unknown；不自动再次创建；用户核对后绑定已有 Session |
| B13 | 同工作区第二个活跃/未知写入 | 拒绝并报告原任务引用；不能靠新 binding_id 绕过去重 |
| B14 | 旧 executor/reviewer 配置 | 老输入/结果/工具集合和安全检查不变 |
| B15 | manual/reviewer 直接调用未注册写工具 | 标准未知工具/拒绝；0 副作用，不以 UI 隐藏代替保护 |
| B16 | 连做两任务、提交后 HEAD 改变 | 原绑定拒绝续写；新 Session 显式绑定新基线；长期配置不改 |

离线验证使用固定脚本 Provider、真实 SQLite/Daemon/MCP 与临时仓库，不连接生产 Store。真实 GUI 检查单独记录：过期 grant 下普通聊天、原生工具存在、重新授权后诊断/显式绑定、多个绑定不串目标；不调用模型即可覆盖接入行为。

## 7. 最小改动范围与实施批次

1. `internal/mcpbridge`：拆出只读 Runtime 连接探测，复用现有安全授权读取；实现服务及不可变绑定 Registry。保留 Client 的执行边界。
2. 新 manual Server 注册与 schema；一个 bind_session 入口。旧 Server/ReviewerServer 不重写，不将兼容工具强行改成可变路由。
3. `cmd/codeagent-mcp`：显式 manual/managed profile 分流、参数冲突校验、manual 长期配置输出；executor 默认保持旧语义。
4. 离线标准 SDK/stdio 测试与 B01–B16；只在通过后构建候选包。不替换运行中二进制。

建议分 B1（延迟连接与 Registry）、B2（manual 工具/CLI/兼容）、B3（故障矩阵与 GUI）三个可审阅补丁。每步提供 Diff；不同时改 Agent Loop、模型参数、Flux 或 Task UI。

本评审需要确认的新增范围只有：一个明确的 bind_session 工具、新 manual profile 的必填 binding_id，以及“重启后显式重绑定”的可见恢复成本。它们用于消除隐式目标，并非新增协作编排。

## 8. 发布与回滚边界

未完成本设计前，V1-a 只能保证已修正配置下的可选服务策略和已覆盖错误诊断；不能宣称已经具备长期连接。当前安装包和生产配置保持原样。

本批无数据库迁移。回滚新二进制后，manual 配置不能直接交给不支持该 profile 的旧程序；应恢复备份或停用该条目。需要旧任务读取时保留原身份/基线并使用原兼容入口，不能改写历史。回滚不重启/取消任务、不恢复过期授权、不重新设 required=true。

现有 owner grant 仍具有广泛 Runtime 权限；本设计只是 Bridge 路由约束，不能称为服务端最小权限授权。同 UID 本地客户端的隔离、外部不可信客户端 scoped grant 以及严格执行隔离继续独立评审，不混入 V1-b。
