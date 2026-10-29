# 旧 Session 归属调查及受控处理评估

2026-10-29。限定对象：Store `cstore_16a5c66010fa0840a975cb9ff13173e8` 中的 `20260809-113038-ea2450ff`，以及判断本次受管准入风险所需的 bracket 关联元数据。仅只读查询原库，没有读出其他会话正文，没有修改历史事实、租约、Task、模型或权限，没有调用第三方模型。

## 结论

1. 找到了明确的旧版字段缺失来源，以及该 Session 与 code-agent 项目历史的强关联；没有恢复出可信的原始**绝对工作区路径**。
2. 该 Session 只有一次后台 Job。记录表达为找不到可执行程序的启动失败；但旧日志没有区分 Runtime 诊断和子进程输出，也没有独立的 exec 阶段/进程身份事实，因此不能自动证明安全。
3. 有界全量 bracket 调查发现当前只有这一个父 Session 的工作区未知，不是尚未排查的长队列。该结论限于本次快照和既有检查规则。
4. 正常子流误判已由上一批修复；本条剩余历史记录不能靠猜路径、清数据、job_finished 或用户一句确认放行。本批不再循环提交启动请求。
5. 下一步应选择补充独立证据的薄核对流程；若无证据，则保持原库拒绝，另行授权并验证独立执行边界后，才可在新 Store 完成桌面验收。新 Store 本身不是 OS 写入隔离。

## 原始记录和旧代码互证

Session 创建于北京时间 2026-08-09 19:30:38，最后更新于 22:15:00。workspace_path、workspace_root、workspace_rel、workspace_ext_id 均为空；metadata 仅有 turn_seq。派生 index.db 中该 Session 的工作区同样为空，且索引是原库的可重建投影，不能当作独立归属证明。

这条会话的 Git 工具结果记录了以下提交短哈希，本仓库能唯一解析到对应提交，时间也吻合：

| Event seq | 工具完成时间（北京时间） | 提交 | 内容 |
| --- | --- | --- | --- |
| 28171 | 20:04:25.537755 | c5452527761ca2e610519240f1ed44390291a618 | TUI 改为用 HOME 作为 Store key，与桌面历史共用 |
| 28190 | 20:08:13.233570 | a76350c7b3d64abf0d18ef886d694abf1aea37be | Server context snapshot |
| 28916 | 22:13:25.088006 | 7ea23f8974834181aeea9ea5036f19311c1e030d | 修复 TUI 会话没有保存 WorkspacePath，并按工作区过滤 resume |

`git show 7ea23f8 -- cmd/codeagent/main.go cmd/codeagent/tui/run.go` 直接显示：此前 Builder 留空 WorkspacePath，TUI 直接保存；该提交才在新建路径设置 `sess.WorkspacePath = root`，并拒绝安全归属不明的旧会话。

因此这次缺字段有实际历史原因，不能说成最近恢复代码随机损坏了数据。该 Session 还记录了修复这一缺陷的提交，但修改源码不意味着当时已运行的二进制或内存 Session 自动补齐字段。

Git 对象可被克隆/复制，HOME Store key 也不等于执行 CWD；提交匹配只能证明项目历史关联，不能单凭它确定当时是本机哪个 checkout。现存 reflog 查询没有找到该日记录。限定工具参数及结果未提取到绝对路径、可信 pwd 结果；没有把相对文件路径当作根目录证明。原生产二进制的构建身份也没有随该 Job 持久化。

## 唯一后台调用

关联：父 Session `20260809-113038-ea2450ff`，Turn `turn_9`，Call `call_00_dybdCSP4xk8YrEspRoNM0899`，子流显示 ID `job_1`。

| seq | kind | 时间（北京时间） | 实际证据 |
| --- | --- | --- | --- |
| 28871 | tool_started | 22:06:26.352645 | run_command，程序为 go，background=true |
| 28873 | job_started | 22:06:26.352740 | 父会话的起始 bracket |
| 28874 | job_output | 22:06:26.355248 | 64 字符输出，以 failed to start 开头，包含 executable file not found |
| 28876 | job_finished | 22:06:26.355385 | failed，exit code -1；约 2.645 毫秒 |
| 28878 | tool_finished | 22:06:26.355579 | 旧版结果保存在 Observation，尚无结构化 Output |

