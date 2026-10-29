# B2-c Desktop Smoke Test

2026-10-28 完成部署检查及元数据冒烟；2026-10-29 补充当前桌面对话的原生 MCP 调用验证。结论：**现有 Talkify Daemon 的 B2-c 装配、原 Session、Task 元数据和增量回放已通过；真实桌面受管执行、ReviewGate/Finalize 闭环本轮未运行。** 下一步可进入 P0.5-A 只读实现，不将本次冒烟当作真实模型任务验收。

本轮没有第三方模型授权，因此没有派发 Turn 或启动 managed Run，没有修改模型、Agent Loop、生成参数或 AgentKit UI。用户已自行更新并重启应用；本轮不再次停止、替换或重启运行中的 Daemon。

## 实际运行实例

| 检查 | 观察结果 |
| --- | --- |
| Talkify / Daemon | PID 25783 / 25823；Daemon 是该 Talkify 的子进程，来自当前 Debug-MacDirect 应用包 |
| HTTPS endpoint | `https://127.0.0.1:62210`，正常 TLS 和主机名校验，没有 skip-verify、代理或重定向回退 |
| Runtime | `srv_31ff12dc-6485-453d-a565-53da1fe86f03`，版本 `dev-36b06b1-ded785f5165e` |
| Store | `cstore_16a5c66010fa0840a975cb9ff13173e8`，Task schema 3 |
| 真实数据库 | `~/.codeagent/projects/xiaoyuan-32d10d908e8b/sessions.db` 与同目录 `managed-collaboration/flux.db`；由该 PID 的已打开文件核对 |
| 能力 | live 返回 `task_managed_workflow_v1`、`task_review_gate_v1`，profile `local_foreground_v1`，`automatic_turn_replay=false` |
| 认证 | 不携带凭据查询 Task capabilities 返回 401；owner 沿用已有显式授权，未放宽共享设备权限 |
| 原 Session | `20261027-165016-b3490cad`，工作区 `/private/tmp/codeagent-gui-workspace-__aaks0i/repo`；授权 reattach 身份验证通过 |

PID、端口和路径指纹是本次观察值，不作为产品默认配置。没有启动另一套 Daemon。运行实例已经由用户升级，因此下面的备份是**用户升级后、此次元数据写入前**的快照，不能追溯称为升级前备份。

## 备份与升级处理

检查发现 Session 库约 62GB，当时磁盘可用约 24GB。普通 SQLite 完整复制会超出可用空间；首个只读完整性检查进程已终止，没有完成普通复制，也没有修改源库。

随后先在两份临时 WAL 数据库上验证 APFS `clonefile`：包含未 checkpoint 数据、克隆后继续写源库、写克隆库互不影响，临时库 `quick_check` 通过。实际备份使用同一实现：

1. 对 Session 和私有 Flux 同时取得短时 SQLite writer reservation/read mark，不执行数据 SQL 写入。
2. 用强制 `clonefile` 保存各自的 main + WAL；失败不会回退成 62GB 普通复制；不复制 SHM。
3. 在源锁保持期间只读打开克隆，核对身份、记录计数和空受管执行域，再释放锁。
4. 保存非秘密 Runtime 身份文件、当前 Daemon 二进制及旧 Bridge。没有复制 connection manifest、令牌、Keychain 或环境密钥。

实际耗时约 **0.585 秒**，目录 [backup-apfs-pre-smoke](/private/tmp/codeagent-b2c-desktop-smoke-20261028/backup-apfs-pre-smoke/)，目录 0700、数据库/身份文件 0600。实际大库只做有界逻辑读回，**没有完成 62GB 全量 integrity scan**。这是同卷共享块的写时复制快照，不是独立磁盘的灾难恢复备份，后续源库/副本分歧仍消耗空间。

当时受管 Run、持久执行租约和私有 Flux Task 均为 0。另有 **57 条旧 inbox 保留 running**，更新范围截至 2026-10-26，早于本次启动；活动快照当时没有 running/queued 状态。活动记录中的旧 Turn ID 不等于当前执行。没有自动修复、取消或重跑这些记录，也不声称其历史副作用已得到证明。数据库体积和历史 inbox 对账需另列 Runtime 运维问题。

项目 `build/codeagent-mcp` 仍是较早构建。本轮先构建新版到临时目录、验证通过并备份旧文件，确认没有运行中的 Bridge 进程后才原子替换项目原路径。新 Bridge 实测为 `0.2.0-experimental`、18 个工具。`.codex/config.toml`、授权文件、访问期限、工作区和 Session 绑定均未更改；没有全局注册。当前对话之后实际发现并调用了新工具。

## 元数据冒烟

使用原隔离 Session，不创建新的 Conversation，不调用任何 send/start_run/approval/工具执行接口。