旧版 `internal/jobs/jobs.go` 会先发布 JobStarted，再调用 cmd.Start；启动错误和子进程 stdout/stderr 都经同一个 sinkWriter 进入 job_output。故“找不到程序”的文本是很强的启动失败线索，但不是带有可信来源类型的 `execution_started=false` 证明。子进程也能输出相同文本；-1 也不是只为未启动保留的返回值。没有 PID、PGID、可核对的 process-start identity、Runtime instance 或 boot identity。

当前 Registry 的显示 job_id 由进程内递增计数生成；重启后可再次出现 job_1。不能查询当前 job_1 的状态，便声称知道 8 月这一 Job 的活跃性。旧 job_output 的 TurnID 也为空，本次只通过原始 seq 和时间窗口取证，没有把该子分区所有历史输出归到此调用。

本批未发现可独立关联到该 Job 的当前活跃进程证据；由于缺少历史进程身份，这不等于证明没有活跃副作用，更不等于可以恢复或重跑。

## 同类记录排查及风险范围

查询用 mode=ro/query_only，单次限制 8 秒、30,000 条 bracket 和 16 MiB payload，强制先用已有 session/kind/id 索引过滤。实际调查快照：

| 项目 | 数量 |
| --- | ---: |
| job_started/task_started bracket 总行数 | 5,412 |
| 扫描 payload bytes | 7,748,408 |
| 缺同 ID Conversation、但关联已知工作区父会话的子流行 | 2,705 |
| 关联工作区未知父会话的子流行 | 1 |
| 工作区未知父会话 | 1 |
| 其余关联缺失/不合法、多父冲突类别 | 0 |

计数是事件行/关联分类，不是当前 OS 进程数，也不是对所有父工作区都已证明安全。其他活跃 Session 会使之后的计数变化，不能把本次快照当作永久豁免。

风险处理应按证据分开：

- 已知目标工作区的活跃执行、租约或未知副作用：继续拒绝并发写入。
- 正常子流有可靠且无歧义的父会话、工作区可解析且不重叠：上一批修复已缩小范围，不因缺同 ID sessions 行而全 Store 拒绝。
- 确有活跃副作用但归属未知：recovery_required，不重新派发。
- 本条仅有历史记录、未能核对活跃性：应标作历史证据待核对，提供具体记录与缺失证据，不把“无法证明”误写成“已发现活跃进程”。证据不足时当前准入仍拒绝。
- 没有后台/client/subagent 执行历史的普通旧会话：不应因工作区为空而全局阻塞。本批新增回归验证这一点。

当前代码仍会因真正未知的历史归属阻塞新任务，因此长期可用性问题并未全部解决。它不应被描述成 Managed Collaboration 桌面验收已通过。

## 最小的受审计核对流程（评估，未实现写接口）

先导出一个可校验的证据索引，而不是修改 Session：本批已生成 [调查证据索引](investigations/legacy-session-20260809-evidence.json)，绑定原 Store、Session、事件分区/seq/时间和原始 payload SHA-256，结论明确为 unresolved，admission_authority=false。

这个 JSON 是调查导出，不能冒充已经防篡改的管理员账本；hash 校验提供内容一致性，不自行证明来源可信。Runtime 不读取它来授权执行。

如后续确需落地，采用一次性、owner-only 的薄管理流程，复用现有事件/产物基础，不增加 Supervisor 或新的执行循环：

1. **取证**：引用原始事件哈希、可信外部日志/备份、工作区身份及执行环境证据；保留证据来源和缺失项。
2. **核对**：人工可以记录意见及依据，但意见和系统可验证事实分开。可接受结果包括 verified_scope、verified_no_exec 或 unresolved；没有机器验证依据的“我确认安全”只能是意见，不能成为 admission proof。
3. **限定作用**：有效决定必须绑定原始 Session/bracket、特定目标 workspace anchor、Runtime/Store/执行环境身份；不能建立“忽略此 Session”的全局白名单。
4. **失效检查**：原事件 hash、目标工作区/执行边界或相关环境身份变化，或者出现新的未知执行证据，应重新核对；到期只能使决定失效，不能用于证明进程已结束。
5. **独立审计**：记录认证操作者、决定时间、正反依据、验证器版本。Agent 自报 plan/review 不能充当管理员决定。通过只意味着允许重新检查准入，不自动 Resume/Retry 原 Turn。

本条现有材料尚不足以填写 verified_scope 或 verified_no_exec；不增加只为这条数据服务的文本特判。

未来最小证据补齐可放在现有 JobEventSink/执行租约上：每次启动的全局唯一 execution identity、Runtime instance、workspace anchor，以及由 host 写入的结构化 exec-started/start-failure facts。缺字段保持 unknown，不能根据旧 job_output 自动回填。这是后续数据协议建议，不是本批扩展执行状态机或修改 Agent Loop。

## 可以执行的人工处理方案及前提

### A. 在原 Store 完成核对

需要额外的可信历史材料，例如能绑定原 Session/Call 的启动日志、工作区快照或带可靠执行阶段来源的进程记录。仅有同项目提交、空派生索引、时间久远、没找到 PID、重启过 App，或者用户确认，都不满足。

取得材料后先生成独立核对决定并检验作用域/证据，不改原 Session 字段或事件；没有可用证据则明确结束本条核对为 unresolved，而不是反复请求 Run。当前没有可直接安全执行的 force-resume。

### B. 经另行授权，在独立执行边界完成新 Store 验收

可先验证产品链路，同时保留旧 Store 原样，但前提是证明旧执行身份无法写入新的目标工作区，例如独立低权限执行身份或具备相应文件系统隔离的执行环境，并核对挂载、ACL/权限、共享目录和执行入口。

**仅新建 SQLite、另起同 UID Daemon、换一个 tmp 路径或清空历史，不构成这一隔离证明。** 当前本批没有创建新 Store、执行环境或额外 Daemon，也没有新增访问权。

满足边界并取得明确授权后，才可使用新工作区/Store/Runtime 身份与匹配的独立 MCP 授权完成受管桌面试验；原 Task/Session 保留可读，不自动搬迁未知执行。新环境通过不等于旧 Store 已恢复，不能宣称日常旧数据兼容问题全部完成。

## Diff、测试和桌面证据

本批生产恢复逻辑未继续放宽；功能修复 Diff 沿用 [上一批说明](p05b-process-ownership-error-fix.md)。只新增两项安全回归：

- 缺工作区的历史 Job，即使输出与真实的 executable-not-found 文本一致并记录 failed/-1，也不能自动放行；核对过程不改原 Session/EventStore 事实。
- 工作区未知但没有进程历史的普通会话，不阻塞无关工作区。

上述两项与现有归属、幂等/重开测试一起运行 `go test -race ./internal/session/sqlite -run 'TestManagedRecoveryProcessOwnership|TestPrepareRunWithUnrelated|TestLegacyFailureText|TestUnownedConversation' -count=1`，通过。日志 `/private/tmp/codeagent-legacy-investigation-regression.log`。本批未修改 Agent Loop、生成参数、Flux、AgentKit UI 或 Adapter。

实际桌面证据仍为此前 Task 创建/计划/角色/Timeline 已显示、Run 准入失败、无执行 Turn；本次未重复失败请求，未提交 Review 或伪造 Accepted，也未修改原主库。没有新的真实模型或桌面受管闭环验收证据。

下一执行前提是 A 得到足够的可验证证据，或 B 的隔离边界与新访问范围获得授权并验证；届时再安全部署新版 daemon、重新检查身份与预算。审查返工必须针对真实问题，不为了凑一轮 request_changes 捏造缺陷。