| 项目 | 实际结果 |
| --- | --- |
| Task | `ctask_a55bef04-850c-4d1f-aa32-2a25ae222e10`，目标 `B2C_DESKTOP_SMOKE_20261028: metadata only, do not execute` |
| Participation | 同一个 reported Codex handle 关联 planner/reviewer；local Code-Agent executor 精确绑定原 server/store/Session |
| 计划 | 保存明确禁止执行的冒烟计划，reported Attempt 和版本 1 artifact 可读 |
| 幂等 | 相同 create request 重试，持续返回原 Task；不会新增 Task、Turn 或里程碑 |
| 历史事件 | 分页 2 条读出 5 条事件：task.created、三条 participation.attached、plan.reported；去重后水位为字符串 `5` |
| Bridge 重启 | 新进程重接原 Session，Snapshot 完全一致；`after=5` 返回空增量 |
| 原 Session 计数 | 冒烟前后均为 4 个 Turn、24 条历史 model_started/model_finished 事件；本轮新增模型请求 0 |
| 最终状态 | `mode=collaborative`、waiting/planning、`execution_not_started`、`task_acceptance=not_reviewed`、无 managed_run/workflow_ref |

**创建关联 Task 不等于创建受管 Run。** 现有 `POST /runs` 成功后会自动派发，没有“仅准备但不执行”的公开入口。本轮不修改 Runtime 配置来伪装无模型执行，因此没有测试真实桌面 managed dispatch、审批或 PASS/Finalize。此前脚本模型离线闭环证据仍单独保留。

原生 MCP 分两层留证：先用实际 STDIO Bridge 验证完整元数据流程及进程重启；随后在**当前 Codex 桌面对话**直接调用 `codeagent_read_task`、`codeagent_read_task_events(after=0/5)`、`codeagent_create_task` 的同请求重试，结果与前者一致。这一层不是 CLI 驱动代替桌面调用。

幂等 create receipt 保持原始 `revision/task_seq=1`，而当前 Snapshot 为 5，属于原回执重放，不能用 receipt 将 UI 状态回退。

## Talkify 界面核对

原生桌面工具直接观察到原 `GUI_SYNC_TURN_1` 列表项可选中，窗口标题、输入框和最近路由信息正常。期间窗口切回了用户另一个运行中的会话，没有继续争夺选择或取消其工作。

旧两轮历史是否完整显示由用户明确确认：**“能看到原两轮历史”**。这一项是用户验收证据，不声称 assistant 已独立逐条检查全部历史。没有向会话发送新消息。

Talkify 尚未实现父 Task 页面，因此此次新 Task 不会因为后台已存在就自动出现在现有 Session Timeline。没有把这个缺失当作事件丢失，也没有把 TaskEvent 注入 Conversation Reducer。

## 验证证据与尚未覆盖

- [live-readonly.json](/private/tmp/codeagent-b2c-desktop-smoke-20261028/live-readonly.json)：认证身份、capabilities、原 Session；不含令牌。
- [backup-summary.json](/private/tmp/codeagent-b2c-desktop-smoke-20261028/backup-summary.json)：备份方法、读回、身份与边界。
- [native-mcp-smoke.json](/private/tmp/codeagent-b2c-desktop-smoke-20261028/native-mcp-smoke.json)：项目安装路径 Bridge 的原生协议记录、Snapshot、分页、重启和计数。
- [desktop-native-tools.json](/private/tmp/codeagent-b2c-desktop-smoke-20261028/desktop-native-tools.json)：当前桌面对话实际 MCP 调用结果与用户 UI 确认。
- 本轮重跑 owner/ready/Bridge scope 定向 race，以及 Foundation Go/Swift B1 fixture；具体输出见同目录报告。

live 验证的是已经 ready 的实例。启动前和恢复失败时不发布能力，依据是源码装配顺序和重跑的负面契约测试，本轮没有再次重启或破坏用户实例去验证时序。没有测试 shared-device 真机、桌面 managed Run、真实网络中断、父 Task UI 或 iOS UI，不声称 Collaborative Mode 已全部验收。

## 回滚依据

Bridge 可恢复本次保存的旧二进制；先确保 Bridge 进程退出，原子替换，再重载项目 MCP。授权与配置保持原范围，无需转成全局访问。

Daemon 本轮未被替换，不需要回退用户已安装版本。数据库没有本批迁移，冒烟仅新增可追踪的 Task 元数据，没有删除既有 Session。Task 没有已实现的安全删除/归档 API，保留这条记录，不直接清表。

**不建议为删除冒烟元数据恢复整库**：用户其他会话在此期间仍可能正常执行，恢复会丢失之后的真实工作。若确实需要灾难恢复，先停止所有 writer、保留故障后的两库和 main/WAL，再成套恢复对应快照并人工核对旧 inbox；不在在线实例上覆盖，也不盲目重发副作用。

完整下一步实施评估见 [P0.5-A 只读 Task Timeline](p05a-readonly-task-timeline-assessment.md)。本轮没有修改 Go/Swift 生产源码、提交、推送或安装依赖。
